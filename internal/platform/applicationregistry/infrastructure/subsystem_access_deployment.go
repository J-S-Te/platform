package infrastructure

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"strings"
	"time"
)

// SubsystemAccessOnboardingProvisioningService 是既有 onboarding 服务中首次接入所需的窄接口。
type SubsystemAccessOnboardingProvisioningService interface {
	OnboardSubsystem(ctx context.Context, input application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error)
}

// SubsystemAccessRelatedClientsSource 解析相关服务客户端（由既有集成规则实现，禁止复制逻辑）。
type SubsystemAccessRelatedClientsSource interface {
	ResolveSubsystemRelatedServiceClients(ctx context.Context, tenantID, applicationCode, environmentCode string) ([]application.RelatedSubsystemServiceClient, error)
}

// SubsystemAccessUpdateCredentialPreparer 准备更新所需的凭据（由应用层服务实现；缺失即表示未接线）。
type SubsystemAccessUpdateCredentialPreparer interface {
	Ensure(ctx context.Context, request application.SubsystemUpdateServiceCredentialRequest) ([]application.SubsystemServiceCredential, error)
}

// SubsystemAccessCatalogPublisherCredentialPreparer 恢复目录发布者凭据（按应用判定是否需要）。
type SubsystemAccessCatalogPublisherCredentialPreparer interface {
	Ensure(ctx context.Context, tenantID, operatorID, applicationID, environmentID, applicationCode, environment string) (application.SubsystemUpdateCatalogPublisherCredential, error)
}

// SubsystemAccessBrowserCredentialPreparer 重建浏览器客户端凭据（由应用层服务实现）。
type SubsystemAccessBrowserCredentialPreparer interface {
	Ensure(ctx context.Context, tenantID, operatorID, applicationID, environmentID, applicationCode, environment, redirectURI string) (application.SubsystemUpdateBrowserCredential, error)
}

// SubsystemAccessManifestStateStore 记录期望清单（漂移检测基准）；与既有实现同一应用层接口。
type SubsystemAccessManifestStateStore interface {
	SetSubsystemDesiredManifest(ctx context.Context, tenantID, applicationCode, environment, checksum string, at time.Time) error
}

// SubsystemAccessKeycloakBrokerCredentials 提供 Keycloak 身份提供方所需的平台侧凭据。
type SubsystemAccessKeycloakBrokerCredentials interface {
	EnsureKeycloakBroker(ctx context.Context, tenantID string) (brokerID string, brokerSecret string, err error)
	RecoverKeycloakBroker(ctx context.Context, tenantID, operatorID string) (brokerID string, brokerSecret string, err error)
}

// SubsystemAccessKeycloakBrokerControl 协调/校验 Keycloak 身份提供方（broker）。
type SubsystemAccessKeycloakBrokerControl interface {
	EnsureBroker(ctx context.Context, clientID, clientSecret string) error
	VerifyBrokerExists(ctx context.Context) error
}

// SubsystemAccessKeycloakBrokerReconciliation 是 broker 协调的注入式配置。
// IsRecoverable 由组合根提供（既有判定函数在 interfaces/http 包，不能反向导入）。
type SubsystemAccessKeycloakBrokerReconciliation struct {
	Credentials   SubsystemAccessKeycloakBrokerCredentials
	Control       SubsystemAccessKeycloakBrokerControl
	IsRecoverable func(error) bool
}

// SubsystemAccessDeploymentContextReader 读取 InitialRuntimeProvisioning 派生所需的受控部署状态上下文。
type SubsystemAccessDeploymentContextReader interface {
	ReadSubsystemAccessDeploymentContext(ctx context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessDeploymentContext, error)
}

// SubsystemAccessObservationGate 是更新路径的观察期门禁（只读）：
// 认证观察期未结束（或未开始/已存在）时，受控更新必须等待，而不是继续执行。
type SubsystemAccessObservationGate interface {
	CanProceedWithUpdate(ctx context.Context, tenantID, applicationCode, environmentCode string) (allowed bool, reason string, err error)
}

// SubsystemAccessIssuerSource 按认证提供方别名解析 issuer（由既有配置实现，统一入口不自行推断）。
type SubsystemAccessIssuerSource interface {
	ResolveSubsystemAccessIssuer(ctx context.Context, authProvider string) (string, error)
}

// SubsystemAccessDeploymentStateReader 读取受控部署状态（就绪核验用）。
type SubsystemAccessDeploymentStateReader interface {
	ReadSubsystemAccessDeploymentState(ctx context.Context, tenantID, applicationCode, environmentCode string) (status string, generation uint64, attemptCount uint32, found bool, err error)
}

// SubsystemAccessAppliedManifestReader 读取"实际已应用清单摘要"（可选端口）。
// verified=false 表示无法确认（例如 Agent 未上报），此时不得判定一致或缺失。
type SubsystemAccessAppliedManifestReader interface {
	ReadSubsystemAccessAppliedManifest(ctx context.Context, tenantID, applicationCode, environmentCode string) (checksum string, verified bool, err error)
}

// SubsystemAccessInitialAdminMarker 记录"初始管理员已授权完成"（旧完成链的一环）。
// 与既有部署状态存储同一实现；未注入时不阻断接入，但证据会显式标注 skipped。
type SubsystemAccessInitialAdminMarker interface {
	MarkSubsystemInitialAccessAssigned(ctx context.Context, tenantID, applicationCode, environment, userID string, at time.Time) error
}

// SubsystemAccessDeploymentAdapter 实现统一接入的认证与部署端口（首次接入路径）：
//   - AUTH：调用既有原子流程 OnboardSubsystem（目录 + OAuth Client + 服务凭据 + 部署状态一次创建），
//     凭据只写入运行状态（内存），证据里不含任何秘密；
//   - DEPLOY：用 round 21 的装配函数构造部署输入并调用部署 Agent 的 Provision；
//   - READINESS：读取受控部署状态，只有 READY 才算成功。
//
// 接管/更新/恢复（ADOPT/UPDATE/RETRY）复用受控更新端口及认证观察期门禁。
type SubsystemAccessBrowserReferenceReader interface {
	ReadSubsystemAccessBrowserClientID(context.Context, string, string, string, string, string) (string, error)
}

