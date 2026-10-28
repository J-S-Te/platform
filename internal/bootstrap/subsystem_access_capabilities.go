package bootstrap

import (
	"context"
	"errors"
	"fmt"
	applicationregistryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	applicationaccess "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	"log/slog"
	"os"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	keycloakauthorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
)

// resolveSubsystemProvisioningCapabilities 解析可接入能力清单：
// 生产来自随发布包审核的 profiles；本地为固定的开发能力集合。API 与 Worker 共用，避免两处口径漂移。
func resolveSubsystemProvisioningCapabilities(cfg config.Config) (application.SubsystemProvisioningCapabilities, error) {
	capabilities := application.SubsystemProvisioningCapabilities{
		Enabled: cfg.SubsystemOnboarding.Enabled,
		Mode:    cfg.SubsystemOnboarding.Mode,
	}
	if strings.EqualFold(strings.TrimSpace(cfg.SubsystemOnboarding.Mode), "production") {
		if cfg.SubsystemOnboarding.Enabled {
			loaded, err := infrastructure.LoadProductionSubsystemCapabilities(
				cfg.SubsystemOnboarding.ProductionDeployRoot,
				cfg.SubsystemOnboarding.ProductionProfilesDirectory,
			)
			if err != nil {
				return application.SubsystemProvisioningCapabilities{}, fmt.Errorf("load production subsystem capabilities: %w", err)
			}
			loaded.Enabled = true
			return loaded, nil
		}
		return capabilities, nil
	}
	capabilities.SupportedEnvironments = []string{"dev", "test", "staging", "prod"}
	capabilities.DefaultEnvironment = "dev"
	capabilities.DefaultClientType = "confidential"
	return capabilities, nil
}

// newSubsystemAccessProvisioner 构造隔离部署 Agent 客户端（清单与运行批准事实的唯一来源）。
func newSubsystemAccessProvisioner(cfg config.Config) (*infrastructure.UnixSocketSubsystemProvisioner, error) {
	capabilities, err := resolveSubsystemProvisioningCapabilities(cfg)
	if err != nil {
		return nil, err
	}
	provisioner, err := infrastructure.NewUnixSocketSubsystemProvisioner(
		cfg.SubsystemOnboarding.Enabled,
		cfg.SubsystemOnboarding.SocketPath,
		cfg.SubsystemOnboarding.Timeout,
		capabilities,
	)
	if err != nil {
		return nil, fmt.Errorf("create subsystem provisioner: %w", err)
	}
	releaseIntegrityRequired := strings.EqualFold(strings.TrimSpace(os.Getenv("SUBSYSTEM_RELEASE_INTEGRITY_REQUIRED")), "true")
	expectedReleaseDigest := strings.TrimSpace(os.Getenv("SUBSYSTEM_RELEASE_EXPECTED_DIGEST"))
	if releaseIntegrityRequired || expectedReleaseDigest != "" {
		if err := provisioner.ConfigureReleaseIntegrity(releaseIntegrityRequired, expectedReleaseDigest); err != nil {
			return nil, err
		}
		handshakeContext, cancel := context.WithTimeout(context.Background(), cfg.SubsystemOnboarding.Timeout)
		defer cancel()
		capabilities, err := provisioner.AvailableCapabilities(handshakeContext)
		if err != nil {
			return nil, fmt.Errorf("release integrity capability handshake: %w", err)
		}
		if capabilities.ReleaseIntegrity != "release-integrity-v1" {
			return nil, fmt.Errorf("deployment agent does not advertise release-integrity-v1")
		}
	}
	return provisioner, nil
}

// subsystemAccessCutoverGateStore 是既有 Keycloak 切换生命周期仓储的窄接口。
type subsystemAccessCutoverGateStore interface {
	CanKeycloakCutover(ctx context.Context, tenantID, applicationCode, environment string) error
}

// subsystemAccessObservationGate 实现更新路径的观察期门禁：只读查询既有生命周期状态。
// 只有观察期相关原因才阻塞（等待）；其它原因不阻塞，与既有受控更新只在切换时校验的行为一致。
type subsystemAccessObservationGate struct {
	store subsystemAccessCutoverGateStore
}

