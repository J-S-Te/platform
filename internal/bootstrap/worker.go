// Package bootstrap 负责把基础设施依赖装配成可独立启动的进程。
package bootstrap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/audit/infrastructure"
	auditworker "github.com/J-S-Te/Basic-Platform/internal/platform/audit/worker"
	applicationaccess "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identityinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	identityworker "github.com/J-S-Te/Basic-Platform/internal/platform/identity/worker"
	keycloakauthorizationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	keycloakauthorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
	keycloakauthorizationworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
	notificationapplication "github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	notificationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/notification/infrastructure"
	notificationworker "github.com/J-S-Te/Basic-Platform/internal/platform/notification/worker"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/internal/shared/observability"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
)

// ProcessRunner is implemented by each background loop composed into the worker process.
type ProcessRunner interface {
	Run(context.Context)
}

// concurrentRunner 让互不依赖的后台循环共享同一取消信号；关闭时等待全部循环完成清理再退出进程。
type concurrentRunner struct {
	runners []ProcessRunner
}

// Run 并发启动所有 Runner 并等待全部退出。
// 并发边界：每个 Runner 各自独立 goroutine 运行；context 被所有子 goroutine 共用；
// 如果 context 被取消，子 Runner 退场后 WaitGroup 才会释放并让进程安全退出。
func (runner *concurrentRunner) Run(ctx context.Context) {
	var group sync.WaitGroup
	for _, process := range runner.runners {
		if process == nil {
			continue
		}
		group.Add(1)
		go func(current ProcessRunner) {
			defer group.Done()
			current.Run(ctx)
		}(process)
	}
	group.Wait()
}

// Worker is the dependency container for local asynchronous job processing.
type Worker struct {
	Runner ProcessRunner
	Logger *slog.Logger

	database *gorm.DB
	logFile  io.Closer
}