type SubsystemAccessConfigurationEvidence interface {
	Snapshot(context.Context, application.SubsystemAccessOperation) (string, error)
	Verify(context.Context, application.SubsystemAccessOperation) (bool, error)
}

type SubsystemAccessDeploymentAdapter struct {
	runtimeLicenses      SubsystemAccessRuntimeEnrollment
	runtimeSettings      func(context.Context) (application.RuntimeLicenseSettings, error)
	runtimeLocal         bool
	configuration        SubsystemAccessConfigurationEvidence
	registry             SubsystemAccessRegistrationLookup
	browserReferences    SubsystemAccessBrowserReferenceReader
	service              SubsystemAccessOnboardingProvisioningService
	provisioner          application.SubsystemProvisioner
	catalog              SubsystemAccessCatalogSource
	related              SubsystemAccessRelatedClientsSource
	state                SubsystemAccessDeploymentStateReader
	issuers              SubsystemAccessIssuerSource
	observation          SubsystemAccessObservationGate
	credentials          SubsystemAccessUpdateCredentialPreparer
	browser              SubsystemAccessBrowserCredentialPreparer
	publisher            SubsystemAccessCatalogPublisherCredentialPreparer
	productionRuntime    bool
	deploymentContext    SubsystemAccessDeploymentContextReader
	brokerReconciliation SubsystemAccessKeycloakBrokerReconciliation
	manifestState        SubsystemAccessManifestStateStore
	initialAdmin         SubsystemAccessInitialAdminMarker
	appliedManifest      SubsystemAccessAppliedManifestReader
	projection           *application.KeycloakAuthorizationProjectionService
	keycloakRealm        string
}

// NewSubsystemAccessDeploymentAdapter 构造适配器；依赖缺失即失败（不做静默降级）。
func NewSubsystemAccessDeploymentAdapter(service SubsystemAccessOnboardingProvisioningService, provisioner application.SubsystemProvisioner, catalog SubsystemAccessCatalogSource, related SubsystemAccessRelatedClientsSource, state SubsystemAccessDeploymentStateReader, issuers SubsystemAccessIssuerSource, observation SubsystemAccessObservationGate, credentials SubsystemAccessUpdateCredentialPreparer, browser SubsystemAccessBrowserCredentialPreparer, publisher SubsystemAccessCatalogPublisherCredentialPreparer) (*SubsystemAccessDeploymentAdapter, error) {
	if service == nil || provisioner == nil || catalog == nil || related == nil || state == nil || issuers == nil {
		return nil, errors.New("subsystem access deployment adapter requires service, provisioner, catalog, related clients, state reader and issuer source")
	}
	return &SubsystemAccessDeploymentAdapter{service: service, provisioner: provisioner, catalog: catalog, related: related, state: state, issuers: issuers, observation: observation, credentials: credentials, browser: browser, publisher: publisher}, nil
}

// AuthHandledByProvisioning 为假：首次接入的认证由本适配器的 ConfigureSubsystemAccessAuthentication 承担。
func (adapter *SubsystemAccessDeploymentAdapter) AuthHandledByProvisioning() bool { return false }

// ConfigureSubsystemAccessAuthentication 首次接入：调用既有原子流程创建目录与凭据，并把凭据放进运行状态。
func (adapter *SubsystemAccessDeploymentAdapter) ConfigureSubsystemAccessAuthentication(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	if operation.Action != application.SubsystemAccessActionOnboard {
		return adapter.configureUpdateAuthentication(ctx, operation, run)
	}
	target, ok, err := resolveSubsystemAccessTarget(ctx, adapter.catalog, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !ok {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "OUT_OF_SCOPE",
			Message: "目标不在当前清单的可接入范围内"}, nil
	}
	result, err := adapter.service.OnboardSubsystem(ctx, application.SubsystemOnboardingInput{
		TenantID: operation.TenantID, OperatorID: operation.OperatorID, InitialAdminUserID: operation.InitialAdminID,
		ApplicationCode: operation.ApplicationCode, ApplicationName: target.ApplicationName, Environment: operation.EnvironmentCode,
		PublicBaseURL: strings.TrimSpace(operation.PublicOrigin), UpstreamURL: target.UpstreamURL,
		PathPrefix: target.PathPrefix, ClientType: target.ClientType,
		IssuerAlias: optionalAccessString(operation.AuthProvider), AllowedServiceBindings: target.AllowedServiceBindings,
	})
	if err != nil {
		if errors.Is(err, application.ErrValidation) {
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
				ErrorCode: "ONBOARDING_REJECTED", Message: err.Error()}, nil
		}
		// 结果未知（可能已创建）：交给 Worker 保留租约、下一轮先核对再决定。
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("onboard subsystem: %w", err)
	}
	if run != nil {
		run.Credentials = &application.SubsystemProvisioningCredentials{
			ClientID:                     result.OAuthClient.ClientID,
			ClientSecret:                 result.PlaintextSecret,
			CatalogPublisherClientID:     result.CatalogPublisherOAuthClient.ClientID,
			CatalogPublisherClientSecret: result.CatalogPublisherPlaintextSecret,
			ServiceCredentials:           result.ServiceCredentials,
			RedirectURI:                  result.RedirectURI,
			PublicURL:                    result.PublicURL,
		}
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "onboarding:" + result.Application.ID + "/" + result.Environment.ID}, nil
}