// CanProceedWithUpdate 判断受控更新是否可以继续：仅当观察期已开始且尚未结束时
// 阻塞（返回 false 及原因码）；其余情况一律放行，原因码仍回传供记录。
func (gate subsystemAccessObservationGate) CanProceedWithUpdate(ctx context.Context, tenantID, applicationCode, environmentCode string) (bool, string, error) {
	if gate.store == nil {
		return true, "", nil
	}
	err := gate.store.CanKeycloakCutover(ctx, tenantID, applicationCode, environmentCode)
	if err == nil {
		return true, "", nil
	}
	code := subsystemAccessObservationReasonCode(err)
	// 只有「观察期已开始但尚未结束」才阻塞：
	//   · 没有任何观察记录（NOT_STARTED）表示该环境从未做过 Keycloak 切换，不应阻塞普通更新；
	//   · 首次切换会由既有流程创建观察记录，不属于阻塞条件；
	//   · 其它原因（配置错误/状态不可用）也不阻塞，原因码仍回传以供记录。
	if code == "OBSERVATION_WINDOW_NOT_COMPLETED" {
		return false, code, nil
	}
	return true, code, nil
}

// subsystemAccessObservationReasonCode 与 HTTP 层的映射保持一致（三处固定消息）。
func subsystemAccessObservationReasonCode(err error) string {
	if err == nil {
		return "OBSERVATION_STATE_UNKNOWN"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "window has not completed"):
		return "OBSERVATION_WINDOW_NOT_COMPLETED"
	case strings.Contains(message, "has not started"):
		return "OBSERVATION_NOT_STARTED"
	case strings.Contains(message, "already exists"):
		return "OBSERVATION_ALREADY_EXISTS"
	default:
		return "OBSERVATION_STATE_UNAVAILABLE"
	}
}

// subsystemAccessIssuerPort 把应用层 issuer 解析器适配为部署端口要求的签名。
type subsystemAccessIssuerPort struct {
	resolver *application.SubsystemIssuerResolver
}

// ResolveSubsystemAccessIssuer 按认证提供方解析子系统应使用的 issuer；解析器未
// 配置时返回错误（fail-closed）。
func (port subsystemAccessIssuerPort) ResolveSubsystemAccessIssuer(_ context.Context, authProvider string) (string, error) {
	if port.resolver == nil {
		return "", errors.New("subsystem access issuer resolver is not configured")
	}
	return port.resolver.Resolve(authProvider)
}

// subsystemAccessRelatedClientsPort 把应用层相关客户端解析器适配为部署端口要求的签名。
type subsystemAccessRelatedClientsPort struct {
	resolver *application.RelatedServiceClientsResolver
}

// ResolveSubsystemRelatedServiceClients 解析该应用环境部署时需要一并准备凭据的
// 关联服务客户端；解析器未配置时返回错误（fail-closed）。
func (port subsystemAccessRelatedClientsPort) ResolveSubsystemRelatedServiceClients(ctx context.Context, tenantID, applicationCode, environmentCode string) ([]application.RelatedSubsystemServiceClient, error) {
	if port.resolver == nil {
		return nil, errors.New("subsystem access related clients resolver is not configured")
	}
	return port.resolver.Resolve(ctx, tenantID, applicationCode, environmentCode)
}