// NewWorker 装配审计导出、留存清理、人员变更与 Keycloak 观察者等任务。
// 参数 cfg 提供数据库与调度配置；返回 *Worker 表示可启动的完整 worker 进程依赖图。
// Worker 不执行 AutoMigrate，数据库变更只能由独立 migrate 命令完成，避免扩容或重启后台任务时并发竞争 DDL。
func NewWorker(cfg config.Config) (*Worker, error) {
	if err := os.MkdirAll(cfg.FileStorageRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create file storage root: %w", err)
	}

	logger, logFile, err := observability.NewLogger(cfg.Logging, cfg.AppName, cfg.Environment)
	if err != nil {
		return nil, err
	}

	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}

	repository, err := auditinfrastructure.NewRepository(db)
	if err != nil {
		// 装配任一步失败都按创建顺序逆向释放资源，不能留下占用日志文件或连接池的半成品进程。
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	service, err := auditapplication.NewService(repository, ulid.Generator{}, auditapplication.SystemClock{})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	exportRunner, err := auditworker.NewExportWorker(service, logger, cfg.FileStorageRoot, cfg.Worker.ID, cfg.Worker.PollInterval, cfg.Worker.StaleLockTimeout)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	archiveWriter, err := auditworker.NewArchiveWriter(cfg.FileStorageRoot)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	retentionService, err := auditapplication.NewRetentionService(
		repository,
		archiveWriter,
		ulid.Generator{},
		auditapplication.SystemClock{},
		&governanceAuditAdapter{service: service, config: cfg.Audit},
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	retentionRunner, err := auditworker.NewRetentionWorker(retentionService, logger, cfg.Worker.ID+"-retention", cfg.Worker.PollInterval, cfg.Worker.StaleLockTimeout)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}

	// 两类任务共享数据库但使用独立租约；其中一个退出不应静默停止另一个，统一由上层 context 收敛。
	runners := []ProcessRunner{exportRunner, retentionRunner}
	notificationRepository, err := notificationinfrastructure.NewRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	notificationPolicy, err := notificationinfrastructure.NewInboxPolicy(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	notificationResolver, err := notificationinfrastructure.NewRecipientResolver(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	notificationService, err := notificationapplication.NewService(notificationRepository, notificationPolicy, notificationResolver, ulid.Generator{}, notificationapplication.SystemClock{})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	notificationRunner, err := notificationworker.NewIngestionWorker(notificationService, logger, cfg.Worker.PollInterval, cfg.Worker.StaleLockTimeout)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	runners = append(runners, notificationRunner)
	// Scheduled personnel changes share the same worker process and database lease
	// semantics; no extra service or broker is required.
	personnelRepo := identityinfrastructure.NewPersonnelChangeGORMRepository(db)
	personnelService, err := identityapplication.NewPersonnelChangeService(personnelRepo, ulid.Generator{}, identityapplication.SystemClock{})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("create personnel change service: %w", err)
	}
	personnelRunner, err := identityworker.NewPersonnelChangeWorker(personnelService, logger, cfg.Worker.ID+"-personnel", cfg.Worker.PollInterval)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	runners = append(runners, personnelRunner)
	if cfg.Keycloak.Enabled {
		// Reconcile Keycloak on every deployment/restart. Operators must not need
		// to click "sync" after shipping a Broker policy or profile change.
		brokerClientID := strings.TrimSpace(cfg.Keycloak.BrokerClientID)
		brokerClientSecret := strings.TrimSpace(cfg.Keycloak.BrokerClientSecret)
		customerPortalBrokerClientID := strings.TrimSpace(cfg.Keycloak.CustomerPortalBrokerClientID)
		customerPortalBrokerClientSecret := strings.TrimSpace(cfg.Keycloak.CustomerPortalBrokerClientSecret)
		var brokerRegistrar keycloakBrokerRegistrar
		var brokerTenantID string
		if brokerClientID == "" || brokerClientSecret == "" || customerPortalBrokerClientID == "" || customerPortalBrokerClientSecret == "" {
			managementRepository, managementErr := applicationregistryinfrastructure.NewManagementRepository(db)
			if managementErr != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("create Keycloak Broker application repository: %w", managementErr)
			}
			managementService, managementErr := applicationregistryapplication.NewManagementService(
				managementRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{},
			)
			if managementErr != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("create Keycloak Broker application service: %w", managementErr)
			}
			oauthRepository, oauthErr := applicationregistryinfrastructure.NewOAuthClientManagementRepository(db)
			if oauthErr != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("create Keycloak Broker OAuth repository: %w", oauthErr)
			}
			oauthService, oauthErr := applicationregistryapplication.NewOAuthClientManagementService(
				oauthRepository, ulid.Generator{}, applicationregistryapplication.SystemClock{},
				applicationregistryapplication.RedirectURIValidationPolicy{AllowInsecureHTTP: cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs},
			)
			if oauthErr != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("create Keycloak Broker OAuth service: %w", oauthErr)
			}
			tenantID, tenantErr := resolveDefaultTenantID(db)
			if tenantErr != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("resolve default tenant for Keycloak Broker: %w", tenantErr)
			}
			brokerRegistrar = keycloakBrokerRegistrar{
				applications: managementService,
				oauth:        oauthService,
				publicURL:    cfg.Keycloak.PublicURL,
				realm:        cfg.Keycloak.Realm,
				environment:  keycloakBrokerEnvironment(cfg.Environment),
			}
			brokerTenantID = tenantID
			if brokerClientID == "" || brokerClientSecret == "" {
				brokerClientID, brokerClientSecret, tenantErr = brokerRegistrar.EnsureKeycloakBroker(context.Background(), tenantID)
				if tenantErr != nil {
					_ = database.Close(db)
					_ = logFile.Close()
					return nil, fmt.Errorf("restore Keycloak Broker credentials: %w", tenantErr)
				}
				logger.Info("restored Keycloak Broker credentials from platform OAuth client", "client_id", brokerClientID)
			}
			customerPortalBrokerClientID, customerPortalBrokerClientSecret, tenantErr = brokerRegistrar.EnsureCustomerPortalBroker(context.Background(), tenantID)
			if tenantErr != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("restore customer portal Broker credentials: %w", tenantErr)
			}
			logger.Info("restored customer portal Broker credentials from platform OAuth client", "client_id", customerPortalBrokerClientID)
		}
		controlPlane := applicationregistryhttp.NewKeycloakControlPlaneWithCredentials(
			cfg.Keycloak.AdminURL, cfg.Keycloak.Realm,
			applicationregistryhttp.KeycloakControlPlaneCredentials{
				ServiceAccountClientID: cfg.Keycloak.AdminClientID, ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret,
				Username: cfg.Keycloak.AdminUsername, Password: cfg.Keycloak.AdminPassword,
			},
			brokerClientID, brokerClientSecret,
			cfg.Auth.OIDCIssuer, cfg.Keycloak.PlatformBackchannelURL,
		)
		// When the broker client already has active credentials, ensureBrokerClient
		// returns an empty secret to avoid unnecessary rotation. In that case we
		// only verify the IdP exists and has a complete config; if the secret is
		// available (new client or forced rotation), we do the full reconciliation.
		if brokerClientID != "" && brokerClientSecret != "" {
			if err := controlPlane.EnsureBroker(context.Background(), brokerClientID, brokerClientSecret); err != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("reconcile Keycloak Broker policy: %w", err)
			}
		} else if brokerClientID != "" {
			if err := controlPlane.VerifyBrokerExists(context.Background()); err != nil {
				if !applicationregistryhttp.IsRecoverableKeycloakBrokerConfigurationError(err) {
					_ = database.Close(db)
					_ = logFile.Close()
					return nil, fmt.Errorf("verify Keycloak Broker exists: %w", err)
				}
				recoveredClientID, recoveredClientSecret, recoveryErr := brokerRegistrar.RecoverKeycloakBroker(context.Background(), brokerTenantID, "system-keycloak")
				if recoveryErr != nil {
					_ = database.Close(db)
					_ = logFile.Close()
					return nil, fmt.Errorf("recover incomplete Keycloak Broker credential: %w", recoveryErr)
				}
				brokerClientID, brokerClientSecret = recoveredClientID, recoveredClientSecret
				if err := controlPlane.EnsureBroker(context.Background(), brokerClientID, brokerClientSecret); err != nil {
					_ = database.Close(db)
					_ = logFile.Close()
					return nil, fmt.Errorf("reconcile recovered Keycloak Broker policy: %w", err)
				}
				logger.Warn("recovered incomplete Keycloak Broker IdP configuration", "client_id", brokerClientID)
			} else {
				logger.Info("verified Keycloak Broker IdP exists with active credentials", "client_id", brokerClientID)
			}
		}
		if customerPortalBrokerClientID != "" && customerPortalBrokerClientSecret != "" {
			if err := controlPlane.EnsureCustomerPortalBroker(context.Background(), customerPortalBrokerClientID, customerPortalBrokerClientSecret); err != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("reconcile customer portal Broker policy: %w", err)
			}
		} else if customerPortalBrokerClientID != "" {
			if err := controlPlane.VerifyCustomerPortalBrokerExists(context.Background()); err != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("verify customer portal Broker exists: %w", err)
			}
			logger.Info("verified customer portal Broker IdP exists with active credentials", "client_id", customerPortalBrokerClientID)
		}
		mappingStore, err := keycloakauthorizationinfrastructure.NewClientMappingStore(db)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		if err := mappingStore.ExpandLegacyKeycloakAuthorizationOutbox(context.Background()); err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("expand legacy Keycloak authorization outbox: %w", err)
		}
		accessService, err := applicationaccess.NewService(db, ulid.Generator{}, applicationaccess.SystemClock{})
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("create Keycloak authorization access resolver: %w", err)
		}
		readinessStore, err := keycloakauthorizationinfrastructure.NewSwitchReadinessStore(db)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		mappings, err := mappingStore.ListStoredKeycloakClientMappings(context.Background())
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		reconcileDependencies := keycloakClientStartupReconcileDependencies{
			markPending: readinessStore.MarkKeycloakClientAndRoleCatalogPending,
			ensureClient: func(ctx context.Context, clientID, name, redirectURI string) (string, error) {
				client, ensureErr := controlPlane.EnsureClient(ctx, clientID, name, redirectURI)
				return client.ClientID, ensureErr
			},
			listRoleCodes: func(ctx context.Context, tenantID, applicationID string) ([]string, error) {
				catalog, catalogErr := accessService.GetCatalog(ctx, tenantID, applicationID)
				if catalogErr != nil {
					return nil, catalogErr
				}
				roleCodes := make([]string, 0, len(catalog.Roles))
				for _, role := range catalog.Roles {
					roleCodes = append(roleCodes, role.Code)
				}
				return roleCodes, nil
			},
			ensureRoles: controlPlane.EnsureClientRoles,
			saveMapping: mappingStore.SaveKeycloakClientMapping,
			backfill:    mappingStore.BackfillKeycloakAuthorization,
			markSynced:  readinessStore.MarkKeycloakClientAndRoleCatalogSynced,
		}
		for _, mapping := range mappings {
			if err := reconcileStoredKeycloakClient(context.Background(), mapping, cfg.Keycloak.RequireHTTPS, reconcileDependencies); err != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, err
			}
		}
		queue, err := keycloakauthorizationinfrastructure.NewOutboxQueue(db)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		source, err := keycloakauthorizationinfrastructure.NewProjectionSource(db, accessService)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		admin, err := keycloakauthorizationinfrastructure.NewKeycloakAdminWithCredentials(cfg.Keycloak.AdminURL, cfg.Keycloak.Realm, keycloakauthorizationinfrastructure.KeycloakAdminCredentials{
			ServiceAccountClientID:     cfg.Keycloak.AdminClientID,
			ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret,
			Username:                   cfg.Keycloak.AdminUsername,
			Password:                   cfg.Keycloak.AdminPassword,
		})
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		store, err := keycloakauthorizationinfrastructure.NewProjectionStore(db)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		synchronizer, err := keycloakauthorizationapplication.NewSynchronizer(source, admin, store)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		if notificationService != nil {
			failureNotifier, notifierErr := keycloakauthorizationinfrastructure.NewProjectionFailureNotifier(notificationService, logger)
			if notifierErr != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("create Keycloak projection failure notifier: %w", notifierErr)
			}
			synchronizer.SetFailureNotifier(failureNotifier)
		}
		keycloakRunner, err := keycloakauthorizationworker.New(queue, synchronizer, logger, cfg.Worker.ID+"-keycloak", cfg.Worker.PollInterval)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		keycloakRunner.StaleLockTimeout = cfg.Worker.StaleLockTimeout
		runners = append(runners, keycloakRunner)
		// Keycloak does not need a custom listener plugin for the platform audit
		// trail: the worker polls its standard user/admin event endpoints with a
		// small idempotent overlap and maps subjects through external_subject_id.
		// Events without a verified platform mapping are intentionally skipped.
		auditIdentityResolver, err := keycloakauthorizationinfrastructure.NewAuditIdentityResolver(db)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		eventCollector, err := keycloakauthorizationapplication.NewKeycloakEventAuditCollector(admin, auditIdentityResolver, service, cfg.Audit.ApplicationCode, cfg.Audit.EnvironmentCode)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		eventAuditRunner, err := newKeycloakEventAuditRunner(eventCollector, logger, cfg.Worker.PollInterval)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		runners = append(runners, eventAuditRunner)
		// Broker 配置漂移只在启动 reconcile 一次；运行中 IdP 配置缺失或平台侧 broker 客户端
		// 凭据失效不会主动被发现，直到用户登录失败。周期校验可提前暴露并记录。
		brokerHealthRunner, err := newKeycloakBrokerHealthRunner(controlPlane, db, logger, cfg.Keycloak.BrokerHealthPollInterval)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("create Keycloak broker health runner: %w", err)
		}
		runners = append(runners, brokerHealthRunner)
	}
	runner := &concurrentRunner{runners: runners}
	return &Worker{Runner: runner, Logger: logger, database: db, logFile: logFile}, nil
}

// Close releases resources held by the worker process.
func (worker *Worker) Close() {
	if worker == nil {
		return
	}
	if err := database.Close(worker.database); err != nil {
		worker.Logger.Error("close worker database", "error", err)
	}
	if worker.logFile != nil {
		if err := worker.logFile.Close(); err != nil {
			worker.Logger.Error("close worker log file", "error", err)
		}
	}
}