// configureUpdateAuthentication 接管/更新/重试的认证阶段。
//
// 凭据准备由 DEPLOY 阶段的受控更新实现（UpdateSubsystemAccess）一次完成：
// 它已包含观察期门禁、服务凭据轮换、浏览器客户端按现有 Client 重新签发与更新装配。
// 这里再调一次会造成同一操作内**两次轮换**，第二次会作废第一次的密钥，
// 因此 AUTH 只做"登记前置校验 + 明确委托"，不重复准备凭据。
func (adapter *SubsystemAccessDeploymentAdapter) configureUpdateAuthentication(ctx context.Context, operation application.SubsystemAccessOperation, _ *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	if strings.TrimSpace(operation.ApplicationID) == "" || strings.TrimSpace(operation.EnvironmentID) == "" {
		return application.SubsystemAccessStageOutcome{
			Status: application.SubsystemAccessStatusFailed, ErrorCode: "REGISTRATION_MISSING",
			Message: "接管/更新需要已登记的应用与环境；请先完成登记或使用首次接入",
		}, nil
	}
	if adapter.credentials == nil || adapter.browser == nil {
		return application.SubsystemAccessStageOutcome{
			Status: application.SubsystemAccessStatusFailed, ErrorCode: "AUTH_PREREQUISITE_MISSING",
			Message: "受控更新的凭据服务未配置，无法继续",
		}, nil
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "delegated:update-credentials"}, nil
}

// ProvisionSubsystemAccess 首次接入的部署：装配输入后调用部署 Agent。
func (adapter *SubsystemAccessDeploymentAdapter) ProvisionSubsystemAccess(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	var identityErr error
	operation, identityErr = adapter.resolveRegistration(ctx, operation)
	if identityErr != nil {
		return application.SubsystemAccessStageOutcome{}, identityErr
	}

	if operation.Action != application.SubsystemAccessActionOnboard {
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("provisioning for action %s is not wired: %w", operation.Action, application.ErrSubsystemAccessInvalidRequest)
	}
	if run == nil {
		run = &application.SubsystemAccessRunState{}
	}
	if run.Credentials == nil {
		// 凭据只在一次运行内存在：缺失通常说明进程重启后从 DEPLOY 继续。
		// 首次接入若已经登记过（部署状态行存在），说明 AUTH 的效果已发生，
		// 此时不应让用户重跑接入，而应通过受控更新服务重新准备凭据（不落明文）。
		if operation.Action == application.SubsystemAccessActionOnboard {
			if _, err := adapter.rebuildOnboardCredentials(ctx, operation, run); err == nil && run.Credentials != nil {
				return adapter.provisionOnboard(ctx, operation, run)
			}
		}
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "CREDENTIALS_MISSING",
			Message: "本次运行未取得部署凭据，且无法从已登记状态安全重建；请重新执行接入"}, nil
	}
	return adapter.provisionOnboard(ctx, operation, run)
}

// provisionOnboard 是首次接入的部署主体（凭据已就绪）。
func (adapter *SubsystemAccessDeploymentAdapter) provisionOnboard(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	snapshot := ""
	if adapter.configuration != nil {
		var err error
		snapshot, err = adapter.configuration.Snapshot(ctx, operation)
		if err != nil {
			return application.SubsystemAccessStageOutcome{}, err
		}
	}
	outcome, err := adapter.provisionOnboardWithCandidate(ctx, operation, run)
	if err == nil && outcome.Status == application.SubsystemAccessStatusSucceeded && snapshot != "" {
		outcome.EvidenceRef = accessCandidatePrefix + snapshot + ";" + outcome.EvidenceRef
	}
	return outcome, err
}

func (adapter *SubsystemAccessDeploymentAdapter) provisionOnboardWithCandidate(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	target, ok, err := resolveSubsystemAccessTarget(ctx, adapter.catalog, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !ok {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "OUT_OF_SCOPE",
			Message: "目标不在当前清单的可接入范围内"}, nil
	}
	relatedClients, err := adapter.related.ResolveSubsystemRelatedServiceClients(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		// 相关服务客户端是部署的输入依赖：读取失败属"结果未知/可重试"，不伪装成成功。
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("resolve related service clients: %w", err)
	}
	issuer, err := adapter.issuers.ResolveSubsystemAccessIssuer(ctx, operation.AuthProvider)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("resolve subsystem access issuer: %w", err)
	}
	input, err := application.BuildSubsystemOnboardingProvisioningInput(application.SubsystemOnboardingProvisioningAssembly{
		TenantID: operation.TenantID, ApplicationID: operation.ApplicationID, EnvironmentID: operation.EnvironmentID,
		ApplicationCode: operation.ApplicationCode, Environment: operation.EnvironmentCode,
		Issuer: strings.TrimSpace(issuer), AuthenticationProvider: strings.TrimSpace(operation.AuthProvider),
		ManifestChecksum: target.ManifestChecksum,
		ClientID:         run.Credentials.ClientID, ClientSecret: run.Credentials.ClientSecret,
		CatalogPublisherClientID:     run.Credentials.CatalogPublisherClientID,
		CatalogPublisherClientSecret: run.Credentials.CatalogPublisherClientSecret,
		ServiceCredentials:           run.Credentials.ServiceCredentials,
		RelatedServiceClients:        relatedClients,
		RedirectURI:                  run.Credentials.RedirectURI, PublicURL: run.Credentials.PublicURL,
		PathPrefix: target.PathPrefix, UpstreamURL: target.UpstreamURL,
	})
	if err != nil {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "PROVISIONING_INPUT_INVALID",
			Message: err.Error()}, nil
	}
	if err := adapter.prepareRuntimeLicense(ctx, operation, &input); err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if err := adapter.provisioner.Provision(ctx, input); err != nil {
		// 部署结果未知（可能已提交）：保留租约，下一轮先核对证据再决定，不重复执行。
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("provision subsystem: %w", err)
	}
	if err := adapter.confirmRuntimeLicense(ctx, operation, input); err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	// 首次接入成功后同样要落期望清单（漂移检测基准）；更新路径已在派发前落库，
	// 首次接入若漏掉会出现"已接入但清单基准为空"，后续永远判不出漂移。
	if adapter.manifestState != nil && strings.TrimSpace(target.ManifestChecksum) != "" {
		if err := adapter.manifestState.SetSubsystemDesiredManifest(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, strings.TrimSpace(target.ManifestChecksum), time.Now().UTC()); err != nil {
			return application.SubsystemAccessStageOutcome{}, fmt.Errorf("record desired manifest after onboarding: %w", err)
		}
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "provision:" + strings.TrimSpace(target.ManifestChecksum)}, nil
}