// newSubsystemAccessRunnerFromConfig 装配统一接入的持久执行器：
// 清单/批准（部署 Agent）→ 目录与部署端口 → 阶段执行器 → 持久执行器 → 调度循环。
// 仅当 cfg.SubsystemOnboarding.Enabled 为真时由 Worker 调用；任一步失败即整体失败（不做静默降级）。
func newSubsystemAccessRunnerFromConfig(cfg config.Config, database *gorm.DB, logger *slog.Logger, broker infrastructure.SubsystemAccessKeycloakBrokerReconciliation, keycloakCatalog *applicationaccess.Service) (*subsystemAccessRunner, error) {
	if database == nil || logger == nil {
		return nil, errors.New("subsystem access runner requires database and logger")
	}
	provisioner, err := newSubsystemAccessProvisioner(cfg)
	if err != nil {
		return nil, err
	}
	onboardingRepository, err := infrastructure.NewSubsystemOnboardingGORMRepository(database)
	if err != nil {
		return nil, err
	}
	onboardingService, err := application.NewSubsystemOnboardingService(
		onboardingRepository, ulid.Generator{}, application.SystemClock{},
		application.RedirectURIValidationPolicy{AllowInsecureHTTP: cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs},
	)
	if err != nil {
		return nil, err
	}
	oauthRepository, err := infrastructure.NewOAuthClientManagementRepository(database)
	if err != nil {
		return nil, err
	}
	oauthService, err := application.NewOAuthClientManagementService(
		oauthRepository, ulid.Generator{}, application.SystemClock{},
		application.RedirectURIValidationPolicy{AllowInsecureHTTP: cfg.Auth.OAuthClientAllowInsecureHTTPRedirectURIs},
	)
	if err != nil {
		return nil, err
	}
	registry, err := infrastructure.NewSubsystemAccessRegistryGORMReader(database)
	if err != nil {
		return nil, err
	}
	stateReader, err := infrastructure.NewSubsystemAccessDeploymentStateGORMReader(database)
	if err != nil {
		return nil, err
	}
	operations, err := infrastructure.NewSubsystemAccessOperationGORMRepository(database)
	if err != nil {
		return nil, err
	}
	// 观察期门禁复用既有 Keycloak 切换生命周期仓储（只读）。
	cutoverStore, err := keycloakauthorizationinfrastructure.NewCutoverLifecycleStore(database)
	if err != nil {
		return nil, err
	}
	relatedResolver, err := application.NewRelatedServiceClientsResolver(onboardingService, oauthService)
	if err != nil {
		return nil, err
	}
	// issuer 解析与 API 进程同一来源：平台 issuer + Keycloak realm issuer；空 issuer 直接失败（不部署）。
	issuerResolver, err := application.NewSubsystemIssuerResolver(
		"", cfg.Auth.OIDCIssuer, cfg.Keycloak.Enabled,
		keycloakRealmIssuer(cfg.Keycloak.PublicURL, cfg.Keycloak.Realm), cfg.Keycloak.Realm,
	)
	if err != nil {
		return nil, err
	}
	onboardingPort, err := infrastructure.NewSubsystemAccessOnboardingAdapter(onboardingService, provisioner, registry)
	if err != nil {
		return nil, err
	}
	// 更新路径的凭据准备（两份，均由既有 OAuth 客户端服务实现）。
	updateServiceCredentials, err := application.NewSubsystemUpdateServiceCredentialsService(oauthService)
	if err != nil {
		return nil, err
	}
	updateBrowserCredentials, err := application.NewSubsystemUpdateBrowserCredentialsService(oauthService)
	if err != nil {
		return nil, err
	}
	updateCatalogPublisherCredentials, err := application.NewSubsystemUpdateCatalogPublisherCredentialsService(oauthService)
	if err != nil {
		return nil, err
	}
	deploymentPort, err := infrastructure.NewSubsystemAccessDeploymentAdapter(
		onboardingService, provisioner, provisioner,
		subsystemAccessRelatedClientsPort{resolver: relatedResolver},
		stateReader,
		subsystemAccessIssuerPort{resolver: issuerResolver},
		subsystemAccessObservationGate{store: cutoverStore},
		updateServiceCredentials,
		updateBrowserCredentials,
		updateCatalogPublisherCredentials,
	)
	if err != nil {
		return nil, err
	}
	configurationTracker, err := infrastructure.NewSubsystemAccessConfigurationTracker(database, provisioner, provisioner)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(cfg.SubsystemOnboarding.Mode, "production") {
		configurationTracker.SetBuiltinRuntimeSource(infrastructure.BuiltinRuntimeComponentCatalog{}, provisioner)
	}
	configurationTracker.SetRuntimeRegistrationCheck(provisioner)
	deploymentPort.SetConfigurationEvidence(configurationTracker)
	if err := configureSubsystemAccessRuntimeEnrollment(cfg, database, oauthService, provisioner, deploymentPort); err != nil {
		return nil, err
	}
	executor, err := application.NewSubsystemAccessStageExecutor(onboardingPort, deploymentPort)
	if err != nil {
		return nil, err
	}
	// 租约/阶段上限/尝试上限使用执行器默认值（2 分钟 / 全阶段 / 3 次）。
	accessWorker, err := application.NewSubsystemAccessWorker(
		operations, executor, application.SystemClock{},
		strings.TrimSpace(cfg.Worker.ID)+"-subsystem-access", 0, 0, 0,
	)
	if err != nil {
		return nil, err
	}
	// 运行环境标记与状态上下文读取器（InitialRuntimeProvisioning 派生所需）。
	runtimeCapabilities, capabilitiesErr := resolveSubsystemProvisioningCapabilities(cfg)
	if capabilitiesErr != nil {
		return nil, capabilitiesErr
	}
	deploymentPort.SetInitialRuntimeProvisioningContext(strings.EqualFold(strings.TrimSpace(runtimeCapabilities.Mode), "production"), stateReader)
	// 期望清单落库复用既有 onboarding 仓储（与 handler 的 setDesiredManifest 同一实现）。
	deploymentPort.SetManifestStateStore(onboardingRepository)
	// 完成收口：初始管理员授权落库 + 已应用清单核对，均复用既有实现。
	deploymentPort.SetInitialAdminMarker(onboardingRepository)
	deploymentPort.SetAppliedManifestReader(subsystemAccessAppliedManifestPort{reader: stateReader})
	browserRuntime := &subsystemAccessBrowserCredentialPort{provider: onboardingRepository, platform: updateBrowserCredentials}
	deploymentPort.SetProviderBrowserCredentials(browserRuntime)
	deploymentPort.SetIdentityReaders(registry, browserRuntime)
	// 认证投影：仅 Keycloak 启用且拿到同一份 applicationaccess.Service 时接线；
	// 否则结果阶段会显式标注 projection-skipped（不静默假装完成）。
	if cfg.Keycloak.Enabled && keycloakCatalog != nil {
		mappingStore, mappingErr := keycloakauthorizationinfrastructure.NewClientMappingStore(database)
		if mappingErr != nil {
			return nil, mappingErr
		}
		readinessStore, readinessErr := keycloakauthorizationinfrastructure.NewSwitchReadinessStore(database)
		if readinessErr != nil {
			return nil, readinessErr
		}
		controlPlane := applicationregistryhttp.NewKeycloakControlPlaneWithCredentials(
			cfg.Keycloak.AdminURL, cfg.Keycloak.Realm, applicationregistryhttp.KeycloakControlPlaneCredentials{
				ServiceAccountClientID:     cfg.Keycloak.AdminClientID,
				ServiceAccountClientSecret: cfg.Keycloak.AdminClientSecret,
				Username:                   cfg.Keycloak.AdminUsername,
				Password:                   cfg.Keycloak.AdminPassword,
			},
			cfg.Keycloak.BrokerClientID, cfg.Keycloak.BrokerClientSecret, cfg.Auth.OIDCIssuer, cfg.Keycloak.PlatformBackchannelURL,
		)
		browserRuntime.mappings, browserRuntime.keycloak = mappingStore, controlPlane
		projection, projectionErr := application.NewKeycloakAuthorizationProjectionService(
			keycloakAuthorizationCatalogAdapter{service: keycloakCatalog}, controlPlane, mappingStore, readinessStore,
		)
		if projectionErr != nil {
			return nil, projectionErr
		}
		deploymentPort.SetKeycloakProjection(projection, cfg.Keycloak.Realm)
	}
	// broker 协调：复用 worker 已构造的 registrar 与控制面（未启用 Keycloak 时为 nil）。
	if broker.Credentials != nil && broker.Control != nil {
		deploymentPort.SetKeycloakBrokerReconciliation(broker)
	}
	return newSubsystemAccessRunner(accessWorker, logger, cfg.Worker.PollInterval, 20)
}

// subsystemAccessAppliedManifestPort 用部署状态读取器实现"已应用清单"端口。
// 未找到状态行时按"未记录"返回（verified=true, checksum 为空），由就绪阶段继续等待。
type subsystemAccessAppliedManifestPort struct {
	reader *infrastructure.SubsystemAccessDeploymentStateGORMReader
}

// ReadSubsystemAccessAppliedManifest 读取已应用清单的校验和；无状态记录时按
// "未记录"返回（found=true、checksum 为空），由就绪阶段继续等待。
func (port subsystemAccessAppliedManifestPort) ReadSubsystemAccessAppliedManifest(ctx context.Context, tenantID, applicationCode, environmentCode string) (string, bool, error) {
	if port.reader == nil {
		return "", false, nil
	}
	state, err := port.reader.ReadSubsystemAccessDeploymentContext(ctx, tenantID, applicationCode, environmentCode)
	if err != nil {
		return "", false, err
	}
	if !state.Found {
		return "", true, nil
	}
	return state.AppliedManifestChecksum, true, nil
}
