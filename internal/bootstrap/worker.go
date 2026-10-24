// Package bootstrap 负责把基础设施依赖装配成可独立启动的进程。
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

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

// ProcessRunner 由组成 worker 进程的每个后台循环实现。
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

// Worker 是本地异步任务处理的依赖容器。
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
	// 传给统一接入执行器的 broker 端口（未启用 Keycloak 时保持 nil）。
	var brokerCredentials applicationregistryinfrastructure.SubsystemAccessKeycloakBrokerCredentials
	var brokerControl applicationregistryinfrastructure.SubsystemAccessKeycloakBrokerControl
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
	identityRepository, err := identityinfrastructure.NewGORMRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("create identity repository for expiry worker: %w", err)
	}
	expiryRunner, err := identityworker.NewExpiryWorker(
		identityinfrastructure.NewExpiryGORMRepository(db), identityRepository, service, notificationService,
		logger, cfg.Audit.ApplicationCode, cfg.Audit.EnvironmentCode, cfg.Worker.PollInterval,
	)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("create identity expiry worker: %w", err)
	}
	runners = append(runners, expiryRunner)
	// 计划人事异动与到期任务共用同一 worker 进程与数据库租约语义，
	// 不需要额外的服务或消息代理。
	personnelRepo := identityinfrastructure.NewPersonnelChangeGORMRepository(db)
	personnelService, err := buildPersonnelWorkerService(cfg, db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("create personnel change service: %w", err)
	}
	// 到期异动由 Worker 真正执行，因此完成通知也必须注入 Worker 使用的服务实例；
	// 只给 HTTP 组合根注入会导致自动执行成功却没有任何人员异动通知。
	personnelService.SetNotifier(notificationService)
	// 超管例外审批的本地审计 outbox 由同一 worker 进程补投。审计端装配与 HTTP 组合根
	// 保持一致：只做字段翻译，不改变审批判定；投递失败保留 outbox 并退避重试。
	personnelAudit := &personnelChangeAuditAdapter{service: service, config: cfg.Audit, ids: ulid.Generator{}}
	personnelService.SetAuditSink(personnelAudit)
	approvalExceptionDispatcher, err := identityapplication.NewApprovalExceptionDispatcher(personnelRepo, personnelAudit, identityapplication.SystemClock{})
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("create personnel approval exception dispatcher: %w", err)
	}
	personnelRunner, err := identityworker.NewPersonnelChangeWorker(personnelService, logger, cfg.Worker.ID+"-personnel", cfg.Worker.PollInterval)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, err
	}
	personnelRunner.SetApprovalExceptionDispatcher(approvalExceptionDispatcher)
	runners = append(runners, personnelRunner)
	// 统一接入的角色目录投影复用本进程内同一个 applicationaccess.Service，
	// 不在装配链里另建第二份实例（否则审计 sink 与缓存会分裂）。
	var keycloakAccessCatalog *applicationaccess.Service
	if cfg.Keycloak.Enabled {
		// 每次部署/重启都对账 Keycloak。发布 Broker 策略或 profile 变更后，
		// 运维不应该还需要手动点一次"同步"。
		brokerClientID := strings.TrimSpace(cfg.Keycloak.BrokerClientID)
		brokerClientSecret := strings.TrimSpace(cfg.Keycloak.BrokerClientSecret)
		customerPortalBrokerClientID := strings.TrimSpace(cfg.Keycloak.CustomerPortalBrokerClientID)
		customerPortalBrokerClientSecret := strings.TrimSpace(cfg.Keycloak.CustomerPortalBrokerClientSecret)
		var brokerRegistrar keycloakBrokerRegistrar
		var brokerTenantID string
		// 周期门户 Broker 对账器在凭据已全部配好时同样需要 registrar，
		// 因此其仓储每次都要初始化。
		{
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
			brokerCredentials = brokerRegistrar
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
			// 客户门户是按需接入的可选子系统。尚未注册 customer_portal 应用/环境时，
			// 只跳过该 Broker 的补齐并记录警告；绝不能因此让 Worker 退出，否则同一容器
			// 内的 API 会被 entrypoint 一并 SIGTERM，形成 api 容器反复重启。
			if customerPortalBrokerClientID == "" || customerPortalBrokerClientSecret == "" {
				customerPortalBrokerClientID, customerPortalBrokerClientSecret, tenantErr = brokerRegistrar.EnsureCustomerPortalBroker(context.Background(), tenantID)
				if tenantErr != nil {
					if !errors.Is(tenantErr, errBrokerTargetNotRegistered) {
						_ = database.Close(db)
						_ = logFile.Close()
						return nil, fmt.Errorf("restore customer portal Broker credentials: %w", tenantErr)
					}
					logger.Warn("customer portal is not onboarded yet; skipping customer portal Broker provisioning", "error", tenantErr)
					customerPortalBrokerClientID, customerPortalBrokerClientSecret = "", ""
				} else {
					logger.Info("restored customer portal Broker credentials from platform OAuth client", "client_id", customerPortalBrokerClientID)
				}
			}
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
		brokerControl = controlPlane
		// broker client 已有活动凭据时，ensureBrokerClient 返回空 secret 以避免
		// 不必要的轮换。此时只验证 IdP 存在且配置完整；若拿到了 secret
		// （新 client 或强制轮换），才执行完整对账。
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
		keycloakAccessCatalog = accessService
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
				// 已退役或被删除的应用不再有授权资源：跳过该映射并留下告警，
				// 不能因为一条历史映射就让整个 Worker（及同进程 API）退出。
				if errors.Is(err, applicationaccess.ErrNotFound) {
					slog.Warn("skip stored Keycloak Client reconcile for inactive application",
						"application_id", mapping.ApplicationID, "environment_id", mapping.EnvironmentID, "client_id", mapping.ClientID)
					continue
				}
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
		eligibilityReconciler, err := keycloakauthorizationinfrastructure.NewAccountEligibilityReconciler(db)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		keycloakRunner.SetEligibilityReconciler(eligibilityReconciler)
		runners = append(runners, keycloakRunner)
		// 平台审计轨迹不需要 Keycloak 安装自定义监听插件：worker 以小幅幂等重叠
		// 轮询其标准用户/管理员事件端点，并经 external_subject_id 映射主体。
		// 没有验证通过的平台映射的事件被刻意跳过。
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
		// 批次 3：采集进度按 realm 持久化，停机后可续采。
		cursorStore := keycloakauthorizationinfrastructure.NewEventCursorRepository(db)
		eventAuditRunner, err := newKeycloakEventAuditRunner(eventCollector, cursorStore, cfg.Keycloak.Realm, logger, cfg.Worker.PollInterval)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, err
		}
		runners = append(runners, eventAuditRunner)
		// 客户门户可能在 Worker 启动后才接入；周期 reconcile 会补齐其 Broker，
		// 同时继续执行平台 Broker 的健康检查。
		portalBrokerReconciler := customerPortalBrokerReconciler{
			registrar: brokerRegistrar, control: controlPlane, tenantID: brokerTenantID,
		}
		brokerHealthRunner, err := newKeycloakBrokerHealthRunner(controlPlane, db, logger, cfg.Keycloak.BrokerHealthPollInterval, portalBrokerReconciler)
		if err != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("create Keycloak broker health runner: %w", err)
		}
		runners = append(runners, brokerHealthRunner)
	}
	// 子系统部署状态看护：把进程死亡遗留的“更新中/接入中”状态定时收口为失败，
	// 让操作者始终可以重试（P1：不再依赖有人打开管理页触发恢复）。
	deploymentSweepStore, err := applicationregistryinfrastructure.NewSubsystemOnboardingGORMRepository(db)
	if err != nil {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("create subsystem deployment sweep store: %w", err)
	}
	if sweeper, err := newSubsystemDeploymentSweeper(deploymentSweepStore, logger, time.Minute, applicationregistryapplication.SubsystemDeploymentStaleAfter); err == nil {
		runners = append(runners, sweeper)
	} else {
		_ = database.Close(db)
		_ = logFile.Close()
		return nil, fmt.Errorf("create subsystem deployment sweeper: %w", err)
	}
	// 统一接入的持久执行器：领取/续租/逐阶段推进（首次接入路径），随 Worker 进程启停。
	// 仅在子系统接入启用时装配；任一步失败即进程启动失败（不做静默降级）。
	if cfg.SubsystemOnboarding.Enabled {
		accessRunner, accessErr := newSubsystemAccessRunnerFromConfig(cfg, db, logger, applicationregistryinfrastructure.SubsystemAccessKeycloakBrokerReconciliation{
			Credentials: brokerCredentials, Control: brokerControl,
			IsRecoverable: applicationregistryhttp.IsRecoverableKeycloakBrokerConfigurationError,
		}, keycloakAccessCatalog)
		if accessErr != nil {
			_ = database.Close(db)
			_ = logFile.Close()
			return nil, fmt.Errorf("create subsystem access runner: %w", accessErr)
		}
		runners = append(runners, accessRunner)
	}
	runner := &concurrentRunner{runners: runners}
	return &Worker{Runner: runner, Logger: logger, database: db, logFile: logFile}, nil
}

// Close 释放 worker 进程占用的资源。
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