// UpdateSubsystemAccess 接管/更新/恢复：更新路径尚未接线（明确报错，不静默成功）。
func (adapter *SubsystemAccessDeploymentAdapter) UpdateSubsystemAccess(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	snapshot := ""
	if adapter.configuration != nil {
		var err error
		snapshot, err = adapter.configuration.Snapshot(ctx, operation)
		if err != nil {
			return application.SubsystemAccessStageOutcome{}, err
		}
	}
	outcome, err := adapter.updateSubsystemAccessWithCandidate(ctx, operation, run)
	if err == nil && outcome.Status == application.SubsystemAccessStatusSucceeded && snapshot != "" {
		outcome.EvidenceRef = accessCandidatePrefix + snapshot + ";" + outcome.EvidenceRef
	}
	return outcome, err
}

func (adapter *SubsystemAccessDeploymentAdapter) updateSubsystemAccessWithCandidate(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	// 观察期门禁（只读）：未结束/未开始/已存在时受控更新必须等待，而不是继续执行。
	if adapter.observation != nil {
		allowed, reason, err := adapter.observation.CanProceedWithUpdate(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
		if err != nil {
			return application.SubsystemAccessStageOutcome{}, fmt.Errorf("read auth observation gate: %w", err)
		}
		if !allowed {
			message := "认证观察期未结束，受控更新需等待"
			if strings.TrimSpace(reason) != "" {
				message = message + "（" + strings.TrimSpace(reason) + "）"
			}
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusWaiting,
				ErrorCode: "AUTH_OBSERVATION_PENDING", Message: message}, nil
		}
	}
	// 凭据准备未接线时明确报错（不可重试）。
	if adapter.credentials == nil || adapter.browser == nil {
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("subsystem access update credentials are not wired yet: %w", application.ErrSubsystemAccessStageNotWired)
	}
	target, ok, err := resolveSubsystemAccessTarget(ctx, adapter.catalog, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !ok {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "OUT_OF_SCOPE",
			Message: "目标不在当前清单的可接入范围内"}, nil
	}
	serviceCredentials, err := adapter.credentials.Ensure(ctx, application.SubsystemUpdateServiceCredentialRequest{
		TenantID: operation.TenantID, OperatorID: operation.OperatorID,
		ApplicationID: operation.ApplicationID, EnvironmentID: operation.EnvironmentID,
		ApplicationCode: operation.ApplicationCode, Environment: operation.EnvironmentCode,
		Operation:    string(operation.Action),
		Requirements: application.SubsystemUpdateServiceCredentialRequirements(operation.ApplicationCode),
	})
	if err != nil {
		if errors.Is(err, application.ErrSubsystemAccessStageNotWired) || errors.Is(err, application.ErrSubsystemUpdateServiceClientInvalid) {
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "UPDATE_CREDENTIALS_UNAVAILABLE", Message: err.Error()}, nil
		}
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("prepare update service credentials: %w", err)
	}
	// 回调地址与更新装配同一派生来源：公开地址 + 清单路径前缀 + /auth/callback。
	redirectURI := strings.TrimRight(strings.TrimSpace(operation.PublicOrigin), "/") + strings.TrimRight(strings.TrimSpace(target.PathPrefix), "/") + "/auth/callback"
	browserCredential, err := adapter.browser.Ensure(ctx, operation.TenantID, operation.OperatorID, operation.ApplicationID, operation.EnvironmentID, operation.ApplicationCode, operation.EnvironmentCode, redirectURI)
	if err == nil && run != nil {
		// 结果阶段的认证投影需要 ClientID：与首次接入一致，写入本次运行状态（仅内存）。
		run.Credentials = &application.SubsystemProvisioningCredentials{
			ClientID:           browserCredential.ClientID,
			ClientSecret:       browserCredential.ClientSecret,
			ServiceCredentials: serviceCredentials,
			PublicURL:          strings.TrimSpace(operation.PublicOrigin),
		}
	}
	if err != nil {
		if errors.Is(err, application.ErrSubsystemAccessStageNotWired) || errors.Is(err, application.ErrSubsystemUpdateServiceClientInvalid) {
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "UPDATE_CREDENTIALS_UNAVAILABLE", Message: err.Error()}, nil
		}
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("prepare update browser credentials: %w", err)
	}
	issuer, err := adapter.issuers.ResolveSubsystemAccessIssuer(ctx, operation.AuthProvider)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("resolve subsystem access issuer: %w", err)
	}
	// InitialRuntimeProvisioning 派生（与既有条件逐条一致）。
	initialRuntimeProvisioning := false
	if adapter.productionRuntime && adapter.deploymentContext != nil {
		deploymentContext, contextErr := adapter.deploymentContext.ReadSubsystemAccessDeploymentContext(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
		if contextErr != nil {
			return application.SubsystemAccessStageOutcome{}, fmt.Errorf("read subsystem deployment context: %w", contextErr)
		}
		action := strings.ToUpper(strings.TrimSpace(string(operation.Action)))
		if deploymentContext.Found &&
			(action == "ADOPT" || (action == "RETRY" && strings.EqualFold(deploymentContext.Status, string(application.SubsystemDeploymentStatusFailed)))) &&
			deploymentContext.AppliedManifestChecksum == "" && deploymentContext.ManifestLastAppliedAt == nil {
			initialRuntimeProvisioning = true
		}
	}

	// 目录发布者凭据：按应用判定，需要时恢复（与既有实现一致）。
	publisherClientID, publisherSecret := "", ""
	if application.RequiresSubsystemCatalogPublisherCredential(operation.ApplicationCode) {
		if adapter.publisher == nil {
			return application.SubsystemAccessStageOutcome{}, fmt.Errorf("subsystem access catalog publisher credentials are not wired yet: %w", application.ErrSubsystemAccessStageNotWired)
		}
		publisherCredential, publisherErr := adapter.publisher.Ensure(ctx, operation.TenantID, operation.OperatorID, operation.ApplicationID, operation.EnvironmentID, operation.ApplicationCode, operation.EnvironmentCode)
		if publisherErr != nil {
			if errors.Is(publisherErr, application.ErrSubsystemUpdateServiceClientInvalid) {
				return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "UPDATE_CREDENTIALS_UNAVAILABLE", Message: publisherErr.Error()}, nil
			}
			return application.SubsystemAccessStageOutcome{}, fmt.Errorf("prepare catalog publisher credentials: %w", publisherErr)
		}
		publisherClientID, publisherSecret = publisherCredential.ClientID, publisherCredential.ClientSecret
	}
	// 相关服务客户端元数据（不含密钥）：与首次接入路径同一取数，避免更新后 Agent 侧绑定信息不刷新。
	relatedClients, err := adapter.related.ResolveSubsystemRelatedServiceClients(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("resolve related service clients: %w", err)
	}
	input, err := application.BuildSubsystemUpdateProvisioningInput(application.SubsystemUpdateProvisioningAssembly{
		TenantID: operation.TenantID, ApplicationID: operation.ApplicationID, ApplicationCode: operation.ApplicationCode,
		Environment: operation.EnvironmentCode, ManifestChecksum: target.ManifestChecksum,
		Issuer: issuer, AuthenticationProvider: operation.AuthProvider,
		AuthenticationRuntimeUpdate: true,
		PublicBaseURL:               operation.PublicOrigin, UpstreamURL: target.UpstreamURL, PathPrefix: target.PathPrefix,
		ClientID: browserCredential.ClientID, ClientSecret: browserCredential.ClientSecret,
		ServiceCredentials:           serviceCredentials,
		CatalogPublisherClientID:     publisherClientID,
		CatalogPublisherClientSecret: publisherSecret,
		RelatedServiceClients:        relatedClients,
		InitialRuntimeProvisioning:   initialRuntimeProvisioning,
	})
	if err != nil {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "PROVISIONING_INPUT_INVALID", Message: err.Error()}, nil
	}
	// 期望清单落库（既有更新流程在协调 IdP 与派发部署之前执行；空清单不落库）。
	if adapter.manifestState != nil && strings.TrimSpace(target.ManifestChecksum) != "" {
		if err := adapter.manifestState.SetSubsystemDesiredManifest(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, strings.TrimSpace(target.ManifestChecksum), time.Now().UTC()); err != nil {
			return application.SubsystemAccessStageOutcome{}, fmt.Errorf("record desired manifest: %w", err)
		}
	}
	// Keycloak 身份提供方协调（既有更新流程在派发部署任务前执行）：仅注入了端口时执行。
	if err := adapter.reconcileKeycloakBroker(ctx, operation); err != nil {
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("reconcile keycloak broker: %w", err)
	}
	if err := adapter.prepareRuntimeLicense(ctx, operation, &input); err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if err := adapter.provisioner.Update(ctx, input); err != nil {
		// 结果未知（可能已提交）：保留租约，下一轮先核对证据再决定。
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("update subsystem: %w", err)
	}
	if err := adapter.confirmRuntimeLicense(ctx, operation, input); err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "update:" + strings.TrimSpace(target.ManifestChecksum)}, nil
}

