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
		controlPlane := applicationregistryhttp.NewKeycloakControlPlaneWithCredentials(
			cfg.Keycloak.AdminURL, cfg.Keycloak.Realm,
			applicationregistryhttp.KeycloakControlPlaneCredentials{
				ServiceAccountClientID: cfg.Keycloak.AdminClientID, ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret,
				Username: cfg.Keycloak.AdminUsername, Password: cfg.Keycloak.AdminPassword,
			},
			cfg.Keycloak.BrokerClientID, cfg.Keycloak.BrokerClientSecret,
			cfg.Auth.OIDCIssuer, cfg.Keycloak.PlatformBackchannelURL,
		)
		// Broker credentials are intentionally short-lived and are generated by
		// the application onboarding flow. A deployment environment may not have
		// a copy of them; passing empty values here would overwrite a working
		// Keycloak provider with an unusable empty configuration.
		if strings.TrimSpace(cfg.Keycloak.BrokerClientID) != "" && strings.TrimSpace(cfg.Keycloak.BrokerClientSecret) != "" {
			if err := controlPlane.EnsureBroker(context.Background(), cfg.Keycloak.BrokerClientID, cfg.Keycloak.BrokerClientSecret); err != nil {
				_ = database.Close(db)
				_ = logFile.Close()
				return nil, fmt.Errorf("reconcile Keycloak Broker policy: %w", err)
			}
		} else {
			logger.Warn("skip Keycloak Broker reconciliation because broker credentials are not configured")
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