// SetKeycloakProjection 由组合根注入共享认证投影服务与 realm；未注入时结果阶段标注 skipped，
// 不静默假装完成（认证投影是接入完成的一部分）。
func (adapter *SubsystemAccessDeploymentAdapter) SetKeycloakProjection(projection *application.KeycloakAuthorizationProjectionService, realm string) {
	adapter.projection = projection
	adapter.keycloakRealm = strings.TrimSpace(realm)
}

// SetInitialAdminMarker 由组合根注入"初始管理员已授权"落库端口；未注入时结果阶段标注 skipped。
func (adapter *SubsystemAccessDeploymentAdapter) SetInitialAdminMarker(marker SubsystemAccessInitialAdminMarker) {
	adapter.initialAdmin = marker
}

// SetAppliedManifestReader 由组合根注入"已应用清单"读取端口；未注入时不核对清单摘要。
func (adapter *SubsystemAccessDeploymentAdapter) SetAppliedManifestReader(reader SubsystemAccessAppliedManifestReader) {
	adapter.appliedManifest = reader
}

// SetManifestStateStore 由组合根注入期望清单落库端口；未注入时不落库（保持既有行为）。
func (adapter *SubsystemAccessDeploymentAdapter) SetManifestStateStore(store SubsystemAccessManifestStateStore) {
	adapter.manifestState = store
}

// SetKeycloakBrokerReconciliation 由组合根注入 broker 协调所需端口；未注入时不协调（保持既有行为）。
func (adapter *SubsystemAccessDeploymentAdapter) SetKeycloakBrokerReconciliation(reconciliation SubsystemAccessKeycloakBrokerReconciliation) {
	adapter.brokerReconciliation = reconciliation
}

// SetInitialRuntimeProvisioningContext 由组合根注入运行环境标记与状态上下文读取器。
func (adapter *SubsystemAccessDeploymentAdapter) SetInitialRuntimeProvisioningContext(production bool, reader SubsystemAccessDeploymentContextReader) {
	adapter.productionRuntime = production
	adapter.deploymentContext = reader
}

// reconcileKeycloakBroker 与既有实现逐条一致：确保 broker 凭据 → 协调 IdP；
// 存量为不可逆哈希且 IdP 未配置时，先轮换再协调（明文仅内存，不记录、不返回）。
func (adapter *SubsystemAccessDeploymentAdapter) reconcileKeycloakBroker(ctx context.Context, operation application.SubsystemAccessOperation) error {
	credentials := adapter.brokerReconciliation.Credentials
	control := adapter.brokerReconciliation.Control
	if credentials == nil || control == nil {
		return nil
	}
	brokerID, brokerSecret, err := credentials.EnsureKeycloakBroker(ctx, operation.TenantID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(brokerSecret) != "" {
		return control.EnsureBroker(ctx, brokerID, brokerSecret)
	}
	verifyErr := control.VerifyBrokerExists(ctx)
	if verifyErr == nil {
		return nil
	}
	if adapter.brokerReconciliation.IsRecoverable == nil || !adapter.brokerReconciliation.IsRecoverable(verifyErr) {
		return verifyErr
	}
	brokerID, brokerSecret, err = credentials.RecoverKeycloakBroker(ctx, operation.TenantID, operation.OperatorID)
	if err != nil {
		return fmt.Errorf("rotate broker credential after incomplete IdP config: %w", err)
	}
	if strings.TrimSpace(brokerSecret) == "" {
		return errors.New("broker credential recovery returned an empty secret")
	}
	return control.EnsureBroker(ctx, brokerID, brokerSecret)
}

// SubsystemAccessDeploymentReady 就绪核验：只有受控部署状态为 READY 才算成功。
func (adapter *SubsystemAccessDeploymentAdapter) SubsystemAccessDeploymentReady(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemAccessStageOutcome, error) {
	status, generation, attemptCount, found, err := adapter.state.ReadSubsystemAccessDeploymentState(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("read subsystem deployment state: %w", err)
	}
	if !found {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusRunning,
			Message: "尚未读取到受控部署状态"}, nil
	}
	evidence := fmt.Sprintf("deployment:%s:generation=%d:attempts=%d", status, generation, attemptCount)
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case application.SubsystemDeploymentStatusReady:
		// 受控部署必须是"有生命周期尝试"的记录：generation/attempts 全为 0 的 READY
		// 说明状态行不是本次受控流程写出来的，不能据此宣告接入成功。
		if generation == 0 && attemptCount == 0 {
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusRunning,
				ErrorCode: "READINESS_UNVERIFIED", Message: "部署状态缺少受控代次信息，暂不能确认接入完成", EvidenceRef: evidence}, nil
		}
		// 清单核对：READY 只说明容器起来了，必须确认"本次期望清单"确实已应用。
		if outcome, ok, verifyErr := adapter.verifyAppliedManifest(ctx, operation); verifyErr != nil {
			return application.SubsystemAccessStageOutcome{}, verifyErr
		} else if ok {
			outcome.EvidenceRef = evidence + ":" + outcome.EvidenceRef
			return outcome, nil
		}
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded, EvidenceRef: evidence}, nil
	case application.SubsystemDeploymentStatusFailed:
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
			ErrorCode: "DEPLOYMENT_FAILED", Message: "部署未通过核验", EvidenceRef: evidence}, nil
	default:
		// 仍在进行中：Worker 保留租约，下一轮继续核对（不重复部署）。
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusRunning, EvidenceRef: evidence}, nil
	}
}

// SubsystemAccessDeploymentStateGORMReader 读取 subsystem_deployment_state（就绪核验的唯一来源）。
type SubsystemAccessDeploymentStateGORMReader struct {
	database *gorm.DB
}

// NewSubsystemAccessDeploymentStateGORMReader 构造读取器。
func NewSubsystemAccessDeploymentStateGORMReader(database *gorm.DB) (*SubsystemAccessDeploymentStateGORMReader, error) {
	if database == nil {
		return nil, errors.New("subsystem access deployment state reader requires a database")
	}
	return &SubsystemAccessDeploymentStateGORMReader{database: database}, nil
}

// ReadSubsystemAccessDeploymentState 返回状态/代次/尝试次数；未找到返回 found=false（不是错误）。
func (reader *SubsystemAccessDeploymentStateGORMReader) ReadSubsystemAccessDeploymentState(ctx context.Context, tenantID, applicationCode, environmentCode string) (string, uint64, uint32, bool, error) {
	var row struct {
		Status       string
		Generation   uint64
		AttemptCount uint32
	}
	result := reader.database.WithContext(ctx).Table("subsystem_deployment_state").
		Select("status, generation, attempt_count").
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environmentCode))).
		Limit(1).Scan(&row)
	if result.Error != nil {
		return "", 0, 0, false, fmt.Errorf("read subsystem deployment state: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return "", 0, 0, false, nil
	}
	return row.Status, row.Generation, row.AttemptCount, true, nil
}

// SubsystemAccessDeploymentContext 是 InitialRuntimeProvisioning 派生所需的受控部署状态上下文。
// 与既有的三元组读取并存（附加式），避免影响既有调用方。
type SubsystemAccessDeploymentContext struct {
	Found                   bool
	Status                  string
	AppliedManifestChecksum string
	ManifestLastAppliedAt   *time.Time
}

// ReadSubsystemAccessDeploymentContext 读取派生所需的额外字段（迁移 000095 的清单漂移状态）。
func (reader *SubsystemAccessDeploymentStateGORMReader) ReadSubsystemAccessDeploymentContext(ctx context.Context, tenantID, applicationCode, environmentCode string) (SubsystemAccessDeploymentContext, error) {
	var row struct {
		Status                  string
		AppliedManifestChecksum sql.NullString
		ManifestLastAppliedAt   sql.NullTime
	}
	result := reader.database.WithContext(ctx).Table("subsystem_deployment_state").
		Select("status, applied_manifest_checksum, manifest_last_applied_at").
		Where("tenant_id = ? AND application_code = ? AND environment_code = ?",
			strings.TrimSpace(tenantID), strings.TrimSpace(applicationCode), strings.ToLower(strings.TrimSpace(environmentCode))).
		Limit(1).Scan(&row)
	if result.Error != nil {
		return SubsystemAccessDeploymentContext{}, fmt.Errorf("read subsystem deployment context: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return SubsystemAccessDeploymentContext{}, nil
	}
	context := SubsystemAccessDeploymentContext{Found: true, Status: row.Status}
	if row.AppliedManifestChecksum.Valid {
		context.AppliedManifestChecksum = row.AppliedManifestChecksum.String
	}
	if row.ManifestLastAppliedAt.Valid {
		appliedAt := row.ManifestLastAppliedAt.Time
		context.ManifestLastAppliedAt = &appliedAt
	}
	return context, nil
}

// resolveSubsystemAccessTarget 从清单解析目标；未命中返回 ok=false。
func resolveSubsystemAccessTarget(ctx context.Context, catalog SubsystemAccessCatalogSource, operation application.SubsystemAccessOperation) (application.SubsystemProvisioningTarget, bool, error) {
	capabilities, err := catalog.AvailableCapabilities(ctx)
	if err != nil {
		return application.SubsystemProvisioningTarget{}, false, fmt.Errorf("read subsystem capability catalog: %w", err)
	}
	for _, target := range capabilities.Targets {
		if strings.EqualFold(strings.TrimSpace(target.ApplicationCode), strings.TrimSpace(operation.ApplicationCode)) &&
			strings.EqualFold(strings.TrimSpace(target.Environment), strings.TrimSpace(operation.EnvironmentCode)) {
			return target, true, nil
		}
	}
	return application.SubsystemProvisioningTarget{}, false, nil
}

// FinalizeSubsystemAccess 完成收口（RESULT 阶段）：把"初始管理员已授权"落到部署状态。
//
// 旧完成链在 Agent 成功后还会：确认运行、应用清单、分配初始管理员、完成认证投影、
// 写 READY。统一入口目前只承担"初始管理员授权"这一步（其余由既有部署/更新流程
// 在各自阶段完成）；未注入标记端口时不阻断，但证据必须显式标注 skipped。
func (adapter *SubsystemAccessDeploymentAdapter) FinalizeSubsystemAccess(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	var identityErr error
	operation, identityErr = adapter.resolveRegistration(ctx, operation)
	if identityErr != nil {
		return application.SubsystemAccessStageOutcome{}, identityErr
	}

	if adapter.configuration != nil {
		verified, err := adapter.configuration.Verify(ctx, operation)
		if err != nil {
			return application.SubsystemAccessStageOutcome{}, err
		}
		if !verified {
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "CONFIGURATION_CHANGED", Message: "部署候选已变更或缺少部署证据，请重新检查条件后发起受控更新"}, nil
		}
	}
	outcome, err := adapter.finalizeInitialAdmin(ctx, operation, run)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		return outcome, err
	}
	projectionOutcome, projected, err := adapter.projectKeycloakAuthorization(ctx, operation, run)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !projected {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
			EvidenceRef: outcome.EvidenceRef + ":projection-skipped"}, nil
	}
	if projectionOutcome.Status != application.SubsystemAccessStatusSucceeded {
		return projectionOutcome, nil
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: outcome.EvidenceRef + ":" + projectionOutcome.EvidenceRef}, nil
}

// finalizeInitialAdmin 把初始管理员授权落库（结果阶段第一步）。
func (adapter *SubsystemAccessDeploymentAdapter) finalizeInitialAdmin(ctx context.Context, operation application.SubsystemAccessOperation, _ *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	admin := strings.TrimSpace(operation.InitialAdminID)
	if admin == "" {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
			EvidenceRef: "result:no-initial-admin"}, nil
	}
	if adapter.initialAdmin == nil {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
			EvidenceRef: "result:initial-admin-skipped"}, nil
	}
	if err := adapter.initialAdmin.MarkSubsystemInitialAccessAssigned(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, admin, time.Now().UTC()); err != nil {
		// 结果未知：保留租约重试，不伪装成功。
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("mark initial access assigned: %w", err)
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "result:initial-admin:" + admin}, nil
}

// projectKeycloakAuthorization 对 Keycloak 提供方执行认证投影（复用共享服务）。
// projected=false 表示非 Keycloak 或未注入服务；缺少 ClientID 必须失败。
func (adapter *SubsystemAccessDeploymentAdapter) projectKeycloakAuthorization(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, bool, error) {
	if !strings.EqualFold(strings.TrimSpace(operation.AuthProvider), "keycloak") {
		return application.SubsystemAccessStageOutcome{}, false, nil
	}
	if adapter.projection == nil || strings.TrimSpace(adapter.keycloakRealm) == "" {
		return application.SubsystemAccessStageOutcome{}, false, nil
	}
	clientID := ""
	if run != nil && run.Credentials != nil {
		clientID = strings.TrimSpace(run.Credentials.ClientID)
	}
	if clientID == "" && adapter.browserReferences != nil {
		var err error
		clientID, err = adapter.browserReferences.ReadSubsystemAccessBrowserClientID(ctx, operation.TenantID, operation.ApplicationID, operation.EnvironmentID, operation.ApplicationCode, operation.EnvironmentCode)
		if err != nil {
			return application.SubsystemAccessStageOutcome{}, true, fmt.Errorf("read projection client reference: %w", err)
		}
	}
	if clientID == "" {
		// 没有 ClientID 就无法投影：不能假装成功，也不能空跑。
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
			ErrorCode: "PROJECTION_CLIENT_MISSING", Message: "缺少 Keycloak Client ID，无法完成认证投影"}, true, nil
	}
	result, err := adapter.projection.Project(ctx, application.KeycloakProjectionInput{
		TenantID: operation.TenantID, ApplicationID: operation.ApplicationID, EnvironmentID: operation.EnvironmentID,
		ClientID: clientID, Realm: adapter.keycloakRealm,
	})
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, true, fmt.Errorf("project keycloak authorization: %w", err)
	}
	if result.ErrorCode != "" {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
			ErrorCode: result.ErrorCode, Message: result.Diagnostic}, true, nil
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "projection:keycloak"}, true, nil
}

// verifyAppliedManifest 比对已应用清单与本次期望清单。
// 返回 ok=false 表示未注入端口（保持既有行为，不核对）。
func (adapter *SubsystemAccessDeploymentAdapter) verifyAppliedManifest(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemAccessStageOutcome, bool, error) {
	if adapter.appliedManifest == nil {
		return application.SubsystemAccessStageOutcome{}, false, nil
	}
	target, ok, err := resolveSubsystemAccessTarget(ctx, adapter.catalog, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, true, err
	}
	if !ok || strings.TrimSpace(target.ManifestChecksum) == "" {
		return application.SubsystemAccessStageOutcome{}, false, nil
	}
	applied, verified, err := adapter.appliedManifest.ReadSubsystemAccessAppliedManifest(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, true, fmt.Errorf("read applied manifest: %w", err)
	}
	if !verified {
		// 未上报 ≠ 一致：继续等待核对，而不是宣告成功或失败。
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusRunning,
			ErrorCode: "MANIFEST_UNVERIFIED", Message: "无法确认清单是否已应用（Agent 未上报）",
			EvidenceRef: "manifest:unverified"}, true, nil
	}
	applied = strings.TrimSpace(applied)
	if applied == "" {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusRunning,
			ErrorCode: "MANIFEST_NOT_APPLIED", Message: "部署状态尚未记录已应用清单",
			EvidenceRef: "manifest:pending"}, true, nil
	}
	if applied != strings.TrimSpace(target.ManifestChecksum) {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
			ErrorCode: "MANIFEST_MISMATCH", Message: "已应用清单与期望清单不一致，需要重新部署",
			EvidenceRef: "manifest:" + applied}, true, nil
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "manifest:" + applied}, true, nil
}

// errOnboardCredentialsNotRebuildable 表示无法安全重建首次接入凭据。
var errOnboardCredentialsNotRebuildable = errors.New("onboard credentials are not rebuildable")

// rebuildOnboardCredentials 在"进程重启后从部署阶段继续"时，从已登记状态重建部署凭据。
//
// 前提：部署状态行存在（OnboardSubsystem 已创建目录与客户端）。此时复用受控更新的
// 凭据服务（浏览器客户端按现有 Client 重新签发、服务凭据轮换、目录发布者恢复），
// 不保存或复用任何明文密钥；状态行不存在说明认证效果未发生，必须重新执行接入。
func (adapter *SubsystemAccessDeploymentAdapter) rebuildOnboardCredentials(ctx context.Context, operation application.SubsystemAccessOperation, run *application.SubsystemAccessRunState) (application.SubsystemAccessStageOutcome, error) {
	if adapter.credentials == nil || adapter.browser == nil || adapter.publisher == nil || adapter.state == nil {
		return application.SubsystemAccessStageOutcome{}, errOnboardCredentialsNotRebuildable
	}
	_, _, _, found, err := adapter.state.ReadSubsystemAccessDeploymentState(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !found {
		return application.SubsystemAccessStageOutcome{}, errOnboardCredentialsNotRebuildable
	}
	target, ok, err := resolveSubsystemAccessTarget(ctx, adapter.catalog, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !ok {
		return application.SubsystemAccessStageOutcome{}, errOnboardCredentialsNotRebuildable
	}
	publisher, err := adapter.publisher.Ensure(ctx, operation.TenantID, operation.OperatorID,
		operation.ApplicationID, operation.EnvironmentID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	redirectURI := strings.TrimRight(strings.TrimSpace(operation.PublicOrigin), "/") + "/" + strings.Trim(target.PathPrefix, "/") + "/auth/callback"
	browser, err := adapter.browser.Ensure(ctx, operation.TenantID, operation.OperatorID,
		operation.ApplicationID, operation.EnvironmentID, operation.ApplicationCode, operation.EnvironmentCode, redirectURI)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	serviceCredentials, err := adapter.credentials.Ensure(ctx, application.SubsystemUpdateServiceCredentialRequest{
		TenantID: operation.TenantID, OperatorID: operation.OperatorID,
		ApplicationID: operation.ApplicationID, EnvironmentID: operation.EnvironmentID,
		ApplicationCode: operation.ApplicationCode, Environment: operation.EnvironmentCode,
		Operation:    strings.ToUpper(string(operation.Action)),
		Requirements: application.SubsystemUpdateServiceCredentialRequirements(operation.ApplicationCode),
	})
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	run.Credentials = &application.SubsystemProvisioningCredentials{
		ClientID:                     browser.ClientID,
		ClientSecret:                 browser.ClientSecret,
		CatalogPublisherClientID:     publisher.ClientID,
		CatalogPublisherClientSecret: publisher.ClientSecret,
		ServiceCredentials:           serviceCredentials,
		RedirectURI:                  redirectURI,
		PublicURL:                    strings.TrimSpace(operation.PublicOrigin),
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "credentials:rebuilt"}, nil
}

// SetIdentityReaders 注入只读目录和客户端查询；恢复结果阶段不会签发新凭据。
func (adapter *SubsystemAccessDeploymentAdapter) SetIdentityReaders(registry SubsystemAccessRegistrationLookup, browser SubsystemAccessBrowserReferenceReader) {
	adapter.registry, adapter.browserReferences = registry, browser
}

func (adapter *SubsystemAccessDeploymentAdapter) resolveRegistration(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemAccessOperation, error) {
	if adapter.registry == nil {
		return operation, nil
	}
	appID, envID, found, err := adapter.registry.LookupSubsystemAccessRegistration(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return operation, err
	}
	if !found || appID == "" || envID == "" {
		return operation, fmt.Errorf("subsystem registration is missing: %w", application.ErrSubsystemAccessInvalidRequest)
	}
	if (operation.ApplicationID != "" && operation.ApplicationID != appID) || (operation.EnvironmentID != "" && operation.EnvironmentID != envID) {
		return operation, fmt.Errorf("subsystem registration changed: %w", application.ErrSubsystemAccessInvalidRequest)
	}
	operation.ApplicationID, operation.EnvironmentID = appID, envID
	return operation, nil
}

func (adapter *SubsystemAccessDeploymentAdapter) SetConfigurationEvidence(tracker SubsystemAccessConfigurationEvidence) {
	adapter.configuration = tracker
}
