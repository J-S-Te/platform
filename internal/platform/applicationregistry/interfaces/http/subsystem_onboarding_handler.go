package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdhttp "net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

// readCatalogPublisherCredentials 返回平台操作者为 contract_management 子系统预置的
// 长期目录发布 OAuth 客户端凭据。凭据保存在平台自身环境中，因此 API 容器
// （刻意无法读取子系统磁盘上的 .env.local）可以把它们转交给部署助手，
// 完成重建后的目录同步。
func readCatalogPublisherCredentials() (string, string, bool) {
	id := strings.TrimSpace(os.Getenv("CONTRACT_CATALOG_PUBLISHER_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("CONTRACT_CATALOG_PUBLISHER_CLIENT_SECRET"))
	if id == "" || secret == "" {
		return "", "", false
	}
	return id, secret, true
}

func optionalIssuerAlias(value string) *string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || value == "platform" || value == "basic_platform" {
		return nil
	}
	return &value
}

// resolveApplicationContext 按（编码, 环境名）查找（应用 ID, 环境 ID）。重建后的目录同步
// 需要平台侧主键而非人类可读编码来定位应用。查找复用既有门户列表路径，
// 不为此新增仓储方法。
func (handler *SubsystemOnboardingHandler) resolveApplicationContext(writer stdhttp.ResponseWriter, request *stdhttp.Request, applicationCode, environment string) (string, string, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		return "", "", false
	}
	return handler.resolveApplicationContextForIdentity(request, principal.Tenant.ID, principal.User.ID, applicationCode, environment)
}

func (handler *SubsystemOnboardingHandler) resolveApplicationContextForIdentity(request *stdhttp.Request, tenantID, identityID, applicationCode, environment string) (string, string, bool) {
	if applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), tenantID, applicationCode, environment); err == nil {
		return applicationID, environmentID, true
	}
	items, err := handler.service.ListPortalApplications(request.Context(), tenantID, identityID, environment)
	if err != nil {
		return "", "", false
	}
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Code), applicationCode) &&
			strings.EqualFold(strings.TrimSpace(item.Environment), environment) {
			return strings.TrimSpace(item.ApplicationID), strings.TrimSpace(item.EnvironmentID), true
		}
	}
	return "", "", false
}

type subsystemOnboardingService interface {
	OnboardSubsystem(context.Context, application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error)
	RegisterSubsystemDirectory(context.Context, application.SubsystemDirectoryRegistrationInput) (application.SubsystemDirectoryRegistrationResult, error)
	ListRegisteredApplicationCodes(context.Context, string) ([]string, error)
	ListPortalApplications(context.Context, string, string, string) ([]application.PortalApplication, error)
	ResolveApplicationEnvironment(context.Context, string, string, string) (string, string, error)
	PreflightValidate(context.Context, application.SubsystemOnboardingInput) error
}

// subsystemEnvironmentIssuerResolver 由持久化控制面仓储实现的可选能力。
// 它让 UpdateSubsystem 能区分真正的 Issuer 切换与"环境本就在使用该提供方"的重建，
// 同时保持轻量测试服务兼容（无此能力时按保守规则处理）。
type subsystemEnvironmentIssuerResolver interface {
	ResolveEnvironmentIssuerAlias(context.Context, string, string, string) (string, error)
}

type subsystemEnvironmentRedirectURIResolver interface {
	ResolveApplicationEnvironmentRedirectURI(context.Context, string, string, string) (string, error)
}

type keycloakAuthorizationCatalog interface {
	ListKeycloakRoleCodes(context.Context, string, string) ([]string, error)
}

type keycloakClientMappingStore interface {
	SaveKeycloakClientMapping(context.Context, string, string, string, string, string) error
	BackfillKeycloakAuthorization(context.Context, string, string, string) error
}

// keycloakClientCompatibilityResolver 是接入重试期间使用的可选持久化能力：
// 当环境标签后来被规范化时（例如 dev 部署曾登记 contract_management-prod-web），
// 保留先前登记的浏览器客户端 ID。解析器以租户/应用/环境为界，
// 绝不通过替换 prod/dev 字符串去猜。
type keycloakClientCompatibilityResolver interface {
	ResolveEffectiveKeycloakClient(context.Context, string, string, string, string) (KeycloakClientResolution, error)
}

// KeycloakClientResolution 是同步时实际采用的生效 Keycloak Client 身份：
// ClientID 为本次请求使用的值；CanonicalClientID 为规范化 ID；Source 标识取值
// 来源（canonical 或兼容解析）；LegacyCompatibility 表示沿用环境标签规范化之前
// 登记的旧客户端 ID。
type KeycloakClientResolution struct {
	ClientID            string
	CanonicalClientID   string
	Source              string
	LegacyCompatibility bool
}

// KeycloakClientMapping 是单个环境 Realm 客户端的刻意非敏感投影。
// 认证页面凭它刷新后仍可用，无需浏览器保存客户端 Secret 或从同步成功
// 响应反推状态。
type KeycloakClientMapping struct {
	Realm        string     `json:"realm,omitempty"`
	ClientID     string     `json:"client_id,omitempty"`
	Status       string     `json:"status"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	Exists       bool       `json:"exists"`
}

// keycloakClientMappingInspector 刻意做成可选，旧测试与部署适配器因此保持兼容。
// 缺失按"映射未配置"报告；它绝不开任何切换门禁。
type keycloakClientMappingInspector interface {
	GetKeycloakClientMapping(context.Context, string, string, string) (KeycloakClientMapping, error)
}

// KeycloakSwitchGate 是非敏感、可审计的切换前置条件。值为 false 表示前置条件
// 未满足，或本服务器无法独立核验；客户端必须把两种情况都当作阻断。
type KeycloakSwitchGate struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Passed     bool   `json:"passed"`
	Detail     string `json:"detail"`
	NextAction string `json:"next_action,omitempty"`
}

// KeycloakSwitchReadiness 刻意由独立持有的校验器供给。应用登记绝不允许
// 从 Admin API 写入成功或浏览器断言推断"代理登录已验证"。
type KeycloakSwitchReadiness struct {
	Gates       []KeycloakSwitchGate `json:"switch_gates"`
	SwitchReady bool                 `json:"switch_ready"`
	NextAction  string               `json:"next_action,omitempty"`
}

type keycloakSwitchReadinessInspector interface {
	InspectKeycloakSwitchReadiness(context.Context, string, string, string) (KeycloakSwitchReadiness, error)
}

// keycloakSwitchReadinessUpdater 刻意比校验器更窄：只有服务端同步成功
// 才能打开其前两道门禁。
type keycloakSwitchReadinessUpdater interface {
	MarkKeycloakClientAndRoleCatalogSynced(context.Context, string, string, string) error
}

// keycloakBrokerLoginVerificationRecorder 持久化由当前已认证会话的校验端点
// 提供的证据，供切换门禁审计。
type keycloakBrokerLoginVerificationRecorder interface {
	RecordBrokerLoginVerification(context.Context, KeycloakBrokerLoginVerification) error
}

// keycloakProjectionOperations 把死信管理收敛到 Keycloak 授权应用服务之后。
// 仅在组合根配置；接触不到任何 Secret、Keycloak Admin 凭据或任意队列变更方法。
type keycloakProjectionOperations interface {
	ListFailed(context.Context, string, projectionapplication.FailurePageRequest) (projectionapplication.FailurePageResult, error)
	AlertStatus(context.Context, string) (projectionapplication.AlertStatus, error)
	Replay(context.Context, projectionapplication.ReplayInput) (projectionapplication.ReplayResult, error)
}

// KeycloakBrokerLoginVerification 是一次 Broker 登录验证的持久化证据：
// 记录被验证的目标环境、身份、Issuer、Client 与验证者会话，供切换门禁审计；
// 不含任何令牌或 Secret。
type KeycloakBrokerLoginVerification struct {
	TenantID, ApplicationID, EnvironmentID string
	IdentityID, Issuer, ClientID           string
	VerifiedByID, SessionID                string
}

type subsystemProvisioningCapabilityProvider interface {
	Capabilities() application.SubsystemProvisioningCapabilities
}

type subsystemAvailableCapabilityProvider interface {
	AvailableCapabilities(context.Context) (application.SubsystemProvisioningCapabilities, error)
}

type subsystemCandidateDiscoverer interface {
	DiscoverSubsystemCandidates(context.Context) ([]application.SubsystemDiscoveryCandidate, error)
}

// subsystemInitialAccessManager 把子系统专属授权放在应用登记之外管理。
// 角色编码为空表示该已登记子系统还没有受管角色目录。
type subsystemInitialAccessManager interface {
	AssignInitialAdministrator(context.Context, string, string, string, string) (string, error)
}

// keycloakBrokerProvisioner 创建 Keycloak 作为上游 broker 使用的唯一专用平台
// OAuth 客户端；其 Secret 绝不经 HTTP 暴露。
type keycloakBrokerProvisioner interface {
	EnsureKeycloakBroker(context.Context, string) (string, string, error)
	RecoverKeycloakBroker(context.Context, string, string) (string, string, error)
}

// keycloakControlPlaneOperations 只包含子系统生命周期用到的 Keycloak 管理动作。
// 保持该边界显式，使恢复行为无需真实 Keycloak 服务即可测试。
type keycloakControlPlaneOperations interface {
	EnsureBroker(context.Context, string, string) error
	VerifyBrokerExists(context.Context) error
	EnsureClient(context.Context, string, string, string) (keycloakClientResult, error)
	EnsureClientRoles(context.Context, string, []string) error
	DetectSubsystemKeycloakDrift(context.Context, string, string, []string) (KeycloakDriftReport, error)
}

// subsystemServiceCredentialManager 是窄口径的控制面能力，用于为已登记环境回填
// 新引入的机器绑定。明文只为新建/轮换的 Secret 生成，直接交给隔离部署 Agent；
// 绝不返回浏览器。
type subsystemServiceCredentialManager interface {
	ListOAuthClients(context.Context, string) ([]application.OAuthClientView, error)
	CreateOAuthClient(context.Context, application.OAuthClientCreateInput) (application.OAuthClientCreateResult, error)
	CreateOAuthClientSecret(context.Context, application.OAuthClientSecretCreateInput) (application.OAuthClientSecretResult, error)
	ReplaceOAuthClientScopes(context.Context, application.OAuthClientScopesUpdateInput) (application.OAuthClientView, error)
}

// subsystemNotificationSink 发送租户内站内通知，用于把子系统接入生命周期结果通知给操作人。
// 该接口由基础平台实现，子系统代码无需改动；nil 时处理器保持轻量测试可用。
type subsystemNotificationSink interface {
	SendSubsystemLifecycle(ctx context.Context, input SubsystemLifecycleNotification) error
}

// SubsystemLifecycleNotification 描述一次子系统接入/重试/更新的结果通知，供装配层适配器使用。
type SubsystemLifecycleNotification struct {
	TenantID        string
	OperatorID      string
	ApplicationName string
	ApplicationCode string
	Environment     string
	Succeeded       bool
	Detail          string
}

// SubsystemOnboardingHandler 暴露简化接入工作流与已认证的门户目录。
// 所配置的 OIDC Issuer 仅由隔离部署工作流使用，绝不与生成的凭据或基础设施
// 命令一起返回给浏览器。
type SubsystemOnboardingHandler struct {
	service                subsystemOnboardingService
	provisioner            application.SubsystemProvisioner
	access                 subsystemInitialAccessManager
	deploymentState        application.SubsystemDeploymentStateStore
	notifications          subsystemNotificationSink
	oidcIssuer             string
	keycloakIssuer         string
	keycloakRealm          string
	keycloakEnabled        bool
	keycloakRequireHTTPS   bool
	defaultIssuerAlias     string
	keycloakControl        keycloakControlPlaneOperations
	keycloakBroker         keycloakBrokerProvisioner
	keycloakCatalog        keycloakAuthorizationCatalog
	keycloakMappings       keycloakClientMappingStore
	keycloakReadiness      keycloakSwitchReadinessInspector
	keycloakOperations     keycloakProjectionOperations
	keycloakCutover        keycloakCutoverLifecycleStore
	serviceCredentials     subsystemServiceCredentialManager
	runtimeLicenses        *application.RuntimeLicenseEnrollmentService
	runtimeLicenseSettings func(context.Context) (application.RuntimeLicenseSettings, error)
	logger                 *slog.Logger
	// egress 是健康探针出网策略（SEC-B5），与反向代理共用 application.EgressPolicy。
	egress *application.EgressPolicy
	// deploymentJobs 跟踪在途的后台部署编排；waitForDeploymentJobs 供测试确定性收口。
	deploymentJobs sync.WaitGroup
	// deploymentTimeout 覆盖后台编排窗口；零值用 subsystemDeploymentAsyncTimeout。测试注入
	// 短窗口时必须走该字段（包级变量会被并行测试读成数据竞争）。
	deploymentTimeout time.Duration
}

const (
	keycloakObservationWindow = 7 * 24 * time.Hour
	keycloakRollbackWindow    = 7 * 24 * time.Hour
)

// ConfigureKeycloak 把 Keycloak 启用为显式部署提供方。凭据只在 API 与部署 Agent
// 之间流转；该配置仅承载公开状态信息。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloak(enabled bool, publicIssuer, realm string) {
	handler.keycloakEnabled = enabled
	handler.keycloakIssuer = strings.TrimRight(strings.TrimSpace(publicIssuer), "/")
	handler.keycloakRealm = strings.TrimSpace(realm)
}

// ConfigureKeycloakTransportPolicy 控制未来切换是否强制 HTTPS。false 刻意兼容
// 当前 HTTP 部署；preflight 仍由公开 origin 推导 Redirect URI 与安全 Cookie，
// 使三项设置不会漂移。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakTransportPolicy(requireHTTPS bool) {
	handler.keycloakRequireHTTPS = requireHTTPS
}

// ConfigureDefaultIssuerAlias 控制接入请求缺省 issuer_alias 时使用的提供方；
// 用户的显式选择始终优先于该默认值。
func (handler *SubsystemOnboardingHandler) ConfigureDefaultIssuerAlias(alias string) {
	handler.defaultIssuerAlias = strings.ToLower(strings.TrimSpace(alias))
}

// ConfigureKeycloakControlPlane 仅允许组合根调用；浏览器永远拿不到该对象
// 及其 Keycloak 管理凭据。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakControlPlane(control keycloakControlPlaneOperations) {
	handler.keycloakControl = control
}

// ConfigureKeycloakBroker 挂接 Keycloak broker 专用 OAuth 客户端的创建与恢复能力。
// 仅组合根可调用；未挂接时 Keycloak 同步与切换一律不可用（失败关闭）。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakBroker(provisioner keycloakBrokerProvisioner) {
	handler.keycloakBroker = provisioner
}

// reconcileKeycloakBroker 保持 Keycloak IdP 与其平台 OAuth 客户端一致。
// 既有有效凭据刻意没有可取回的明文，因此 IdP 健康时只做核验；
// 只有 Keycloak 确定性地报告 IdP 配置不完整时才生成新 Secret——
// 连接、认证与权限错误保持失败关闭，绝不触发凭据轮换。
func (handler *SubsystemOnboardingHandler) reconcileKeycloakBroker(ctx context.Context, tenantID, operatorID string) error {
	brokerID, brokerSecret, err := handler.keycloakBroker.EnsureKeycloakBroker(ctx, tenantID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(brokerSecret) != "" {
		return handler.keycloakControl.EnsureBroker(ctx, brokerID, brokerSecret)
	}

	err = handler.keycloakControl.VerifyBrokerExists(ctx)
	if err == nil || !IsRecoverableKeycloakBrokerConfigurationError(err) {
		return err
	}

	// 存储值是单向哈希，无法安全还原。改经 OAuth 客户端服务轮换，
	// 再用一次性明文结果立即调和 IdP。全程不记录、不返回任何 Secret。
	brokerID, brokerSecret, err = handler.keycloakBroker.RecoverKeycloakBroker(ctx, tenantID, operatorID)
	if err != nil {
		return fmt.Errorf("rotate broker credential after incomplete IdP config: %w", err)
	}
	if strings.TrimSpace(brokerSecret) == "" {
		return errors.New("broker credential recovery returned an empty secret")
	}
	return handler.keycloakControl.EnsureBroker(ctx, brokerID, brokerSecret)
}

// ConfigureKeycloakAuthorizationCatalog 挂接应用角色目录的读取来源；
// 未挂接时 Client 同步与 Keycloak 投影不可用。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakAuthorizationCatalog(catalog keycloakAuthorizationCatalog) {
	handler.keycloakCatalog = catalog
}

// ConfigureKeycloakClientMappingStore 挂接 Client 映射持久化与授权投影回填；
// 未挂接时同步请求按校验失败拒绝，不产生任何 Keycloak 侧写入。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakClientMappingStore(store keycloakClientMappingStore) {
	handler.keycloakMappings = store
}

// ConfigureKeycloakSwitchReadinessInspector 挂接权威的服务端投影与 broker 登录
// 证据来源。为向后兼容做成可选；未挂接时所有 Keycloak 切换保持关闭。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakSwitchReadinessInspector(inspector keycloakSwitchReadinessInspector) {
	handler.keycloakReadiness = inspector
}

// ConfigureKeycloakProjectionOperations 启用租户范围内的 FAILED 投影检查与受控重放。
// 传 nil 时运维端点刻意保持不可用，而不是暴露一条不完整的恢复路径。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakProjectionOperations(operations keycloakProjectionOperations) {
	handler.keycloakOperations = operations
}

// ConfigureKeycloakCutoverLifecycle 挂接持久化的按环境七天观察期与回滚窗口控制面。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakCutoverLifecycle(store keycloakCutoverLifecycleStore) {
	handler.keycloakCutover = store
}

// ConfigureSubsystemServiceCredentials 为旧版本创建的环境启用新用途绑定机器凭据的
// 幂等迁移。
func (handler *SubsystemOnboardingHandler) ConfigureSubsystemServiceCredentials(manager subsystemServiceCredentialManager) {
	handler.serviceCredentials = manager
}

// 仅组合根可启用商业授权登记；普通 HTTP 请求无法提供组件审批或另选凭据来源。
func (handler *SubsystemOnboardingHandler) ConfigureRuntimeLicenseEnrollment(service *application.RuntimeLicenseEnrollmentService) {
	handler.runtimeLicenses = service
}

// ConfigureRuntimeLicenseSettings 挂接运行时授权所需的实例级设置读取。仅组合根可
// 调用；商业授权登记已启用而该读取缺失或失败时，部署编排按
// LICENSE_RUNTIME_BINDING_FAILED 收口失败，绝不无设置放行。
func (handler *SubsystemOnboardingHandler) ConfigureRuntimeLicenseSettings(provider func(context.Context) (application.RuntimeLicenseSettings, error)) {
	handler.runtimeLicenseSettings = provider
}

func unverifiedKeycloakSwitchReadiness() KeycloakSwitchReadiness {
	return KeycloakSwitchReadiness{Gates: []KeycloakSwitchGate{
		{Key: "client_ready", Label: "Client 已就绪", Detail: "尚未针对当前应用环境验证 Realm Client。", NextAction: "先执行“同步 Keycloak”。"},
		{Key: "role_catalog_synced", Label: "角色目录已同步", Detail: "尚未针对当前应用环境验证角色目录。", NextAction: "同步 Keycloak 后检查角色目录。"},
		{Key: "user_projection_completed", Label: "用户投影已完成", Detail: "服务器没有可审计的用户投影完成证据。", NextAction: "完成用户投影，并接入服务器端状态回传。"},
		{Key: "broker_login_verified", Label: "Broker 登录验证已通过", Detail: "服务器没有可审计的 Broker 登录验证证据。", NextAction: "使用目标应用完成一次 Broker 登录验证，并接入服务器端状态回传。"},
	}, NextAction: "四项门禁均需由服务器验证后才能切换 Issuer。"}
}

func keycloakSyncReadiness() KeycloakSwitchReadiness {
	readiness := unverifiedKeycloakSwitchReadiness()
	readiness.Gates[0] = KeycloakSwitchGate{Key: "client_ready", Label: "Client 已就绪", Passed: true, Detail: "本次同步已成功创建或更新 Realm Client。"}
	readiness.Gates[1] = KeycloakSwitchGate{Key: "role_catalog_synced", Label: "角色目录已同步", Passed: true, Detail: "本次同步已成功写入当前应用角色目录。"}
	return readiness
}

func (handler *SubsystemOnboardingHandler) keycloakSwitchReadiness(ctx context.Context, tenantID, applicationCode, environment string) KeycloakSwitchReadiness {
	if handler.keycloakReadiness == nil {
		return unverifiedKeycloakSwitchReadiness()
	}
	readiness, err := handler.keycloakReadiness.InspectKeycloakSwitchReadiness(ctx, tenantID, applicationCode, environment)
	if err != nil || len(readiness.Gates) == 0 {
		return unverifiedKeycloakSwitchReadiness()
	}
	required := map[string]bool{"client_ready": false, "role_catalog_synced": false, "user_projection_completed": false, "broker_login_verified": false}
	for _, gate := range readiness.Gates {
		if _, requiredGate := required[gate.Key]; requiredGate {
			required[gate.Key] = gate.Passed
		}
		if !gate.Passed {
			readiness.SwitchReady = false
		}
	}
	for _, passed := range required {
		if !passed {
			readiness.SwitchReady = false
			return readiness
		}
	}
	readiness.SwitchReady = true
	return readiness
}

func writeKeycloakSwitchBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, readiness KeycloakSwitchReadiness) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_AUTH_PROVIDER_SWITCH_NOT_READY",
		"认证提供方尚未满足切换门禁，Issuer 未下发到运行时",
		map[string]any{"switch_gates": readiness.Gates, "next_action": readiness.NextAction},
	))
}

// writeKeycloakObservationBlocked 以稳定错误码回应观察期门禁失败。安全（SEC-B3）：
// 底层错误原文（可能含表名、SQL state、文件路径）只允许进入结构化日志，HTTP details
// 仅暴露稳定 reason_code 与本地化提示，遵守 httperror 的客户端安全契约。
func writeKeycloakObservationBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, reasonCode string) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_AUTH_PROVIDER_SWITCH_OBSERVATION_REQUIRED",
		"认证提供方尚未完成七天观察期，Issuer 未下发到运行时",
		map[string]any{"reason_code": reasonCode, "observation_window_days": 7, "next_action": "在 Keycloak 认证接入中发起观察；观察期完成后再切换。"},
	))
}

// keycloakObservationReasonCode 把观察期门禁错误归类为稳定代码；错误原文不出本函数。
func keycloakObservationReasonCode(err error) string {
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
	case strings.Contains(message, "duration must be positive"):
		return "OBSERVATION_INVALID_DURATION"
	default:
		// 其余原因可能是底层 DB/GORM 错误的包装（SEC-B3），绝不透出原文。
		return "OBSERVATION_STATE_UNAVAILABLE"
	}
}

func (handler *SubsystemOnboardingHandler) keycloakCutoverRequired(ctx context.Context, tenantID, applicationCode, environment string) bool {
	// 常规装配下环境聚合由接入服务持有；保留状态存储回退，兼容把该可选查询
	// 暴露在状态存储上的旧装配。
	for _, candidate := range []any{handler.service, handler.deploymentState} {
		resolver, ok := candidate.(subsystemEnvironmentIssuerResolver)
		if !ok {
			continue
		}
		currentAlias, err := resolver.ResolveEnvironmentIssuerAlias(ctx, tenantID, applicationCode, environment)
		if err == nil {
			return !strings.EqualFold(strings.TrimSpace(currentAlias), "keycloak")
		}
	}
	// 未知状态必须保持失败关闭：生产装配注入持久化 resolver，
	// 轻量调用方/测试沿用"首次切换即切换"规则。
	return true
}

func (handler *SubsystemOnboardingHandler) issuerForAlias(alias string) (string, error) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" && handler.defaultIssuerAlias != "" {
		alias = handler.defaultIssuerAlias
	}
	switch alias {
	case "", "platform", "basic_platform":
		return handler.oidcIssuer, nil
	case "keycloak":
		if !handler.keycloakEnabled || handler.keycloakIssuer == "" || handler.keycloakRealm == "" {
			return "", application.ErrValidation
		}
		return handler.keycloakIssuer, nil
	default:
		return "", application.ErrValidation
	}
}

func (handler *SubsystemOnboardingHandler) effectiveIssuerAlias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if alias == "" {
		alias = handler.defaultIssuerAlias
	}
	if alias == "" || alias == "basic_platform" {
		alias = "platform"
	}
	return alias
}

// 普通重建保留环境已持久化的提供方；更换提供方只能走专用的、带门禁的
// 切换/回滚操作。
func (handler *SubsystemOnboardingHandler) deploymentIssuerAlias(ctx context.Context, tenant, app, env, requested, forced string) (string, error) {
	if forced != "" {
		alias := handler.effectiveIssuerAlias(forced)
		if _, err := handler.issuerForAlias(alias); err != nil {
			return "", err
		}
		return alias, nil
	}
	for _, candidate := range []any{handler.service, handler.deploymentState} {
		resolver, ok := candidate.(subsystemEnvironmentIssuerResolver)
		if !ok {
			continue
		}
		persisted, err := resolver.ResolveEnvironmentIssuerAlias(ctx, tenant, app, env)
		if err != nil {
			return "", err
		}
		alias := strings.ToLower(strings.TrimSpace(persisted))
		// 历史持久化的 NULL/空表示平台 Issuer，而不是当前接入默认值
		// （默认值可能已改为 Keycloak）。
		if alias == "" || alias == "basic_platform" {
			alias = "platform"
		}
		if _, err = handler.issuerForAlias(alias); err != nil {
			return "", err
		}
		if strings.TrimSpace(requested) != "" && handler.effectiveIssuerAlias(requested) != alias {
			return "", application.ErrValidation
		}
		return alias, nil
	}
	// 轻量遗留组合没有持久化 resolver（生产装配必须提供）：这里按请求别名解析，
	// 遗留调用方的既有切换门禁保持不变。
	alias := handler.effectiveIssuerAlias(requested)
	if _, err := handler.issuerForAlias(alias); err != nil {
		return "", err
	}
	return alias, nil
}

// NewSubsystemOnboardingHandler 构造子系统接入 HTTP 适配器。可选状态存储保持
// 轻量调用方/测试兼容；生产装配传入持久化控制面仓储。
func NewSubsystemOnboardingHandler(service subsystemOnboardingService, provisioner application.SubsystemProvisioner, access subsystemInitialAccessManager, oidcIssuer string, logger *slog.Logger, stateStores ...application.SubsystemDeploymentStateStore) (*SubsystemOnboardingHandler, error) {
	return NewSubsystemOnboardingHandlerWithNotifications(service, provisioner, access, oidcIssuer, logger, nil, stateStores...)
}

// NewSubsystemOnboardingHandlerWithNotifications 额外注入站内通知发送器（可选）。
func NewSubsystemOnboardingHandlerWithNotifications(service subsystemOnboardingService, provisioner application.SubsystemProvisioner, access subsystemInitialAccessManager, oidcIssuer string, logger *slog.Logger, notifications subsystemNotificationSink, stateStores ...application.SubsystemDeploymentStateStore) (*SubsystemOnboardingHandler, error) {
	if service == nil || provisioner == nil || access == nil || strings.TrimSpace(oidcIssuer) == "" {
		return nil, errors.New("subsystem onboarding service, provisioner, access manager and OIDC issuer are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var deploymentState application.SubsystemDeploymentStateStore
	if len(stateStores) > 0 {
		deploymentState = stateStores[0]
	}
	return &SubsystemOnboardingHandler{
		service: service, provisioner: provisioner, access: access, deploymentState: deploymentState,
		notifications: notifications,
		oidcIssuer:    strings.TrimRight(strings.TrimSpace(oidcIssuer), "/"), logger: logger,
		egress: application.EgressPolicyFromEnv(),
	}, nil
}

// DiscoverSubsystemCandidates 仅暴露租户内尚未登记容器的自愿申报 Docker 标签元数据。
// 已登记判定按应用身份匹配，既不按候选自报的环境，也不按当前用户的门户可见性。
// 本端点绝不执行部署命令，因此可以独立于接入权限、仅授予应用读权限。
func (handler *SubsystemOnboardingHandler) DiscoverSubsystemCandidates(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	discoverer, ok := handler.provisioner.(subsystemCandidateDiscoverer)
	if !ok {
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	candidates, err := discoverer.DiscoverSubsystemCandidates(request.Context())
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	registered, err := handler.service.ListRegisteredApplicationCodes(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	existing := make(map[string]struct{}, len(registered))
	for _, code := range registered {
		existing[strings.ToLower(strings.TrimSpace(code))] = struct{}{}
	}
	result := make([]application.SubsystemDiscoveryCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate.ApplicationCode))
		if _, found := existing[key]; !found {
			result = append(result, candidate)
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "发现未登记子系统成功", result)
}

// allowedServiceBindingsForTarget 返回服务器清单为该应用/环境声明的服务用途白名单；
// 未声明时返回 nil，应用层回退到平台硬编码默认。
func (handler *SubsystemOnboardingHandler) allowedServiceBindingsForTarget(applicationCode, environment string) []string {
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		for _, target := range provider.Capabilities().Targets {
			if target.ApplicationCode == strings.TrimSpace(applicationCode) && target.Environment == strings.TrimSpace(environment) {
				if target.AllowedServiceBindings == nil {
					return nil
				}
				return append([]string(nil), target.AllowedServiceBindings...)
			}
		}
	}
	return nil
}

func (handler *SubsystemOnboardingHandler) manifestChecksumForTarget(applicationCode, environment string) string {
	provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider)
	if !ok {
		return ""
	}
	for _, target := range provider.Capabilities().Targets {
		if strings.EqualFold(target.ApplicationCode, strings.TrimSpace(applicationCode)) && strings.EqualFold(target.Environment, strings.TrimSpace(environment)) {
			return strings.TrimSpace(target.ManifestChecksum)
		}
	}
	return ""
}

func (handler *SubsystemOnboardingHandler) setDesiredManifest(ctx context.Context, tenantID, applicationCode, environment, checksum string) error {
	if strings.TrimSpace(checksum) == "" {
		return nil
	}
	store, ok := handler.deploymentState.(application.SubsystemManifestStateStore)
	if !ok {
		return nil
	}
	return store.SetSubsystemDesiredManifest(ctx, tenantID, applicationCode, environment, checksum, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markManifestApplied(ctx context.Context, tenantID, applicationCode, environment, checksum string) error {
	if strings.TrimSpace(checksum) == "" {
		return nil
	}
	store, ok := handler.deploymentState.(application.SubsystemManifestStateStore)
	if !ok {
		return nil
	}
	return store.MarkSubsystemManifestApplied(ctx, tenantID, applicationCode, environment, checksum, time.Now().UTC())
}

// notifySubsystemLifecycle 在接入/重试/更新成功或失败时向操作人发送站内通知；通知失败只记日志，
// 不阻断接入结果（接入本身是否成功由控制面状态决定）。
func (handler *SubsystemOnboardingHandler) notifySubsystemLifecycle(ctx context.Context, tenantID, operatorID, applicationName, applicationCode, environment string, succeeded bool, detail string) {
	if handler.notifications == nil || strings.TrimSpace(operatorID) == "" {
		return
	}
	// 安全（SEC-B9）：入库前最后一道防线——即使调用方误传错误原文，含 SQL 关键词
	// 或文件路径的通知详情也只保留稳定错误码（或占位符），完整 cause 只进结构化日志。
	detail = sanitizeLifecycleNotificationDetail(detail)
	if err := handler.notifications.SendSubsystemLifecycle(ctx, SubsystemLifecycleNotification{
		TenantID: tenantID, OperatorID: operatorID, ApplicationName: applicationName,
		ApplicationCode: applicationCode, Environment: environment, Succeeded: succeeded, Detail: detail,
	}); err != nil {
		handler.logger.Warn("subsystem lifecycle notification failed", "application_code", applicationCode, "environment", environment, "error", err)
	}
}

type subsystemOnboardingRequest struct {
	ApplicationCode string  `json:"application_code"`
	ApplicationName string  `json:"application_name"`
	Description     *string `json:"description"`
	Environment     string  `json:"environment"`
	PublicBaseURL   string  `json:"public_base_url"`
	UpstreamURL     string  `json:"upstream_url"`
	PathPrefix      string  `json:"path_prefix"`
	ClientType      string  `json:"client_type"`
	InitialAdminID  string  `json:"initial_admin_user_id"`
	IssuerAlias     string  `json:"issuer_alias"`
}

// subsystemDirectoryRegistrationRequest 只含目录与网关元数据。刻意排除 OAuth 客户端
// 创建：Keycloak 场景在本登记成功后经 /keycloak-integration/sync 完成。
type subsystemDirectoryRegistrationRequest struct {
	ApplicationCode string  `json:"application_code"`
	ApplicationName string  `json:"application_name"`
	Description     *string `json:"description"`
	Environment     string  `json:"environment"`
	PublicBaseURL   string  `json:"public_base_url"`
	UpstreamURL     string  `json:"upstream_url"`
	PathPrefix      string  `json:"path_prefix"`
	IssuerAlias     string  `json:"issuer_alias"`
}

// subsystemLifecycleRequest 是 Update 与 Teardown 共用的载荷。Update 可携带已持久化的
// 公开网关字段，让 Agent 重放非机密运行时值；Teardown 忽略它们。
type subsystemLifecycleRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	PublicBaseURL   string `json:"public_base_url"`
	UpstreamURL     string `json:"upstream_url"`
	PathPrefix      string `json:"path_prefix"`
	IssuerAlias     string `json:"issuer_alias"`
}

type keycloakClientSyncResponse struct {
	Alias               string               `json:"alias"`
	Realm               string               `json:"realm"`
	ClientID            string               `json:"client_id"`
	CanonicalClientID   string               `json:"canonical_client_id,omitempty"`
	ClientIDSource      string               `json:"client_id_source,omitempty"`
	LegacyCompatibility bool                 `json:"legacy_compatibility,omitempty"`
	ClaimsState         string               `json:"claims_state"`
	SwitchReady         bool                 `json:"switch_ready"`
	SwitchGates         []KeycloakSwitchGate `json:"switch_gates"`
	NextAction          string               `json:"next_action,omitempty"`
}

// keycloakBrokerLoginVerificationRequest 刻意不是通用回调断言：当前已认证会话
// 必须为自身身份、精确的应用环境与 Keycloak 客户端作证。
type keycloakBrokerLoginVerificationRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	IdentityID      string `json:"identity_id"`
	Issuer          string `json:"issuer"`
	ClientID        string `json:"client_id"`
}

type subsystemAutomationResponse struct {
	Status    string `json:"status"`
	PublicURL string `json:"public_url"`
}

type subsystemOnboardingResponse struct {
	Application   applicationResponse             `json:"application"`
	Environment   environmentResponse             `json:"environment"`
	LoginTarget   loginTargetResponse             `json:"login_target"`
	OAuthClient   oauthClientResponse             `json:"oauth_client"`
	Automation    subsystemAutomationResponse     `json:"automation"`
	Authorization *subsystemAuthorizationResponse `json:"authorization,omitempty"`
}

type subsystemDirectoryRegistrationResponse struct {
	Application applicationResponse `json:"application"`
	Environment environmentResponse `json:"environment"`
	LoginTarget loginTargetResponse `json:"login_target"`
	NextAction  string              `json:"next_action"`
}

type subsystemAuthorizationResponse struct {
	InitialAdminUserID string `json:"initial_admin_user_id"`
	RoleCode           string `json:"role_code"`
}

type portalApplicationResponse struct {
	ApplicationID        string  `json:"application_id"`
	Code                 string  `json:"code"`
	Name                 string  `json:"name"`
	Description          *string `json:"description"`
	EnvironmentID        string  `json:"environment_id"`
	Environment          string  `json:"environment"`
	PathPrefix           *string `json:"path_prefix"`
	TargetCode           string  `json:"target_code"`
	TargetURI            string  `json:"target_uri"`
	PublicURL            string  `json:"public_url"`
	Allowed              bool    `json:"allowed"`
	ProjectionStatus     string  `json:"projection_status"`
	ProjectionReady      bool    `json:"projection_ready"`
	ProjectionNextAction string  `json:"projection_next_action,omitempty"`
}

type subsystemDeploymentStateResponse struct {
	ApplicationCode         string     `json:"application_code"`
	Environment             string     `json:"environment"`
	Status                  string     `json:"status"`
	Operation               string     `json:"operation"`
	Generation              uint64     `json:"generation"`
	AttemptCount            uint       `json:"attempt_count"`
	DesiredManifestChecksum string     `json:"desired_manifest_checksum,omitempty"`
	AppliedManifestChecksum string     `json:"applied_manifest_checksum,omitempty"`
	ManifestDriftStatus     string     `json:"manifest_drift_status"`
	ManifestLastAppliedAt   *time.Time `json:"manifest_last_applied_at,omitempty"`
	ManifestLastVerifiedAt  *time.Time `json:"manifest_last_verified_at,omitempty"`
	LastErrorCode           string     `json:"last_error_code,omitempty"`
	LastError               string     `json:"last_error,omitempty"`
	NextAction              string     `json:"next_action,omitempty"`
	StartedAt               *time.Time `json:"started_at,omitempty"`
	CompletedAt             *time.Time `json:"completed_at,omitempty"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

type subsystemOnboardingDefaults struct {
	ApplicationCode string `json:"application_code,omitempty"`
	ApplicationName string `json:"application_name,omitempty"`
	Description     string `json:"description,omitempty"`
	Environment     string `json:"environment"`
	PublicBaseURL   string `json:"public_base_url"`
	UpstreamURL     string `json:"upstream_url,omitempty"`
	PathPrefix      string `json:"path_prefix,omitempty"`
	ClientType      string `json:"client_type"`
}

type subsystemProvisioningTargetResponse struct {
	ApplicationCode  string `json:"application_code"`
	ApplicationName  string `json:"application_name"`
	Description      string `json:"description,omitempty"`
	Environment      string `json:"environment"`
	UpstreamURL      string `json:"upstream_url"`
	PathPrefix       string `json:"path_prefix"`
	ClientType       string `json:"client_type"`
	ManifestChecksum string `json:"manifest_checksum,omitempty"`
}

type subsystemProvisioningCapabilitiesResponse struct {
	AutomationEnabled         bool                                      `json:"automation_enabled"`
	DeploymentMode            string                                    `json:"deployment_mode"`
	SupportedApplicationCodes []string                                  `json:"supported_application_codes"`
	SupportedEnvironments     []string                                  `json:"supported_environments"`
	Targets                   []subsystemProvisioningTargetResponse     `json:"targets,omitempty"`
	Defaults                  subsystemOnboardingDefaults               `json:"defaults"`
	AuthenticationProviders   []subsystemAuthenticationProviderResponse `json:"authentication_providers"`
}

// subsystemAuthenticationProviderResponse 刻意非敏感，是提供方状态、切换可用性与
// 回滚的页面契约。
type subsystemAuthenticationProviderResponse struct {
	Alias       string               `json:"alias"`
	Name        string               `json:"name"`
	Issuer      string               `json:"issuer"`
	Realm       string               `json:"realm,omitempty"`
	Status      string               `json:"status"`
	SwitchReady bool                 `json:"switch_ready"`
	SwitchGates []KeycloakSwitchGate `json:"switch_gates,omitempty"`
	NextAction  string               `json:"next_action,omitempty"`
	Detail      string               `json:"detail,omitempty"`
}

// OnboardSubsystem 处理 POST /api/v1/subsystem-onboarding。
func (handler *SubsystemOnboardingHandler) OnboardSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemOnboardingRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	initialAdminUserID := strings.TrimSpace(payload.InitialAdminID)
	if initialAdminUserID == "" {
		initialAdminUserID = principal.User.ID
	}
	effectiveIssuerAlias := handler.effectiveIssuerAlias(payload.IssuerAlias)
	onboardingInput := application.SubsystemOnboardingInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, InitialAdminUserID: initialAdminUserID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, ClientType: payload.ClientType,
		IssuerAlias:            optionalIssuerAlias(effectiveIssuerAlias),
		AllowedServiceBindings: handler.allowedServiceBindingsForTarget(payload.ApplicationCode, payload.Environment),
	}
	if err := application.ValidateSubsystemOnboardingInput(onboardingInput); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.service.PreflightValidate(request.Context(), onboardingInput); err != nil {
		handler.writeError(writer, request, err, "preflight")
		return
	}
	issuer, err := handler.issuerForAlias(effectiveIssuerAlias)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Preflight(request.Context(), application.SubsystemPreflightInput{
		TenantID: principal.Tenant.ID, ApplicationCode: onboardingInput.ApplicationCode,
		Environment: onboardingInput.Environment, Issuer: issuer,
		ManifestChecksum: handler.manifestChecksumForTarget(onboardingInput.ApplicationCode, onboardingInput.Environment),
		PublicBaseURL:    onboardingInput.PublicBaseURL, UpstreamURL: onboardingInput.UpstreamURL,
		PathPrefix: onboardingInput.PathPrefix, ClientType: onboardingInput.ClientType,
	}); err != nil {
		handler.writeError(writer, request, err, "preflight")
		return
	}
	relatedClients, relatedErr := handler.relatedServiceClients(request.Context(), principal.Tenant.ID, onboardingInput.ApplicationCode, onboardingInput.Environment)
	if relatedErr != nil {
		handler.writeError(writer, request, relatedErr, "contract integration dependency")
		return
	}
	result, err := handler.service.OnboardSubsystem(request.Context(), onboardingInput)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	pathPrefix, upstreamURL := "", ""
	// Onboard 原子地创建 generation 1。即使在后续状态读取完成前请求被取消，
	// 也要保留该所有权标记，避免并发接管。
	generation := uint64(1)
	accepted := false
	defer func() {
		if !accepted {
			handler.markDeploymentFailureDetached(subsystemDeploymentJob{TenantID: principal.Tenant.ID, ApplicationCode: result.Application.Code, Environment: result.Environment.Environment, Operation: "ONBOARD", Generation: generation}, "DEPLOYMENT_PREPARATION_FAILED", "部署准备失败，请点击重试")
		}
	}()
	if result.Environment.PathPrefix != nil {
		pathPrefix = *result.Environment.PathPrefix
	}
	if result.Environment.UpstreamURL != nil {
		upstreamURL = *result.Environment.UpstreamURL
	}
	provisioningClientID, provisioningClientSecret := result.OAuthClient.ClientID, result.PlaintextSecret
	manifestChecksum := handler.manifestChecksumForTarget(result.Application.Code, result.Environment.Environment)
	if err := handler.setDesiredManifest(request.Context(), principal.Tenant.ID, result.Application.Code, result.Environment.Environment, manifestChecksum); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	keycloakClientID := ""
	if strings.EqualFold(effectiveIssuerAlias, "keycloak") {
		if handler.keycloakControl == nil || handler.keycloakBroker == nil {
			handler.writeError(writer, request, application.ErrValidation)
			return
		}
		if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), result.Application.Code+"-"+result.Environment.Environment+"-web", result.Application.Name+" "+result.Environment.Environment, result.RedirectURI)
		if clientErr != nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		provisioningClientID, provisioningClientSecret = client.ClientID, client.ClientSecret
		keycloakClientID = client.ClientID
	}
	// 装配逻辑提取到应用层（BuildSubsystemOnboardingProvisioningInput）：统一入口与旧路径共用同一份装配，
	// 避免两套实现漂移；凭据仍只在内存中传递，不落库、不回显。
	provisioningInput, err := application.BuildSubsystemOnboardingProvisioningInput(application.SubsystemOnboardingProvisioningAssembly{
		TenantID: principal.Tenant.ID, ApplicationID: result.Application.ID, EnvironmentID: result.Environment.ID,
		ApplicationCode: result.Application.Code, Environment: result.Environment.Environment,
		Issuer: issuer, AuthenticationProvider: effectiveIssuerAlias, ManifestChecksum: manifestChecksum,
		ClientID: provisioningClientID, ClientSecret: provisioningClientSecret,
		CatalogPublisherClientID:     result.CatalogPublisherOAuthClient.ClientID,
		CatalogPublisherClientSecret: result.CatalogPublisherPlaintextSecret,
		ServiceCredentials:           result.ServiceCredentials,
		RelatedServiceClients:        relatedClients,
		RedirectURI:                  result.RedirectURI, PublicURL: result.PublicURL,
		PathPrefix: pathPrefix, UpstreamURL: upstreamURL,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	handler.startSubsystemDeploymentJob(request, subsystemDeploymentJob{
		Operation:          "ONBOARD",
		Generation:         generation,
		TenantID:           principal.Tenant.ID,
		OperatorUserID:     principal.User.ID,
		ApplicationCode:    result.Application.Code,
		ApplicationName:    result.Application.Name,
		ApplicationID:      result.Application.ID,
		EnvironmentID:      result.Environment.ID,
		Environment:        result.Environment.Environment,
		Input:              provisioningInput,
		InitialAdminUserID: initialAdminUserID,
		KeycloakClientID:   keycloakClientID,
		ManifestChecksum:   manifestChecksum,
	})
	accepted = true
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusAccepted, "接入请求已受理，部署正在后台执行；请轮询部署状态", map[string]string{
		"status":           application.SubsystemDeploymentStatusUpdating,
		"operation":        "ONBOARD",
		"application_code": result.Application.Code,
		"environment":      result.Environment.Environment,
	})
	handler.logger.Info("subsystem onboarding accepted", "path", request.URL.Path,
		"application_code", result.Application.Code, "environment", result.Environment.Environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// RegisterSubsystemDirectory 处理 POST /api/v1/subsystem-directory。
//
// 这是 V2 登记入口：只登记应用目录、环境与登录目标数据。此处不创建任何平台
// OAuth Client、Keycloak Client、服务凭据、部署状态或初始角色授权。迁移期间
// 既有的一体化 /subsystem-onboarding 调用方继续受支持，但新的 Keycloak 集成
// 必须先走本端点，再使用专用 Keycloak 集成 API。
func (handler *SubsystemOnboardingHandler) RegisterSubsystemDirectory(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemDirectoryRegistrationRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	effectiveIssuerAlias := handler.effectiveIssuerAlias(payload.IssuerAlias)
	if _, err := handler.issuerForAlias(effectiveIssuerAlias); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	result, err := handler.service.RegisterSubsystemDirectory(request.Context(), application.SubsystemDirectoryRegistrationInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID,
		ApplicationCode: payload.ApplicationCode, ApplicationName: payload.ApplicationName,
		Description: payload.Description, Environment: payload.Environment,
		PublicBaseURL: payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL,
		PathPrefix: payload.PathPrefix, IssuerAlias: optionalIssuerAlias(effectiveIssuerAlias),
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "应用目录与登录目标已登记；请继续在 Keycloak 认证接入中同步 Client 并配置运行时", subsystemDirectoryRegistrationResponse{
		Application: applicationToResponse(result.Application),
		Environment: environmentToResponse(result.Environment),
		LoginTarget: loginTargetToResponse(result.LoginTarget),
		NextAction:  "继续执行 Keycloak Client 同步；完成后再应用子系统运行时配置。",
	})
}

// ListPortalApplications 处理 GET /api/v1/portal/applications。所有已认证用户可读
// 当前租户的活跃目录；该只读端点不要求管理权限。
func (handler *SubsystemOnboardingHandler) ListPortalApplications(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	// 本响应是用户级授权投影。若无显式缓存控制，账号切换后浏览器或反向代理可能
	// 继续返回上一个 bp_session 用户的模块列表，即使认证本身已正确解析新 Cookie。
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Add("Vary", "Cookie")
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, request.URL.Query().Get("environment"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	responses := make([]portalApplicationResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, portalApplicationResponse{
			ApplicationID: item.ApplicationID, Code: item.Code, Name: item.Name, Description: item.Description,
			EnvironmentID: item.EnvironmentID, Environment: item.Environment, PathPrefix: item.PathPrefix,
			TargetCode: item.TargetCode, TargetURI: item.TargetURI, PublicURL: item.PublicURL,
			Allowed: item.Allowed, ProjectionStatus: item.Projection.Status,
			ProjectionReady: item.Projection.Ready, ProjectionNextAction: item.Projection.NextAction,
		})
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "门户应用目录查询成功", responses)
}

// GetSubsystemCapabilities 暴露服务端配置的非敏感接入策略，供渲染管理表单；
// 权威校验仍由隔离部署 Agent 执行。
func (handler *SubsystemOnboardingHandler) GetSubsystemCapabilities(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	if _, ok := subsystemPrincipal(writer, request); !ok {
		return
	}
	capabilities := application.SubsystemProvisioningCapabilities{
		Enabled: false, Mode: "unknown", SupportedEnvironments: []string{},
	}
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		capabilities = provider.Capabilities()
	}
	if provider, ok := handler.provisioner.(subsystemAvailableCapabilityProvider); ok {
		available, err := provider.AvailableCapabilities(request.Context())
		if err != nil {
			handler.writeError(writer, request, err)
			return
		}
		capabilities = available
	}
	defaultEnvironment := strings.TrimSpace(capabilities.DefaultEnvironment)
	if defaultEnvironment == "" && len(capabilities.SupportedEnvironments) > 0 {
		defaultEnvironment = capabilities.SupportedEnvironments[0]
	}
	if defaultEnvironment == "" {
		defaultEnvironment = "dev"
	}
	defaultClientType := strings.TrimSpace(capabilities.DefaultClientType)
	if defaultClientType == "" {
		defaultClientType = "confidential"
	}
	targets := make([]subsystemProvisioningTargetResponse, 0, len(capabilities.Targets))
	for _, target := range capabilities.Targets {
		targets = append(targets, subsystemProvisioningTargetResponse{
			ApplicationCode:  target.ApplicationCode,
			ApplicationName:  target.ApplicationName,
			Description:      target.Description,
			Environment:      target.Environment,
			UpstreamURL:      target.UpstreamURL,
			PathPrefix:       target.PathPrefix,
			ClientType:       target.ClientType,
			ManifestChecksum: target.ManifestChecksum,
		})
	}
	response := subsystemProvisioningCapabilitiesResponse{
		AutomationEnabled:         capabilities.Enabled,
		DeploymentMode:            capabilities.Mode,
		SupportedApplicationCodes: append([]string(nil), capabilities.SupportedApplicationCodes...),
		SupportedEnvironments:     append([]string(nil), capabilities.SupportedEnvironments...),
		Targets:                   targets,
		Defaults: subsystemOnboardingDefaults{
			ApplicationCode: capabilities.DefaultApplicationCode,
			ApplicationName: capabilities.DefaultApplicationName,
			Description:     capabilities.DefaultDescription,
			Environment:     defaultEnvironment,
			PublicBaseURL:   handler.oidcIssuer,
			UpstreamURL:     capabilities.DefaultUpstreamURL,
			PathPrefix:      capabilities.DefaultPathPrefix,
			ClientType:      defaultClientType,
		},
		AuthenticationProviders: []subsystemAuthenticationProviderResponse{{
			Alias: "platform", Name: "基础平台 OIDC", Issuer: handler.oidcIssuer,
			Status: "READY", SwitchReady: true,
		}},
	}
	keycloak := subsystemAuthenticationProviderResponse{
		Alias: "keycloak", Name: "Keycloak", Issuer: handler.keycloakIssuer,
		Realm: handler.keycloakRealm, Status: "NOT_CONFIGURED",
		Detail:      "尚未由平台配置 Keycloak 管理连接和 Broker Client。",
		SwitchGates: unverifiedKeycloakSwitchReadiness().Gates,
		NextAction:  "完成 Keycloak 配置后，选择具体应用环境同步并验证四项门禁。",
	}
	if handler.keycloakEnabled && handler.keycloakControl != nil && handler.keycloakBroker != nil && handler.keycloakIssuer != "" && handler.keycloakRealm != "" {
		keycloak.Status = "READY"
		keycloak.Detail = "Keycloak 管理连接已就绪；仍需对具体应用环境完成四项门禁验证。"
		keycloak.NextAction = "先同步 Keycloak，再完成用户投影和 Broker 登录验证。"
	}
	response.AuthenticationProviders = append(response.AuthenticationProviders, keycloak)
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统部署能力查询成功", response)
}

// SubsystemHealthEntry 描述单个子系统环境的集成健康度。
type SubsystemHealthEntry struct {
	ApplicationCode   string `json:"application_code"`
	ApplicationName   string `json:"application_name"`
	Environment       string `json:"environment"`
	DirectoryOK       bool   `json:"directory_ok"`
	CredentialsOK     bool   `json:"credentials_ok"`
	RuntimeOK         bool   `json:"runtime_ok"`
	KeycloakOK        bool   `json:"keycloak_ok"`
	DirectoryStatus   string `json:"directory_status"`
	CredentialsStatus string `json:"credentials_status"`
	RuntimeStatus     string `json:"runtime_status"`
	KeycloakStatus    string `json:"keycloak_status"`
	ProjectionStatus  string `json:"projection_status,omitempty"`
	Status            string `json:"status"`
	NextAction        string `json:"next_action,omitempty"`
}

// GetSubsystemHealthDashboard 返回当前租户全部子系统的集成健康度，只读诊断端点。
func (handler *SubsystemOnboardingHandler) GetSubsystemHealthDashboard(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	items, err := handler.service.ListPortalApplications(request.Context(), principal.Tenant.ID, principal.User.ID, "")
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// 真实运行时探活：从仓储解析每个激活环境的容器内健康 URL，并发探测（单目标 2 秒
	// 超时）。仓储不支持解析（如测试桩）时退回 UNVERIFIED，而不是假报 READY。
	healthResults := map[string]bool{}
	if resolver, ok := handler.deploymentState.(application.SubsystemHealthTargetResolver); ok && handler.deploymentState != nil {
		if targets, err := resolver.ResolveSubsystemHealthTargets(request.Context(), principal.Tenant.ID); err == nil {
			var waitGroup sync.WaitGroup
			var resultsMutex sync.Mutex
			// 安全（SEC-B5）：探针与反向代理共用同一出网策略——先做 URL 层校验，
			// 客户端 Transport 再在建连前对解析出的 IP 复验；被拒绝的目标直接跳过，
			// 看板保持非 READY，绝不触达云元数据或未授权的回环/私网地址。
			egress := handler.egress
			if egress == nil {
				egress = application.EgressPolicyFromEnv()
			}
			client := egress.NewEgressHTTPClient(2 * time.Second)
			for _, target := range targets {
				if err := egress.ValidateURL(target.HealthURL); err != nil {
					handler.logger.Warn("subsystem health probe blocked by egress policy",
						"application_code", target.ApplicationCode, "environment", target.Environment, "error", err)
					continue
				}
				waitGroup.Add(1)
				go func(target application.SubsystemHealthTarget) {
					defer waitGroup.Done()
					probeRequest, err := stdhttp.NewRequest(stdhttp.MethodGet, target.HealthURL, nil)
					if err != nil {
						return
					}
					response, err := client.Do(probeRequest)
					if err != nil {
						return
					}
					defer response.Body.Close()
					resultsMutex.Lock()
					healthResults[target.ApplicationCode+":"+target.Environment] = response.StatusCode >= 200 && response.StatusCode < 400
					resultsMutex.Unlock()
				}(target)
			}
			waitGroup.Wait()
		} else {
			handler.logger.Warn("subsystem health targets unavailable", "tenant_id", principal.Tenant.ID, "error", err)
		}
	}
	entries := make([]SubsystemHealthEntry, 0, len(items))
	for _, item := range items {
		entry := SubsystemHealthEntry{
			ApplicationCode:   item.Code,
			ApplicationName:   item.Name,
			Environment:       item.Environment,
			DirectoryOK:       true,
			DirectoryStatus:   "READY",
			CredentialsStatus: "UNKNOWN",
			KeycloakStatus:    "UNKNOWN",
			ProjectionStatus:  strings.ToUpper(strings.TrimSpace(item.Projection.Status)),
			Status:            "VERIFICATION_REQUIRED",
			NextAction:        "调用当前环境同步状态接口，分别验证凭据交付和 Keycloak 控制面状态。",
		}
		if !handler.keycloakEnabled {
			entry.KeycloakStatus = "NOT_APPLICABLE"
		}
		switch runtimeHealthy, probed := healthResults[item.Code+":"+item.Environment]; {
		case probed && runtimeHealthy:
			entry.RuntimeOK = true
			entry.RuntimeStatus = "READY"
		case probed:
			entry.RuntimeOK = false
			entry.RuntimeStatus = "UNAVAILABLE"
			entry.Status = "RUNTIME_UNAVAILABLE"
			entry.NextAction = "子系统容器健康检查失败；请查看目标容器日志，修复后点击重试。"
		default:
			entry.RuntimeOK = false
			entry.RuntimeStatus = "UNVERIFIED"
			entry.NextAction = "无法探测子系统运行时（未注册 upstream 或未部署）；请确认部署状态。"
		}
		entries = append(entries, entry)
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统健康状态查询成功", entries)
}

// VerifyKeycloakBrokerLogin 从专用 Keycloak 用户 JWT 校验路由记录证据；
// 路由中间件先固定 Issuer 并验签，本 handler 再把令牌身份与受众绑定到
// 单个应用环境。
func (handler *SubsystemOnboardingHandler) VerifyKeycloakBrokerLogin(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	claims, ok := keycloakctx.BrokerClaimsFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	if strings.TrimSpace(claims.SessionID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload keycloakBrokerLoginVerificationRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	payload.ApplicationCode = strings.TrimSpace(payload.ApplicationCode)
	payload.Environment = strings.ToLower(strings.TrimSpace(payload.Environment))
	payload.IdentityID = strings.TrimSpace(payload.IdentityID)
	payload.Issuer = strings.TrimRight(strings.TrimSpace(payload.Issuer), "/")
	payload.ClientID = strings.TrimSpace(payload.ClientID)
	if payload.ApplicationCode == "" || payload.Environment == "" || payload.IdentityID == "" || payload.Issuer == "" || payload.ClientID == "" || payload.IdentityID != claims.IdentityID || !containsExactAudience(claims.Audience, payload.ClientID) {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	if !handler.keycloakEnabled || handler.keycloakIssuer == "" || payload.Issuer != claims.Issuer || !strings.EqualFold(payload.Issuer, handler.keycloakIssuer) {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, found := handler.resolveApplicationContextForIdentity(request, claims.TenantID, claims.IdentityID, payload.ApplicationCode, payload.Environment)
	if !found {
		// 不得泄露当前会话门户可见范围之外的目标是否存在。
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	recorder, ok := handler.keycloakReadiness.(keycloakBrokerLoginVerificationRecorder)
	if !ok {
		// 缺少持久化记录器属于失败关闭的部署配置。
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	if err := recorder.RecordBrokerLoginVerification(request.Context(), KeycloakBrokerLoginVerification{
		TenantID: claims.TenantID, ApplicationID: applicationID, EnvironmentID: environmentID,
		IdentityID: payload.IdentityID, Issuer: payload.Issuer, ClientID: payload.ClientID,
		VerifiedByID: claims.IdentityID, SessionID: claims.SessionID,
	}); err != nil {
		handler.logger.Warn("Keycloak broker login verification rejected", "application_code", payload.ApplicationCode, "environment", payload.Environment, "identity_id", payload.IdentityID, "error", err)
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak Broker 登录验证已记录；Issuer 仍受其余门禁保护", map[string]string{"status": "verified", "application_code": payload.ApplicationCode, "environment": payload.Environment})
}

func containsExactAudience(audience []string, expected string) bool {
	for _, value := range audience {
		if value == expected {
			return true
		}
	}
	return false
}

// SyncKeycloakClient 创建或更新所选子系统的 Keycloak RP 及其必需的 token claim mapper。
// 刻意与 Issuer 切换分离：Admin API 写入成功不能证明代理登录已被实测，
// 最终运行时切换保持失败关闭。
func (handler *SubsystemOnboardingHandler) SyncKeycloakClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	if _, ok := subsystemPrincipal(writer, request); !ok {
		return
	}
	if handler.keycloakControl == nil || handler.keycloakBroker == nil || !handler.keycloakEnabled {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	principal, _ := authctx.PrincipalFromContext(request.Context())
	applicationID, environmentID, found := handler.resolveApplicationContext(writer, request, payload.ApplicationCode, payload.Environment)
	if !found || handler.keycloakCatalog == nil || handler.keycloakMappings == nil {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "broker", err)
		return
	}
	canonicalClientID := strings.ToLower(strings.TrimSpace(payload.ApplicationCode)) + "-" + strings.ToLower(strings.TrimSpace(payload.Environment)) + "-web"
	resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
	if resolver, ok := handler.keycloakMappings.(keycloakClientCompatibilityResolver); ok {
		resolved, resolveErr := resolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID)
		if resolveErr != nil {
			handler.writeKeycloakSyncFailure(writer, request, payload, "client", resolveErr)
			return
		}
		if strings.TrimSpace(resolved.ClientID) != "" {
			resolution = resolved
		}
	}
	transport, transportErr := application.ValidateKeycloakCutoverTransport(payload.PublicBaseURL, payload.PathPrefix, handler.keycloakRequireHTTPS)
	if transportErr != nil {
		handler.writeError(writer, request, transportErr)
		return
	}
	redirectURI := transport.RedirectURI
	result, err := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, redirectURI)
	if err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "client", err)
		return
	}
	// 同步 Keycloak Client 不能只更新 Keycloak。授权上下文解析依赖平台自身的
	// platform_oauth_client 记录；旧环境如果只有 Keycloak Client，登录时会在
	// azp 校验阶段得到 403。先确认 Keycloak 写入成功，再以同一租户、应用和
	// 环境补齐平台 Web Client，避免失败时留下孤儿授权记录。
	if _, err := handler.ensureWebOAuthClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, principal.User.ID, result.ClientID, payload.ApplicationCode, redirectURI); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "oauth_client", err)
		return
	}
	roleCodes, err := handler.keycloakCatalog.ListKeycloakRoleCodes(request.Context(), principal.Tenant.ID, applicationID)
	if err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "roles", err)
		return
	}
	if err := handler.keycloakControl.EnsureClientRoles(request.Context(), result.ClientID, roleCodes); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "roles", err)
		return
	}
	if err := handler.keycloakMappings.SaveKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID, handler.keycloakRealm, result.ClientID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "mapping", err)
		return
	}
	if err := handler.keycloakMappings.BackfillKeycloakAuthorization(request.Context(), principal.Tenant.ID, applicationID, environmentID); err != nil {
		handler.writeKeycloakSyncFailure(writer, request, payload, "backfill", err)
		return
	}
	if updater, ok := handler.keycloakReadiness.(keycloakSwitchReadinessUpdater); ok {
		if err := updater.MarkKeycloakClientAndRoleCatalogSynced(request.Context(), principal.Tenant.ID, applicationID, environmentID); err != nil {
			handler.writeKeycloakSyncFailure(writer, request, payload, "readiness", err)
			return
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	// 上述 Admin API 调用只证明本次请求的前两道门禁。部署若配有权威校验器，
	// 应向其索取完整的环境级证据；否则保留部分结果，不自行放行。
	readiness := keycloakSyncReadiness()
	if handler.keycloakReadiness != nil {
		readiness = handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, payload.ApplicationCode, payload.Environment)
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak Realm Client、身份、组织、角色与权限 Claims 映射已同步；Issuer 仍被四项门禁保护", keycloakClientSyncResponse{Alias: "keycloak", Realm: handler.keycloakRealm, ClientID: result.ClientID, CanonicalClientID: resolution.CanonicalClientID, ClientIDSource: resolution.Source, LegacyCompatibility: resolution.LegacyCompatibility, ClaimsState: "MAPPERS_SYNCED", SwitchReady: readiness.SwitchReady, SwitchGates: readiness.Gates, NextAction: readiness.NextAction})
}

// ensureWebOAuthClient 确保平台授权上下文可以解析子系统使用的浏览器 Client。
// 已存在且绑定一致的 ACTIVE 记录直接复用；缺失记录使用授权码 + PKCE 创建。
// 明文密钥不向上层返回，避免同步接口泄露浏览器 Client 的运行时凭据。
func (handler *SubsystemOnboardingHandler) ensureWebOAuthClient(ctx context.Context, tenantID, applicationID, environmentID, operatorID, clientID, applicationCode, redirectURI string) (application.OAuthClientView, error) {
	if handler.serviceCredentials == nil {
		return application.OAuthClientView{}, application.ErrSubsystemProvisioningUnavailable
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
			return application.OAuthClientView{}, application.ErrConflict
		}
		return client, nil
	}
	created, err := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: clientID, ClientName: applicationCode + " Web", ClientType: "confidential",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
		RequirePKCE: true, GrantTypes: []string{"authorization_code"}, Scopes: []string{"openid", "profile"},
		RedirectURIs: []string{redirectURI},
	})
	if err != nil {
		return application.OAuthClientView{}, err
	}
	return created.Client, nil
}

// writeKeycloakSyncFailure 保持对外契约刻意非敏感，同时保留足够阶段信息让运维
// 选择正确的修复动作。原始错误只在服务端记录，并以请求 ID 关联。
func (handler *SubsystemOnboardingHandler) writeKeycloakSyncFailure(writer stdhttp.ResponseWriter, request *stdhttp.Request, payload subsystemLifecycleRequest, stage string, err error) {
	detail, nextAction := keycloakSyncFailureGuidance(stage)
	handler.logger.Warn("Keycloak Client synchronization failed",
		"request_id", requestctx.RequestID(request.Context()),
		"stage", stage,
		"application_code", payload.ApplicationCode,
		"environment", payload.Environment,
		"error", err,
	)
	httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.New(
		httperror.DependencyUnavailable.Code,
		httperror.DependencyUnavailable.Message,
		map[string]string{"stage": stage, "detail": detail, "next_action": nextAction},
	))
}

func keycloakSyncFailureGuidance(stage string) (detail, nextAction string) {
	switch stage {
	case "broker":
		return "Keycloak Broker 同步暂时不可用。", "检查 Keycloak 管理连接和平台 Broker 配置后，在当前环境重新同步。"
	case "client":
		return "Keycloak Realm Client 同步暂时不可用。", "检查目标 Realm、Client 配置与回调地址后，在当前环境重新同步。"
	case "oauth_client":
		return "平台 Web OAuth Client 同步暂时不可用。", "检查平台 OAuth Client 接入记录与租户应用环境关联后，在当前环境重新同步。"
	case "roles":
		return "Keycloak Client 角色目录同步暂时不可用。", "检查应用角色目录与 Keycloak Client Role 管理权限后，在当前环境重新同步。"
	case "mapping":
		return "Keycloak Client 映射保存暂时不可用。", "检查基础平台数据库与 Keycloak Client 映射状态后，在当前环境重新同步。"
	case "backfill":
		return "Keycloak 授权投影回填暂时不可用。", "检查基础平台数据库与授权投影队列后，在当前环境重新同步。"
	case "readiness":
		return "Keycloak 切换门禁状态保存暂时不可用。", "检查基础平台数据库与 Keycloak 就绪状态后，在当前环境重新同步。"
	default:
		return "Keycloak 同步暂时不可用。", "检查 Keycloak 管理连接后，在当前环境重新同步。"
	}
}

// UpdateSubsystem 处理 POST /api/v1/subsystem-update。调用方必须先经常规管理
// PATCH 端点更新数据库聚合（Environment 基址、OAuth redirect URI）；本端点只重跑
// provisioner，让运行中的子系统拿到新的 .env.local 值并重载门户网关。
func (handler *SubsystemOnboardingHandler) UpdateSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "", "UPDATE")
}

// AdoptSubsystem 把仅在目录中登记的环境纳入部署状态机。刻意与 update 分开：
// 调用方无法把未受管的运行时误当成"已部署"处理。
func (handler *SubsystemOnboardingHandler) AdoptSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "", "ADOPT")
}

// SwitchToKeycloak 是显式的认证集成切换动作。目标 Issuer 由服务端固定下发，
// 浏览器无法把一次 Keycloak 切换请求变成任意的提供方变更。
func (handler *SubsystemOnboardingHandler) SwitchToKeycloak(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "keycloak", "UPDATE")
}

// RollbackToPlatform 是显式的认证集成回滚动作。复用与旧版子系统更新相同的
// 受控部署工作流，同时把目标 Issuer 固定为 Basic Platform。
func (handler *SubsystemOnboardingHandler) RollbackToPlatform(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.updateSubsystem(writer, request, "platform", "UPDATE")
}

type updateServiceCredentialRequirement = application.SubsystemUpdateServiceCredentialRequirement

func updateServiceCredentialRequirements(applicationCode string) []updateServiceCredentialRequirement {
	// 需求表已提取到应用层（逐行复制）；这里转调，使既有用例成为等价性证明。
	return application.SubsystemUpdateServiceCredentialRequirements(applicationCode)
}

func (handler *SubsystemOnboardingHandler) ensureUpdateServiceCredentials(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID, operation string) ([]application.SubsystemServiceCredential, error) {
	requirements := updateServiceCredentialRequirements(applicationCode)
	if handler.serviceCredentials == nil || len(requirements) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(applicationID) == "" || strings.TrimSpace(environmentID) == "" {
		return nil, application.ErrNotFound
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	byClientID := make(map[string]application.OAuthClientView, len(clients))
	for _, client := range clients {
		byClientID[client.ClientID] = client
	}
	credentials := make([]application.SubsystemServiceCredential, 0, len(requirements))
	for _, requirement := range requirements {
		clientID := applicationCode + "-" + environment + "-" + requirement.Suffix
		if client, ok := byClientID[clientID]; ok {
			if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
				return nil, application.ErrConflict
			}
			if (p1ServicePurpose(requirement.Purpose) || applicationCode == "contract_management" && requirement.Purpose == application.ServiceCredentialNotificationIngest) &&
				(client.TenantID != tenantID || client.ClientType != "service" || client.TokenAuthMethod != "client_secret_basic" || !sameStringSet(client.GrantTypes, []string{"client_credentials"})) {
				return nil, application.ErrConflict
			}
			desiredScopes := serviceCredentialScopes(requirement)
			if !sameStringSet(client.Scopes, desiredScopes) {
				// 历史服务凭据可能在文件下载能力引入前创建。受控更新在签发新密钥前
				// 先整体修复 Scope，避免“密钥已下发但调用仍因旧权限 403”的半成功状态。
				client, err = handler.serviceCredentials.ReplaceOAuthClientScopes(ctx, application.OAuthClientScopesUpdateInput{
					TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID,
					Scopes: desiredScopes, Version: client.Version,
				})
				if err != nil {
					return nil, err
				}
			}
			// 接管与重试要生成可恢复的替换凭据：此前生成的 Secret 可能恰在 Agent 失败前
			// 刚刚签发。标记 rotate 的凭据在普通受控更新中同样会重新下发。
			if operation != "RETRY" && operation != "ADOPT" && !requirement.Rotate {
				if p1ServicePurpose(requirement.Purpose) {
					credentials = append(credentials, application.SubsystemServiceCredential{Purpose: requirement.Purpose, OAuthClient: client})
				}
				continue
			}
			var expectedVersion *uint64
			if p1ServicePurpose(requirement.Purpose) {
				v := client.Version
				expectedVersion = &v
			}
			secret, secretErr := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{
				TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID, ExpectedClientVersion: expectedVersion,
			})
			if secretErr != nil {
				return nil, secretErr
			}
			credentials = append(credentials, application.SubsystemServiceCredential{
				Purpose: requirement.Purpose, OAuthClient: client, PlaintextSecret: secret.PlaintextSecret,
			})
			continue
		}
		scopes := serviceCredentialScopes(requirement)
		created, createErr := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
			TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
			ClientID: clientID, ClientName: requirement.ClientName, ClientType: "service",
			TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
			GrantTypes: []string{"client_credentials"}, Scopes: scopes,
		})
		if createErr != nil {
			return nil, createErr
		}
		credentials = append(credentials, application.SubsystemServiceCredential{
			Purpose: requirement.Purpose, OAuthClient: created.Client, PlaintextSecret: created.PlaintextSecret,
		})
	}
	return credentials, nil
}

// ensureInitialPlatformBrowserCredential 面向只写的平台浏览器初始 Secret：
// 仅受信任的首次接管/重试路径可为既有绑定客户端铸造新版本。
func (handler *SubsystemOnboardingHandler) ensureInitialPlatformBrowserCredential(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID, redirectURI string) (application.OAuthClientView, string, error) {
	if handler.serviceCredentials == nil || applicationID == "" || environmentID == "" {
		return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
	}
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, "", err
	}
	canonical := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
	for _, client := range clients {
		if client.ClientID != canonical {
			continue
		}
		if client.ApplicationID != applicationID || client.EnvironmentID != environmentID || !strings.EqualFold(client.Status, "ACTIVE") || client.ClientType != "confidential" || client.TokenAuthMethod != "client_secret_basic" || !containsExactString(client.GrantTypes, "authorization_code") || !client.RequirePKCE || !containsExactString(client.RedirectURIs, redirectURI) {
			return application.OAuthClientView{}, "", application.ErrConflict
		}
		secret, err := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID})
		if err != nil {
			return application.OAuthClientView{}, "", err
		}
		if strings.TrimSpace(secret.PlaintextSecret) == "" {
			return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
		}
		return client, secret.PlaintextSecret, nil
	}
	if strings.TrimSpace(redirectURI) == "" {
		return application.OAuthClientView{}, "", application.ErrValidation
	}
	created, err := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: canonical, ClientName: applicationCode + " Web", ClientType: "confidential", TokenAuthMethod: "client_secret_basic",
		AccessTokenTTLSeconds: 15 * 60, RefreshTokenTTLSeconds: 30 * 24 * 60 * 60,
		RequirePKCE: true, GrantTypes: []string{"authorization_code", "refresh_token"}, Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirectURI},
	})
	if err != nil {
		return application.OAuthClientView{}, "", err
	}
	if strings.TrimSpace(created.PlaintextSecret) == "" {
		return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
	}
	return created.Client, created.PlaintextSecret, nil
}

func containsExactString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

// serviceCredentialScopes 返回指定机器凭据的完整最小权限集合。
// 文件网关客户端需要上传、绑定及按文件 ID 回读；其他用途保持单一 Scope。
func serviceCredentialScopes(requirement updateServiceCredentialRequirement) []string {
	scopes := []string{requirement.PrimaryScope()}
	if requirement.Purpose == application.ServiceCredentialFileGatewayWrite {
		scopes = append(scopes, "platform:file:bind", "platform:file:download")
	}
	return scopes
}

// sameStringSet 比较两个已授权 Scope 集合，忽略顺序但不忽略缺失或多余权限。
func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]int, len(left))
	for _, value := range left {
		seen[value]++
	}
	for _, value := range right {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}

// ensureCatalogPublisherCredential 为受控重试恢复目录发布能力，且绝不尝试读取
// 已存储的 Secret。Secret 是只写的：新版本只为受信任的部署 Agent 铸造，
// 绝不返回给浏览器。
func (handler *SubsystemOnboardingHandler) ensureCatalogPublisherCredential(ctx context.Context, tenantID, applicationID, environmentID, applicationCode, environment, operatorID string) (application.OAuthClientView, string, error) {
	if handler.serviceCredentials == nil {
		return application.OAuthClientView{}, "", application.ErrSubsystemProvisioningUnavailable
	}
	clientID := applicationCode + "-" + environment + "-catalog-publisher"
	clients, err := handler.serviceCredentials.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return application.OAuthClientView{}, "", err
	}
	for _, client := range clients {
		if client.ClientID != clientID {
			continue
		}
		if !strings.EqualFold(client.Status, "ACTIVE") || client.ApplicationID != applicationID || client.EnvironmentID != environmentID {
			return application.OAuthClientView{}, "", application.ErrConflict
		}
		secret, secretErr := handler.serviceCredentials.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{TenantID: tenantID, OAuthClientID: client.ID, OperatorID: operatorID})
		if secretErr != nil {
			return application.OAuthClientView{}, "", secretErr
		}
		return client, secret.PlaintextSecret, nil
	}
	created, createErr := handler.serviceCredentials.CreateOAuthClient(ctx, application.OAuthClientCreateInput{
		TenantID: tenantID, ApplicationID: applicationID, EnvironmentID: environmentID, OperatorID: operatorID,
		ClientID: clientID, ClientName: applicationCode + " Authorization Catalog Publisher", ClientType: "service",
		TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 15 * 60,
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"authorization.catalog.sync"},
	})
	if createErr != nil {
		return application.OAuthClientView{}, "", createErr
	}
	return created.Client, created.PlaintextSecret, nil
}

func (handler *SubsystemOnboardingHandler) updateSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request, forcedIssuerAlias, requestedOperation string) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if forcedIssuerAlias != "" {
		payload.IssuerAlias = forcedIssuerAlias
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	// 基础平台应用自身是登录与授权控制面根节点，其运行时由平台发布流程管理，不属于
	// subsystems.d 审核清单。若把它的环境交给子系统部署 Agent，生产模式会因为缺少清单
	// 校验和而失败，并在页面上留下误导性的“依赖服务不可用”。这里在写入任何生命周期
	// 状态之前明确拒绝，与应用的退役/环境删除保护保持一致。
	if isBuiltInPlatformApplication(applicationCode) {
		writeBuiltInPlatformRuntimeBlocked(writer, request, applicationCode, environment)
		return
	}
	isRetry := requestedOperation == "RETRY" || strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/subsystem-retry")
	// 平台签发的浏览器密钥不在普通更新中轮换。Keycloak 环境则需重新读取当前
	// RP 密钥并交给 Agent，修复运行配置漂移；读取不是轮换。授权目录发布凭据
	// 是应用绑定的机器身份，
	// 每次受控更新都必须重新下发，才能修复旧环境中遗留的占位值或已失效密钥。
	effectiveIssuerAlias, aliasErr := handler.deploymentIssuerAlias(request.Context(), principal.Tenant.ID, applicationCode, environment, payload.IssuerAlias, forcedIssuerAlias)
	if aliasErr != nil {
		handler.writeError(writer, request, aliasErr)
		return
	}
	// 显式 /switch 请求始终视为一次切换尝试，即使旧版浏览器已先行乐观更新了
	// issuer_alias。否则通用元数据写入可能意外绕过观察期门禁。
	keycloakCutover := strings.EqualFold(effectiveIssuerAlias, "keycloak") && (strings.EqualFold(forcedIssuerAlias, "keycloak") || handler.keycloakCutoverRequired(request.Context(), principal.Tenant.ID, applicationCode, environment))
	if keycloakCutover {
		readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if !readiness.SwitchReady {
			writeKeycloakSwitchBlocked(writer, request, readiness)
			return
		}
		if handler.keycloakCutover == nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if err := handler.keycloakCutover.CanKeycloakCutover(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
			// 安全（SEC-B3）：cause 只进结构化日志，HTTP details 用稳定错误码。
			handler.logger.Warn("Keycloak cutover blocked by observation gate", "application_code", applicationCode, "environment", environment, "error", err)
			writeKeycloakObservationBlocked(writer, request, keycloakObservationReasonCode(err))
			return
		}
	}
	// 与切换同理，显式回滚不得依赖浏览器此前的 issuer_alias 更新。生命周期存储是
	// 回滚截止期限的事实来源，并对仅平台环境安全拒绝。
	keycloakRollback := strings.EqualFold(forcedIssuerAlias, "platform")
	if keycloakRollback {
		if handler.keycloakCutover == nil {
			handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
			return
		}
		if err := handler.keycloakCutover.CanKeycloakRollback(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
			// 安全（SEC-B3）：cause 只进结构化日志，HTTP details 用稳定错误码。
			handler.logger.Warn("Keycloak rollback blocked by observation gate", "application_code", applicationCode, "environment", environment, "error", err)
			writeKeycloakObservationBlocked(writer, request, keycloakObservationReasonCode(err))
			return
		}
	}
	issuer, err := handler.issuerForAlias(effectiveIssuerAlias)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// 网关字段的裁剪与派生规则在应用层统一实现（BuildSubsystemUpdateProvisioningInput）；
	// 这里保留局部变量供后续受控步骤（凭据刷新、发布者凭据）复用。
	publicBaseURL := strings.TrimRight(strings.TrimSpace(payload.PublicBaseURL), "/")
	upstreamURL := strings.TrimRight(strings.TrimSpace(payload.UpstreamURL), "/")
	pathPrefix := strings.TrimRight(strings.TrimSpace(payload.PathPrefix), "/")
	// 装配逻辑提取到应用层（BuildSubsystemUpdateProvisioningInput）：统一入口与旧路径共用同一份派生与校验，
	// 避免两套实现漂移；凭据仍在后续受控步骤中解析后填充。
	updateInput, err := application.BuildSubsystemUpdateProvisioningInput(application.SubsystemUpdateProvisioningAssembly{
		TenantID: principal.Tenant.ID, ApplicationCode: applicationCode, Environment: environment,
		ManifestChecksum: handler.manifestChecksumForTarget(applicationCode, environment),
		Issuer:           issuer, AuthenticationProvider: effectiveIssuerAlias,
		AuthenticationRuntimeUpdate: keycloakCutover || keycloakRollback,
		PublicBaseURL:               payload.PublicBaseURL, UpstreamURL: payload.UpstreamURL, PathPrefix: payload.PathPrefix,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	// 生产模式下 Agent 只接受随发布包审核的清单目标，并要求请求携带清单校验和。API 侧
	// 若解析不到目标（清单缺失，或 platform-api 与 subsystem-provisioner 的部署模式
	// 不一致），继续调用 Agent 只会得到无信息的拒绝并污染部署状态。这里先给出可执行的
	// 诊断，避免用户看到重复的 "subsystem provisioning unavailable"。
	if capabilityProvider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		if strings.EqualFold(strings.TrimSpace(capabilityProvider.Capabilities().Mode), "production") &&
			strings.TrimSpace(updateInput.ManifestChecksum) == "" {
			writeProductionManifestTargetMissing(writer, request, applicationCode, environment)
			return
		}
	}
	// 每次携带显式网关字段的受控 Keycloak 重建都要调和 RP 凭据，而不只是 ADOPT/RETRY。
	// 此前普通 UPDATE 会保留过期的运行时 Secret，即使 Client 映射与容器都健康，
	// OIDC 回调仍会报 unauthorized_client。EnsureClient 只读取既有 Secret，
	// 不轮换、也不暴露给浏览器。
	keycloakRuntimeCredentialRefresh := strings.EqualFold(effectiveIssuerAlias, "keycloak") &&
		!keycloakCutover && strings.TrimSpace(updateInput.RedirectURI) != ""
	ensureKeycloakCredentials := func(applicationID, environmentID string) bool {
		if keycloakRuntimeCredentialRefresh {
			if handler.keycloakControl == nil {
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			canonicalClientID := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
			resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
			if resolver, ok := handler.keycloakMappings.(keycloakClientCompatibilityResolver); ok {
				effective, resolveErr := resolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID)
				if resolveErr != nil {
					handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
					return false
				}
				if strings.TrimSpace(effective.ClientID) != "" {
					resolution = effective
				}
			}
			client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, updateInput.RedirectURI)
			if clientErr != nil || client.ClientID != resolution.ClientID || strings.TrimSpace(client.ClientSecret) == "" {
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			updateInput.ClientID, updateInput.ClientSecret = client.ClientID, client.ClientSecret
			updateInput.AuthenticationRuntimeUpdate = true
		}
		if keycloakCutover {
			transport, transportErr := application.ValidateKeycloakCutoverTransport(publicBaseURL, pathPrefix, handler.keycloakRequireHTTPS)
			if transportErr != nil {
				handler.writeError(writer, request, transportErr)
				return false
			}
			updateInput.PublicURL = transport.PublicURL
			updateInput.RedirectURI = transport.RedirectURI
			if handler.keycloakControl == nil || handler.keycloakBroker == nil || publicBaseURL == "" || pathPrefix == "" {
				handler.writeError(writer, request, application.ErrValidation)
				return false
			}
			if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			canonicalClientID := strings.ToLower(applicationCode) + "-" + strings.ToLower(environment) + "-web"
			resolution := KeycloakClientResolution{ClientID: canonicalClientID, CanonicalClientID: canonicalClientID, Source: "canonical"}
			if applicationID, environmentID, resolved := handler.resolveApplicationContext(writer, request, applicationCode, environment); resolved {
				if compatibilityResolver, supported := handler.keycloakMappings.(keycloakClientCompatibilityResolver); supported {
					if effective, resolveErr := compatibilityResolver.ResolveEffectiveKeycloakClient(request.Context(), principal.Tenant.ID, applicationID, environmentID, canonicalClientID); resolveErr != nil {
						handler.logger.Warn("Keycloak Client compatibility resolution failed", "application_code", applicationCode, "environment", environment, "error", resolveErr)
					} else if strings.TrimSpace(effective.ClientID) != "" {
						resolution = effective
					}
				}
			}
			client, clientErr := handler.keycloakControl.EnsureClient(request.Context(), resolution.ClientID, "Basic Platform "+resolution.ClientID, updateInput.RedirectURI)
			if clientErr != nil {
				handler.logger.Warn("Keycloak Client synchronization before switch failed", "application_code", applicationCode, "environment", environment, "error", clientErr)
				handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
				return false
			}
			updateInput.ClientID, updateInput.ClientSecret = client.ClientID, client.ClientSecret
		}
		return true
	}
	// 标识符从部署控制面解析，而不是门户投影：失败与更新中的环境被刻意隐藏在
	// 用户可见的门户目录之外。
	var deploymentContext application.SubsystemDeploymentState
	environmentID := ""
	if handler.deploymentState != nil {
		state, err := handler.deploymentState.GetSubsystemDeploymentContext(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if err != nil {
			// 仅目录登记的应用/环境还没有部署状态行。首次部署失败后 UI 以"重试"暴露
			// 恢复动作，因此在该边界把 RETRY 视同 ADOPT。ResolveApplicationEnvironment
			// 仍会校验目标是已知的租户内应用；未知目标保持 404。
			if requestedOperation != "ADOPT" && !isRetry || !errors.Is(err, application.ErrNotFound) {
				handler.writeError(writer, request, err)
				return
			}
			applicationID, resolvedEnvironmentID, resolveErr := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
			if resolveErr != nil {
				handler.writeError(writer, request, resolveErr)
				return
			}
			deploymentContext = application.SubsystemDeploymentState{TenantID: principal.Tenant.ID, ApplicationID: applicationID, EnvironmentID: resolvedEnvironmentID, ApplicationCode: applicationCode, Environment: environment}
			updateInput.ApplicationID = applicationID
			environmentID = resolvedEnvironmentID
		} else {
			deploymentContext = state
			updateInput.ApplicationID = state.ApplicationID
			environmentID = state.EnvironmentID
		}
	} else if applicationID, resolvedEnvironmentID, ok := handler.resolveApplicationContext(writer, request, applicationCode, environment); ok {
		updateInput.ApplicationID = applicationID
		environmentID = resolvedEnvironmentID
	}
	if handler.deploymentState != nil && (deploymentContext.Status == application.SubsystemDeploymentStatusProvisioning ||
		deploymentContext.Status == application.SubsystemDeploymentStatusUpdating ||
		deploymentContext.Status == application.SubsystemDeploymentStatusVerifying) {
		if staleSubsystemDeployment(deploymentContext, time.Now().UTC()) {
			// 过期的在途状态先按既有语义收口为失败，让本次受控操作可以继续。
			deploymentContext = handler.recoverStaleSubsystemDeployment(request.Context(), deploymentContext)
		}
		if deploymentContext.Status == application.SubsystemDeploymentStatusProvisioning ||
			deploymentContext.Status == application.SubsystemDeploymentStatusUpdating ||
			deploymentContext.Status == application.SubsystemDeploymentStatusVerifying {
			// 真正的在途部署：拒绝并发编排，而不是让两个部署互相覆盖状态机。
			httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
				"SUBSYSTEM_DEPLOYMENT_IN_PROGRESS",
				"该应用环境的部署正在进行中；请等待完成或稍后刷新状态",
				map[string]string{"application_code": applicationCode, "environment": environment}))
			return
		}
	}
	if strings.TrimSpace(updateInput.ApplicationID) == "" {
		handler.writeError(writer, request, application.ErrNotFound)
		return
	}
	if publisherID, publisherSecret, ok := readCatalogPublisherCredentials(); ok {
		updateInput.CatalogPublisherClientID = publisherID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	operation := requestedOperation
	if operation == "" {
		operation = "UPDATE"
	}
	if isRetry {
		operation = "RETRY"
	}
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		updateInput.InitialRuntimeProvisioning = handler.deploymentState != nil && strings.EqualFold(provider.Capabilities().Mode, "production") &&
			(operation == "ADOPT" || (operation == "RETRY" && deploymentContext.Status == application.SubsystemDeploymentStatusFailed)) && deploymentContext.AppliedManifestChecksum == "" && deploymentContext.ManifestLastAppliedAt == nil
	}
	if requiresCatalogPublisherCredential(applicationCode) && handler.serviceCredentials == nil {
		// 授权目录发布凭据为只写密钥，缺少凭据管理器时绝不能假装更新成功并让
		// Agent 启动一个仍使用占位值的容器。
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	// 无变化跳过：纯“更新运行时”重放（未携带任何网关字段变更、未切换认证方），且期望
	// 清单与已应用清单一致、环境本就 READY 时，受控部署等价于对同一清单再跑一次
	// compose build——既浪费数分钟构建窗口，也让离线/受限网络环境因构建出网失败而无法
	// 完成本应成功的操作。直接确认现状并保持 READY；需要强制重建时使用 RETRY（全量）。
	if operation == "UPDATE" && !keycloakCutover && !keycloakRollback &&
		strings.TrimSpace(publicBaseURL) == "" && strings.TrimSpace(upstreamURL) == "" && strings.TrimSpace(pathPrefix) == "" &&
		deploymentContext.Status == application.SubsystemDeploymentStatusReady &&
		deploymentContext.DesiredManifestChecksum != "" &&
		deploymentContext.DesiredManifestChecksum == deploymentContext.AppliedManifestChecksum &&
		deploymentContext.DesiredManifestChecksum == updateInput.ManifestChecksum {
		handler.logger.Info("subsystem update skipped: deployment manifest unchanged",
			"application_code", applicationCode, "environment", environment,
			"manifest_checksum", updateInput.ManifestChecksum)
		writer.Header().Set("Cache-Control", "no-store, private")
		writer.Header().Set("Pragma", "no-cache")
		httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "部署清单无变化，运行时保持现状", map[string]string{
			"status":           application.SubsystemDeploymentStatusReady,
			"operation":        operation,
			"application_code": applicationCode,
			"environment":      environment,
		})
		return
	}
	generation := uint64(0)
	if handler.deploymentState != nil {
		var claimErr error
		generation, claimErr = handler.deploymentState.ClaimSubsystemDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, operation, time.Now().UTC())
		if claimErr != nil {
			if errors.Is(claimErr, application.ErrSubsystemDeploymentTransition) {
				httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New("SUBSYSTEM_DEPLOYMENT_IN_PROGRESS", "该应用环境的部署正在进行中", nil))
			} else {
				handler.writeError(writer, request, claimErr)
			}
			return
		}
	}
	accepted := false
	defer func() {
		if !accepted {
			handler.markDeploymentFailureDetached(subsystemDeploymentJob{TenantID: principal.Tenant.ID, ApplicationCode: applicationCode, Environment: environment, Operation: operation, Generation: generation}, "DEPLOYMENT_PREPARATION_FAILED", "部署准备失败，请点击重试")
		}
	}()
	relatedClients, relatedErr := handler.relatedServiceClients(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if relatedErr != nil {
		handler.writeError(writer, request, relatedErr, "contract integration dependency")
		return
	}
	updateInput.RelatedServiceClients = relatedClients
	if !ensureKeycloakCredentials(updateInput.ApplicationID, environmentID) {
		return
	}
	if updateInput.InitialRuntimeProvisioning && !strings.EqualFold(effectiveIssuerAlias, "keycloak") {
		client, secret, err := handler.ensureInitialPlatformBrowserCredential(request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID, applicationCode, environment, principal.User.ID, updateInput.RedirectURI)
		if err != nil {
			handler.writeError(writer, request, err)
			return
		}
		updateInput.ClientID, updateInput.ClientSecret = client.ClientID, secret
		updateInput.AuthenticationRuntimeUpdate = true
	}
	serviceCredentials, credentialErr := handler.ensureUpdateServiceCredentials(
		request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID,
		applicationCode, environment, principal.User.ID, operation,
	)
	if credentialErr != nil {
		handler.writeError(writer, request, credentialErr)
		return
	}
	updateInput.ServiceCredentials = serviceCredentials
	updateInput.EnvironmentID = environmentID

	if handler.serviceCredentials != nil && requiresCatalogPublisherCredential(applicationCode) {
		publisher, publisherSecret, publisherErr := handler.ensureCatalogPublisherCredential(
			request.Context(), principal.Tenant.ID, updateInput.ApplicationID, environmentID, applicationCode, environment, principal.User.ID,
		)
		if publisherErr != nil {
			handler.writeError(writer, request, publisherErr)
			return
		}
		updateInput.CatalogPublisherClientID = publisher.ClientID
		updateInput.CatalogPublisherClientSecret = publisherSecret
	}
	initialAccessUserID := principal.User.ID
	needsInitialAccess := operation == "ADOPT"
	if handler.deploymentState != nil && operation != "ADOPT" {
		if storedAdminUserID := strings.TrimSpace(deploymentContext.InitialAdminUserID); storedAdminUserID != "" {
			initialAccessUserID = storedAdminUserID
		}
		// UPDATE 与 RETRY 都是受控部署操作。升级迁移可以把历史上被错误标记为
		// “已分配”的初始访问重置为待补偿；无论用户从页面选择更新还是重试，均应在
		// Agent 发布最新角色目录后幂等补发一次。非空 marker 仍保持“不主动恢复已撤销
		// 权限”的既有语义。
		needsInitialAccess = deploymentContext.InitialAccessAssignedAt == nil
	}
	if err := handler.setDesiredManifest(request.Context(), principal.Tenant.ID, applicationCode, environment, updateInput.ManifestChecksum); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	jobApplicationID := deploymentContext.ApplicationID
	if jobApplicationID == "" {
		jobApplicationID = updateInput.ApplicationID
	}
	jobEnvironmentID := deploymentContext.EnvironmentID
	if jobEnvironmentID == "" {
		jobEnvironmentID = environmentID
	}
	if isRetry && handler.keycloakBroker != nil {
		// 重试通常紧随一次失败的部署：SSO 末端的 broker 凭据可能仍处缺口（自愈巡检
		// 有周期）。这里尽力补齐一次；失败仅告警——broker 缺口不阻断部署本身，只影响
		// 部署完成后的首次统一登录。
		if err := handler.reconcileKeycloakBroker(request.Context(), principal.Tenant.ID, principal.User.ID); err != nil {
			handler.logger.Warn("retry could not reconcile Keycloak broker credential",
				"application_code", applicationCode, "environment", environment, "error", err)
		}
	}
	handler.startSubsystemDeploymentJob(request, subsystemDeploymentJob{
		Operation:          operation,
		Generation:         generation,
		TenantID:           principal.Tenant.ID,
		OperatorUserID:     principal.User.ID,
		ApplicationCode:    applicationCode,
		ApplicationName:    applicationCode,
		ApplicationID:      jobApplicationID,
		EnvironmentID:      jobEnvironmentID,
		Environment:        environment,
		Input:              updateInput,
		InitialAdminUserID: initialAccessUserID,
		NeedsInitialAccess: needsInitialAccess,
		KeycloakCutover:    keycloakCutover,
		KeycloakRollback:   keycloakRollback,
		ManifestChecksum:   updateInput.ManifestChecksum,
	})
	accepted = true
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusAccepted, "部署已受理，正在后台执行；状态请查询 subsystem-status", map[string]string{
		"status":           application.SubsystemDeploymentStatusUpdating,
		"operation":        operation,
		"application_code": applicationCode,
		"environment":      environment,
	})
	handler.logger.Info("subsystem deployment accepted", "path", request.URL.Path,
		"application_code", applicationCode, "environment", environment, "operation", operation,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// requiresCatalogPublisherCredential 判断子系统是否会在启动时发布自有授权目录。
// 该机器凭据是只写数据，部署中断后不能从运行时文件反向恢复，因此每次受控生命周期操作
// 都重新下发，避免旧环境继续使用占位值或失效密钥。
func requiresCatalogPublisherCredential(applicationCode string) bool {
	switch applicationCode {
	case "contract_management", "customer_and_opportunity", "customer_portal", "data_analysis", "project_management", "settlement":
		return true
	default:
		return false
	}
}

// TeardownSubsystem 处理 POST /api/v1/subsystem-teardown。停止容器、删除 .env.local、
// 移除门户网关 include 并重载 nginx。HTTP 层不在此删除对应数据库行：脚本随后调用
// DELETE /environments 与（可选的）DELETE /applications，让审计留痕保留每个清理步骤。
func (handler *SubsystemOnboardingHandler) TeardownSubsystem(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	// 基础平台自身的运行时不由子系统部署 Agent 管理；与更新路径一致，在清理任何
	// 运行时资源或写入生命周期状态之前拒绝，避免误停平台自身服务。
	if isBuiltInPlatformApplication(applicationCode) {
		writeBuiltInPlatformRuntimeBlocked(writer, request, applicationCode, environment)
		return
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusDraining, "TEARDOWN", "", ""); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.provisioner.Teardown(request.Context(), principal.Tenant.ID, applicationCode, environment); err != nil {
		handler.markDeploymentFailed(request.Context(), principal.Tenant.ID, applicationCode, environment, "TEARDOWN", "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败")
		handler.writeError(writer, request, err)
		return
	}
	if err := handler.transitionDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, application.SubsystemDeploymentStatusOffboarded, "TEARDOWN", "", ""); err != nil {
		handler.logger.Error("subsystem teardown completed but state update failed", "application_code", applicationCode, "environment", environment, "error", err)
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统已拆解", map[string]string{
		"status":           "torn_down",
		"application_code": applicationCode,
		"environment":      environment,
	})
	handler.logger.Warn("subsystem torn down", "path", request.URL.Path,
		"application_code", applicationCode,
		"environment", environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// DiscardFailedSubsystemDeployment 处理 POST /api/v1/subsystem-deployment-discard。
// 这是半成品子系统的清理路径：控制面记录存在、部署失败，运维需要释放
// code+environment 组合以便重新接入。
//
// 顺序是安全边界：先按 status+generation 原子占用记录（PROVISION_FAILED → DISCARDING），只有
// 抢占成功的请求才执行 Agent Teardown。旧实现先 Teardown、再按 status=FAILED 删除记录，并发
// 的「重试」把记录推进为 UPDATING 后删除会被拒绝，但 Teardown 已经拆掉了重试刚部署的环境。
// Teardown 仍是 best-effort（失败部署可能从未创建容器或文件）；应用/环境记录刻意保留，随后
// 用 DELETE /applications 与 DELETE /environments 释放路径前缀。
func (handler *SubsystemOnboardingHandler) DiscardFailedSubsystemDeployment(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	state, err := handler.deploymentState.GetSubsystemDeploymentState(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	state = handler.recoverStaleSubsystemDeployment(request.Context(), state)
	if state.Status != application.SubsystemDeploymentStatusFailed {
		handler.writeError(writer, request, fmt.Errorf("deployment is not in a failed state: %s", state.Status))
		return
	}
	now := time.Now().UTC()
	// 先占用再销毁：占用失败说明状态已被并发推进（通常是对同一环境的「重试」已经接管），
	// 此时继续 Teardown 会拆掉重试刚部署起来的基础设施，因此直接按冲突返回且不做任何清理。
	claimed, claimErr := handler.deploymentState.ClaimFailedSubsystemDeploymentForDiscard(request.Context(), principal.Tenant.ID, applicationCode, environment, state.Generation, now)
	if claimErr != nil {
		handler.writeError(writer, request, claimErr)
		return
	}
	if !claimed {
		handler.logger.Warn("failed subsystem deployment discard skipped because the state advanced",
			"path", request.URL.Path, "application_code", applicationCode, "environment", environment,
			"generation", state.Generation, "actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
		)
		handler.writeError(writer, request, fmt.Errorf("%w: 该环境的部署状态已被并发操作推进（可能已开始重试），本次未清理任何基础设施；请刷新状态后确认", application.ErrConflict))
		return
	}
	// 基础设施清理是尽力而为：失败部署可能在数据库提交后才停止，容器、.env.local
	// 或网关 include 可能根本不存在。
	if teardownErr := handler.provisioner.Teardown(request.Context(), principal.Tenant.ID, applicationCode, environment); teardownErr != nil {
		handler.logger.Warn("discard teardown left infrastructure residue",
			"application_code", applicationCode, "environment", environment, "error", teardownErr)
	}
	// 占用时 generation 自增 1，删除必须带上占用后的代号，避免误删被看护任务收口后的记录。
	if err := handler.deploymentState.DiscardClaimedSubsystemDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, state.Generation+1); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "失败部署已清理", map[string]string{
		"status":           "discarded",
		"application_code": applicationCode,
		"environment":      environment,
		"next_action":      "DELETE /applications 与 /environments 以释放路径前缀后重新接入",
	})
	handler.logger.Warn("failed subsystem deployment discarded", "path", request.URL.Path,
		"application_code", applicationCode,
		"environment", environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

const subsystemDeploymentHTTPTimeout = 16 * time.Minute

// 失败状态写入必须能在浏览器断连或反向代理超时后完成。保持较短的超时，让降级的
// 数据库不能无限占住请求 handler，同时部署生命周期仍能到达终态。
const subsystemDeploymentFailureWriteTimeout = 10 * time.Second

// 部署状态会在长耗时 Agent 调用之前写入。若 API 进程在该调用期间被终止，
// 状态不能永远停留在非终态：管理 UI 只对失败的尝试暴露重试。该阈值刻意略高于
// 同步请求超时，使真正缓慢但仍存活的部署不会被状态轮询打断。
const subsystemDeploymentStaleAfter = application.SubsystemDeploymentStaleAfter

// extendSubsystemDeploymentWriteDeadline 让同步控制面请求在 Agent 有界的 15 分钟
// 部署窗口内保持存活。ResponseController 解包 Gin writer 直达底层网络连接；
// httptest 与不支持的 writer 安全忽略该能力。
func extendSubsystemDeploymentWriteDeadline(writer stdhttp.ResponseWriter) {
	controller := stdhttp.NewResponseController(writer)
	_ = controller.SetWriteDeadline(time.Now().Add(subsystemDeploymentHTTPTimeout))
}

// GetSubsystemStatus 处理 GET /api/v1/subsystem-status。仅暴露持久化的生命周期
// 元数据；部署命令、文件系统路径与凭据绝不经过该 API。
func (handler *SubsystemOnboardingHandler) GetSubsystemStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	if handler.deploymentState == nil {
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	state, err := handler.deploymentState.GetSubsystemDeploymentState(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		// 手工登记的应用/环境先于（或有意绕过）受管部署记录存在，但仍是合法的
		// 控制面资源，因此状态读取不能把整个运行时卡片变成 404。先解析登记边界：
		// 未知应用保持真实 404，已知应用返回明确的安全状态。
		if !errors.Is(err, application.ErrNotFound) {
			handler.writeError(writer, request, err)
			return
		}
		applicationID, environmentID, resolveErr := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if resolveErr != nil {
			handler.writeError(writer, request, resolveErr)
			return
		}
		state = application.SubsystemDeploymentState{
			TenantID: principal.Tenant.ID, ApplicationID: applicationID, EnvironmentID: environmentID,
			ApplicationCode: applicationCode, Environment: environment,
			Status: application.SubsystemDeploymentStatusUnmanaged, Operation: "NONE",
		}
	}
	state = handler.recoverStaleSubsystemDeployment(request.Context(), state)
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "子系统部署状态查询成功", subsystemDeploymentStateResponse{
		ApplicationCode:         state.ApplicationCode,
		Environment:             state.Environment,
		Status:                  state.Status,
		Operation:               state.Operation,
		Generation:              state.Generation,
		AttemptCount:            state.AttemptCount,
		DesiredManifestChecksum: state.DesiredManifestChecksum,
		AppliedManifestChecksum: state.AppliedManifestChecksum,
		ManifestDriftStatus:     state.ManifestDriftStatus,
		ManifestLastAppliedAt:   state.ManifestLastAppliedAt,
		ManifestLastVerifiedAt:  state.ManifestLastVerifiedAt,
		LastErrorCode:           state.LastErrorCode,
		LastError:               state.LastError,
		NextAction:              subsystemDeploymentStateNextAction(state),
		StartedAt:               state.StartedAt,
		CompletedAt:             state.CompletedAt,
		UpdatedAt:               state.UpdatedAt,
	})
}

// GetKeycloakIntegrationStatus 处理 GET /api/v1/keycloak-integration/status。
// 与部署状态分开：响应是所选提供方、其非敏感 Client 映射和服务端核验切换门禁的
// 持久视图。运行时凭据与任意环境元数据被刻意排除。
func (handler *SubsystemOnboardingHandler) GetKeycloakIntegrationStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	issuerAlias := handler.effectiveIssuerAlias("")
	if resolver, supported := handler.service.(subsystemEnvironmentIssuerResolver); supported {
		if persisted, resolveErr := resolver.ResolveEnvironmentIssuerAlias(request.Context(), principal.Tenant.ID, applicationCode, environment); resolveErr == nil {
			issuerAlias = handler.effectiveIssuerAlias(persisted)
		}
	}
	mapping := KeycloakClientMapping{Status: "NOT_CONFIGURED"}
	if inspector, supported := handler.keycloakMappings.(keycloakClientMappingInspector); supported {
		loaded, loadErr := inspector.GetKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID)
		if loadErr != nil {
			handler.logger.Warn("load Keycloak Client mapping status failed", "application_code", applicationCode, "environment", environment, "error", loadErr)
			mapping.Status = "UNKNOWN"
		} else {
			mapping = loaded
		}
	}
	if mapping.Realm == "" {
		mapping.Realm = handler.keycloakRealm
	}
	readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
	claimsState := "NOT_CONFIGURED"
	if mapping.Exists && strings.EqualFold(mapping.Status, "SYNCED") {
		claimsState = "MAPPERS_SYNCED"
	} else if mapping.Status == "UNKNOWN" {
		claimsState = "UNKNOWN"
	}
	cutover := KeycloakCutoverLifecycle{Status: "NOT_CONFIGURED"}
	var timeline []KeycloakCutoverTimelineEvent
	if handler.keycloakCutover != nil {
		loaded, lifecycleErr := handler.keycloakCutover.GetKeycloakCutoverLifecycle(request.Context(), principal.Tenant.ID, applicationCode, environment)
		if lifecycleErr != nil {
			handler.logger.Warn("load Keycloak cutover lifecycle failed", "application_code", applicationCode, "environment", environment, "error", lifecycleErr)
			cutover.Status = "UNKNOWN"
		} else {
			cutover = loaded
			timeline, lifecycleErr = handler.keycloakCutover.ListKeycloakCutoverTimeline(request.Context(), principal.Tenant.ID, applicationCode, environment, 50)
			if lifecycleErr != nil {
				handler.logger.Warn("load Keycloak cutover timeline failed", "application_code", applicationCode, "environment", environment, "error", lifecycleErr)
			}
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 认证接入状态查询成功", map[string]any{
		"application_code": applicationCode,
		"environment":      environment,
		"provider":         issuerAlias,
		"realm":            mapping.Realm,
		"client_id":        mapping.ClientID,
		"client_state":     mapping.Status,
		"claims_state":     claimsState,
		"last_synced_at":   mapping.LastSyncedAt,
		"switch_ready":     readiness.SwitchReady,
		"switch_gates":     readiness.Gates,
		"next_action":      readiness.NextAction,
		"cutover":          cutover,
		"timeline":         timeline,
	})
}

// KeycloakSyncStatusResponse 是单个子系统环境的聚焦同步状态，包含供运维看板
// 使用的漂移检测结果。
type KeycloakSyncStatusResponse struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	ClientID        string `json:"client_id,omitempty"`
	ClientState     string `json:"client_state"`
	Realm           string `json:"realm"`
	LastSyncedAt    any    `json:"last_synced_at,omitempty"`
	SwitchReady     bool   `json:"switch_ready"`
	DriftAvailable  bool   `json:"drift_available"`
	DriftError      string `json:"drift_error,omitempty"`
	Drift           *struct {
		HasDrift       bool     `json:"has_drift"`
		MissingRoles   []string `json:"missing_roles,omitempty"`
		StaleRoles     []string `json:"stale_roles,omitempty"`
		MissingMappers []string `json:"missing_mappers,omitempty"`
		DriftedMappers []string `json:"drifted_mappers,omitempty"`
		RedirectURIOK  bool     `json:"redirect_uri_ok"`
		BrokerConfigOK bool     `json:"broker_config_ok"`
	} `json:"drift,omitempty"`
}

func (handler *SubsystemOnboardingHandler) loadKeycloakRoleCodesForDrift(ctx context.Context, tenantID, applicationID string) ([]string, error) {
	if handler.keycloakCatalog == nil {
		return nil, errors.New("Keycloak authorization catalog is not configured")
	}
	return handler.keycloakCatalog.ListKeycloakRoleCodes(ctx, tenantID, applicationID)
}

// GetKeycloakSyncStatus 返回单个子系统环境的聚焦同步状态，含漂移检测。
func (handler *SubsystemOnboardingHandler) GetKeycloakSyncStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	applicationCode := strings.TrimSpace(request.URL.Query().Get("application_code"))
	environment := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("environment")))
	if applicationCode == "" || environment == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	applicationID, environmentID, err := handler.service.ResolveApplicationEnvironment(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	response := KeycloakSyncStatusResponse{
		ApplicationCode: applicationCode,
		Environment:     environment,
		Realm:           handler.keycloakRealm,
	}
	// 读取 Client 映射。
	if inspector, supported := handler.keycloakMappings.(keycloakClientMappingInspector); supported {
		mapping, loadErr := inspector.GetKeycloakClientMapping(request.Context(), principal.Tenant.ID, applicationID, environmentID)
		if loadErr != nil {
			handler.logger.Warn("load Keycloak client mapping for sync status failed",
				"application_code", applicationCode, "environment", environment, "error", loadErr)
			response.DriftError = "CLIENT_MAPPING_UNAVAILABLE"
		} else {
			response.ClientID = mapping.ClientID
			response.ClientState = mapping.Status
			response.LastSyncedAt = mapping.LastSyncedAt
		}
	}
	// 读取切换门禁就绪状态。
	readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, applicationCode, environment)
	response.SwitchReady = readiness.SwitchReady
	// 控制面可用时执行漂移检测。
	if handler.keycloakControl != nil && response.ClientID != "" {
		roleCodes, catalogErr := handler.loadKeycloakRoleCodesForDrift(request.Context(), principal.Tenant.ID, applicationID)
		if catalogErr != nil {
			handler.logger.Warn("load role catalog for drift check failed",
				"application_code", applicationCode, "environment", environment, "error", catalogErr)
			response.DriftError = "ROLE_CATALOG_UNAVAILABLE"
		} else {
			resolver, supported := handler.service.(subsystemEnvironmentRedirectURIResolver)
			if !supported {
				response.DriftError = "GATEWAY_CONFIGURATION_UNAVAILABLE"
				writer.Header().Set("Cache-Control", "no-store, private")
				httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 同步状态查询成功", response)
				return
			}
			redirectURI, redirectErr := resolver.ResolveApplicationEnvironmentRedirectURI(request.Context(), principal.Tenant.ID, applicationID, environmentID)
			if redirectErr != nil {
				handler.logger.Warn("resolve redirect URI for drift check failed", "application_code", applicationCode, "environment", environment, "error", redirectErr)
				response.DriftError = "GATEWAY_CONFIGURATION_UNAVAILABLE"
				writer.Header().Set("Cache-Control", "no-store, private")
				httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 同步状态查询成功", response)
				return
			}
			drift, driftErr := handler.keycloakControl.DetectSubsystemKeycloakDrift(request.Context(), response.ClientID, redirectURI, roleCodes)
			if driftErr != nil {
				handler.logger.Warn("drift detection failed",
					"application_code", applicationCode, "environment", environment, "error", driftErr)
				response.DriftError = "DRIFT_DETECTION_FAILED"
			} else {
				response.DriftAvailable = true
				response.Drift = &struct {
					HasDrift       bool     `json:"has_drift"`
					MissingRoles   []string `json:"missing_roles,omitempty"`
					StaleRoles     []string `json:"stale_roles,omitempty"`
					MissingMappers []string `json:"missing_mappers,omitempty"`
					DriftedMappers []string `json:"drifted_mappers,omitempty"`
					RedirectURIOK  bool     `json:"redirect_uri_ok"`
					BrokerConfigOK bool     `json:"broker_config_ok"`
				}{
					HasDrift:       drift.HasDrift,
					MissingRoles:   drift.MissingRoles,
					StaleRoles:     drift.StaleRoles,
					MissingMappers: drift.MissingMappers,
					DriftedMappers: drift.DriftedMappers,
					RedirectURIOK:  drift.RedirectURIOK,
					BrokerConfigOK: drift.BrokerConfigOK,
				}
			}
		}
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 同步状态查询成功", response)
}

// recoverStaleSubsystemDeployment 只收口 API/Agent 重启可能遗留的生命周期状态。
// 转移刻意从已认证的状态读取路径执行，运维刷新页面即可恢复界面，
// 无需直接操作数据库或重新发起接入。
func (handler *SubsystemOnboardingHandler) recoverStaleSubsystemDeployment(ctx context.Context, state application.SubsystemDeploymentState) application.SubsystemDeploymentState {
	if !staleSubsystemDeployment(state, time.Now().UTC()) {
		return state
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	recovered, err := handler.deploymentState.RecoverStaleSubsystemDeployment(writeCtx, state, time.Now().UTC().Add(-subsystemDeploymentStaleAfter), time.Now().UTC())
	if err != nil {
		handler.logger.Warn("stale subsystem deployment could not be recovered",
			"application_code", state.ApplicationCode, "environment", state.Environment, "error", err)
		return state
	}
	if !recovered {
		current, getErr := handler.deploymentState.GetSubsystemDeploymentState(ctx, state.TenantID, state.ApplicationCode, state.Environment)
		if getErr == nil {
			return current
		}
		return state
	}
	state.Status = application.SubsystemDeploymentStatusFailed
	state.LastErrorCode = "DEPLOYMENT_INTERRUPTED"
	state.LastError = "部署请求中断，请点击重试"
	state.CompletedAt = pointerToTime(time.Now().UTC())
	state.UpdatedAt = time.Now().UTC()
	return state
}

func staleSubsystemDeployment(state application.SubsystemDeploymentState, now time.Time) bool {
	if state.Status != application.SubsystemDeploymentStatusProvisioning &&
		state.Status != application.SubsystemDeploymentStatusUpdating &&
		state.Status != application.SubsystemDeploymentStatusVerifying &&
		state.Status != application.SubsystemDeploymentStatusDiscarding {
		return false
	}
	if state.StartedAt == nil {
		return false
	}
	return now.Sub(state.StartedAt.UTC()) > subsystemDeploymentStaleAfter
}

func pointerToTime(value time.Time) *time.Time {
	return &value
}

func subsystemDeploymentStateNextAction(state application.SubsystemDeploymentState) string {
	if state.Status == application.SubsystemDeploymentStatusUnmanaged {
		return "该应用环境已登记，但尚未产生受控部署记录；请通过本地编排状态和健康检查确认服务运行。只有部署 Agent 已明确支持该应用时，才能执行受控运行时更新"
	}
	if state.Status != application.SubsystemDeploymentStatusFailed {
		return ""
	}
	if state.LastErrorCode == "INITIAL_ACCESS_ASSIGNMENT_FAILED" || state.LastErrorCode == "INITIAL_ACCESS_STATE_FAILED" {
		return "部署运行时已完成，但初始管理员授权尚未完成；确认目标用户有效且授权目录已同步后，点击“重试”"
	}
	if _, nextAction := runtimeLicenseApprovalDiagnostic(state.LastErrorCode); nextAction != "" {
		return nextAction
	}
	return "请检查部署 Agent、Docker 和目标 API 健康状态，修复后在当前环境点击“重试”；不要重复新增接入"
}

func (handler *SubsystemOnboardingHandler) transitionDeployment(ctx context.Context, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.TransitionSubsystemDeployment(ctx, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markInitialAccessAssigned(ctx context.Context, tenantID, applicationCode, environment, userID string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.MarkSubsystemInitialAccessAssigned(ctx, tenantID, applicationCode, environment, userID, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markDeploymentFailed(ctx context.Context, tenantID, applicationCode, environment, operation, errorCode, errorMessage string) {
	if err := handler.transitionDeploymentFailure(ctx, tenantID, applicationCode, environment, operation, errorCode, errorMessage); err != nil {
		handler.logger.Error("failed to persist subsystem deployment failure", "application_code", applicationCode, "environment", environment, "error", err)
	}
}

func (handler *SubsystemOnboardingHandler) transitionDeploymentFailure(ctx context.Context, tenantID, applicationCode, environment, operation, errorCode, errorMessage string) error {
	if handler.deploymentState == nil {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	return handler.transitionDeployment(writeCtx, tenantID, applicationCode, environment, application.SubsystemDeploymentStatusFailed, operation, errorCode, errorMessage)
}

func validateLifecycleRequest(payload subsystemLifecycleRequest) error {
	if strings.TrimSpace(payload.ApplicationCode) == "" {
		return application.ErrValidation
	}
	if strings.TrimSpace(payload.Environment) == "" {
		return application.ErrValidation
	}
	return nil
}

// isBuiltInPlatformApplication 判断目标应用是否是平台自身的控制面应用。
// 取值刻意镜像未导出的 application.builtInApplicationCode：HTTP 边界保留自己的
// 副本，使运行时生命周期守卫不因别处重命名而漂移。
func isBuiltInPlatformApplication(applicationCode string) bool {
	switch strings.ToLower(strings.TrimSpace(applicationCode)) {
	case "platform", "basic_platform":
		return true
	default:
		return false
	}
}

// writeBuiltInPlatformRuntimeBlocked 拒绝针对平台应用自身的运行时生命周期操作。
// 平台运行时由平台发布流程部署，绝不由子系统部署 Agent 部署；
// 暴露该动作只会产生一个难以理解的 Agent 失败。
func writeBuiltInPlatformRuntimeBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, applicationCode, environment string) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_PLATFORM_RUNTIME_NOT_MANAGED",
		"基础平台应用由平台发布流程管理，不能通过子系统部署 Agent 更新、重试、接管或下线运行时",
		map[string]string{
			"application_code": applicationCode,
			"environment":      environment,
			"next_action": "基础平台自身的升级请在服务器部署目录按发布流程执行；" +
				"子系统运行时的部署操作只适用于 subsystems.d 审核清单内的应用环境",
		},
	))
}

// subsystemDeploymentJob 携带一次已受理的部署编排脱离 HTTP 请求执行。所有持久标识都在
// 请求返回前捕获完成；后台步骤绝不读取 request 作用域的状态。
type subsystemDeploymentJob struct {
	Generation         uint64
	Operation          string
	TenantID           string
	OperatorUserID     string
	ApplicationCode    string
	ApplicationName    string
	ApplicationID      string
	EnvironmentID      string
	Environment        string
	Input              application.SubsystemProvisioningInput
	InitialAdminUserID string
	NeedsInitialAccess bool
	KeycloakClientID   string
	KeycloakCutover    bool
	KeycloakRollback   bool
	ManifestChecksum   string
}

// subsystemDeploymentAsyncTimeout 约束一次脱离请求的部署编排，与 Agent 自身 16 分钟部署
// 窗口对齐：终态由本编排关闭，而不是落到 20 分钟的 stale 恢复路径（后者只兜进程死亡）。
const subsystemDeploymentAsyncTimeout = 16 * time.Minute

// startSubsystemDeploymentJob 在与 HTTP 请求解除关联的上下文上运行已受理的部署。客户端
// 断连、浏览器刷新或 CLI 超时因此不再能杀死进行中的 Agent，也不会把生命周期留在
// UPDATING；后台内的失败收口用 closeFailedDeployment（再二次脱离上下文），专供编排
// 超时后仍能把状态落进 PROVISION_FAILED。
func (handler *SubsystemOnboardingHandler) startSubsystemDeploymentJob(request *stdhttp.Request, job subsystemDeploymentJob) {
	timeout := handler.deploymentTimeout
	if timeout <= 0 {
		timeout = subsystemDeploymentAsyncTimeout
	}
	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), timeout)
	handler.deploymentJobs.Add(1)
	go func() {
		defer handler.deploymentJobs.Done()
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				handler.logger.Error("subsystem deployment job panicked",
					"application_code", job.ApplicationCode, "environment", job.Environment, "panic", recovered)
				handler.markDeploymentFailureDetached(job, "DEPLOYMENT_JOB_PANICKED", "部署编排异常终止；请修复后点击重试")
			}
		}()
		if runErr := handler.runSubsystemDeploymentCompletion(bgCtx, job); runErr != nil {
			handler.logger.Error("subsystem deployment job finished with error",
				"application_code", job.ApplicationCode, "environment", job.Environment,
				"operation", job.Operation, "error", runErr)
		}
	}()
}

// runSubsystemDeploymentCompletion 执行长耗时的 Agent 调用与全部持久终态写。每个失败分支
// 都自行把生命周期收口（脱离 ctx），编排超时也只会落在 PROVISION_FAILED，绝不冻结在
// UPDATING。
func (handler *SubsystemOnboardingHandler) runSubsystemDeploymentCompletion(ctx context.Context, job subsystemDeploymentJob) error {
	if handler.runtimeLicenses != nil {
		if handler.runtimeLicenseSettings == nil {
			handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_BINDING_FAILED", "授权实例绑定读取未配置", application.ErrValidation, false)
			return application.ErrValidation
		}
		settings, settingsErr := handler.runtimeLicenseSettings(ctx)
		if settingsErr != nil && !errors.Is(settingsErr, application.ErrRuntimeLicenseNotInitialized) {
			handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_BINDING_FAILED", "授权实例绑定读取失败", settingsErr, false)
			return settingsErr
		}
		if settingsErr == nil {
			job.Input.RuntimeLicenseSettings = settings
			credentials, err := handler.runtimeLicenses.PrepareWithSettings(ctx, job.TenantID, job.ApplicationID, job.EnvironmentID, job.ApplicationCode, job.Environment, job.OperatorUserID, settings)
			if err != nil {
				handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_ENROLLMENT_FAILED", "授权运行组件登记失败；请检查批准发布清单后重试", err, false)
				return err
			}
			job.Input.RuntimeLicenseCredentials = credentials
		}
	}
	if job.Operation == "ONBOARD" {
		if err := handler.provisioner.Provision(ctx, job.Input); err != nil {
			handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败", err, true)
			return err
		}
		if err := handler.waitLocalRuntimeConfirmation(ctx, job); err != nil {
			return err
		}
		if err := handler.markManifestApplied(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.ManifestChecksum); err != nil {
			handler.closeFailedDeployment(ctx, job, "MANIFEST_STATE_FAILED", "部署清单状态保存失败", err, true)
			return err
		}
		if _, err := handler.access.AssignInitialAdministrator(ctx, job.TenantID, job.ApplicationCode, job.InitialAdminUserID, job.OperatorUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_ASSIGNMENT_FAILED", "初始管理员授权失败", err, true)
			return err
		}
		if err := handler.markInitialAccessAssigned(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.InitialAdminUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_STATE_FAILED", "初始管理员授权状态保存失败", err, true)
			return err
		}
		if err := handler.completeOnboardKeycloakProjection(ctx, job); err != nil {
			return err
		}
		if err := handler.completeDeployment(ctx, job, application.SubsystemDeploymentStatusReady, "", ""); err != nil {
			// 编排本身已完成而终态写失败：必须收口为失败，不能把状态机冻结在 UPDATING。
			handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_STATE_FAILED", "部署完成但状态写入失败", err, true)
			return err
		}
		handler.notifySubsystemLifecycle(ctx, job.TenantID, job.OperatorUserID, job.ApplicationName, job.ApplicationCode, job.Environment, true, "")
		return nil
	}
	if err := handler.provisioner.Update(ctx, job.Input); err != nil {
		handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败", err, true)
		return err
	}
	if err := handler.waitLocalRuntimeConfirmation(ctx, job); err != nil {
		return err
	}
	if err := handler.markManifestApplied(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.ManifestChecksum); err != nil {
		handler.closeFailedDeployment(ctx, job, "MANIFEST_STATE_FAILED", "部署清单状态保存失败", err, false)
		return err
	}
	if job.NeedsInitialAccess {
		if _, err := handler.access.AssignInitialAdministrator(ctx, job.TenantID, job.ApplicationCode, job.InitialAdminUserID, job.OperatorUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_ASSIGNMENT_FAILED", "初始管理员授权失败", err, false)
			return err
		}
		if err := handler.markInitialAccessAssigned(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.InitialAdminUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_STATE_FAILED", "初始管理员授权状态保存失败", err, false)
			return err
		}
	}
	if job.KeycloakCutover || job.KeycloakRollback {
		var lifecycleErr error
		if job.KeycloakCutover {
			_, lifecycleErr = handler.keycloakCutover.ConfirmKeycloakCutover(ctx, job.TenantID, job.ApplicationID, job.EnvironmentID, job.OperatorUserID, keycloakRollbackWindow)
		} else {
			_, lifecycleErr = handler.keycloakCutover.RecordKeycloakRollback(ctx, job.TenantID, job.ApplicationID, job.EnvironmentID, job.OperatorUserID)
		}
		if lifecycleErr != nil {
			handler.logger.Error("Keycloak runtime change completed but lifecycle evidence could not be recorded",
				"application_code", job.ApplicationCode, "environment", job.Environment, "error", lifecycleErr)
			handler.closeFailedDeployment(ctx, job, "KEYCLOAK_LIFECYCLE_RECORD_FAILED", "Keycloak 切换证据记录失败", lifecycleErr, true)
			return lifecycleErr
		}
	}
	if err := handler.completeDeployment(ctx, job, application.SubsystemDeploymentStatusReady, "", ""); err != nil {
		handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_STATE_FAILED", "部署完成但状态写入失败", err, true)
		return err
	}
	handler.notifySubsystemLifecycle(ctx, job.TenantID, job.OperatorUserID, job.ApplicationName, job.ApplicationCode, job.Environment, true, "")
	return nil
}

// completeOnboardKeycloakProjection 完成 Keycloak 优先接入的角色目录投影与就绪标记。
//
// 具体步骤由共享应用服务 KeycloakAuthorizationProjectionService 实现：新旧接入入口
// 调用同一份逻辑，避免"新入口迁移了主步骤、却漏掉旧流程必要处理"的漂移。
func (handler *SubsystemOnboardingHandler) completeOnboardKeycloakProjection(ctx context.Context, job subsystemDeploymentJob) error {
	if job.KeycloakClientID == "" {
		return nil
	}
	if handler.keycloakCatalog == nil || handler.keycloakMappings == nil {
		handler.closeFailedDeployment(ctx, job, application.KeycloakProjectionCodeMappingUnavailable, "Keycloak 授权映射组件不可用", nil, false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	var readiness application.KeycloakProjectionReadinessUpdater
	if updater, ok := handler.keycloakReadiness.(keycloakSwitchReadinessUpdater); ok {
		readiness = updater
	}
	service, err := application.NewKeycloakAuthorizationProjectionService(handler.keycloakCatalog, handler.keycloakControl, handler.keycloakMappings, readiness)
	if err != nil {
		handler.closeFailedDeployment(ctx, job, application.KeycloakProjectionCodeMappingUnavailable, "Keycloak 授权映射组件不可用", err, false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	result, err := service.Project(ctx, application.KeycloakProjectionInput{
		TenantID: job.TenantID, ApplicationID: job.ApplicationID, EnvironmentID: job.EnvironmentID,
		ClientID: job.KeycloakClientID, Realm: handler.keycloakRealm,
	})
	if err != nil {
		handler.closeFailedDeployment(ctx, job, application.KeycloakProjectionCodeBackfillFail, "Keycloak 授权投影失败", err, false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	if result.ErrorCode != "" {
		handler.closeFailedDeployment(ctx, job, result.ErrorCode, keycloakProjectionFailureMessage(result.ErrorCode), errors.New(result.Diagnostic), false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	return nil
}

// keycloakProjectionFailureMessage 把共享服务的错误码翻译成面向操作员的描述。
func keycloakProjectionFailureMessage(code string) string {
	switch code {
	case application.KeycloakProjectionCodeRoleCatalogSyncFail:
		return "Keycloak 角色目录同步失败"
	case application.KeycloakProjectionCodeClientMappingFail:
		return "Keycloak Client 映射保存失败"
	case application.KeycloakProjectionCodeBackfillFail:
		return "Keycloak 授权投影回填失败"
	case application.KeycloakProjectionCodeReadinessFail:
		return "Keycloak 就绪状态写入失败"
	default:
		return "Keycloak 授权投影失败"
	}
}

// closeFailedDeployment 用独立超时的上下文把失败收口进生命周期状态机：即使编排 ctx 已因
// 超时取消，状态机也必须落在 PROVISION_FAILED。通知沿用原始 ctx 尽力而为。
func (handler *SubsystemOnboardingHandler) closeFailedDeployment(ctx context.Context, job subsystemDeploymentJob, errorCode, errorMessage string, cause error, notify bool) {
	if code := application.RuntimeLicenseApprovalErrorCode(cause); code != "" {
		errorCode = code
		errorMessage, _ = runtimeLicenseApprovalDiagnostic(code)
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	if err := handler.completeDeployment(writeCtx, job, application.SubsystemDeploymentStatusFailed, errorCode, errorMessage); err != nil {
		handler.logger.Error("failed to persist subsystem deployment failure",
			"application_code", job.ApplicationCode, "environment", job.Environment, "error", err)
	}
	if notify {
		handler.notifySubsystemLifecycle(ctx, job.TenantID, job.OperatorUserID, job.ApplicationName, job.ApplicationCode, job.Environment, false, subsystemLifecycleFailureDetail(cause))
	}
}

// waitForDeploymentJobs 等待全部已受理的后台部署完成。本包测试用它使异步编排
// 可以确定性收口。
func (handler *SubsystemOnboardingHandler) waitForDeploymentJobs() {
	handler.deploymentJobs.Wait()
}

// markDeploymentFailureDetached 是 panic 兜底专用的失败收口：完全脱离任何可能已取消的
// 上下文。
func (handler *SubsystemOnboardingHandler) markDeploymentFailureDetached(job subsystemDeploymentJob, errorCode, errorMessage string) {
	writeCtx, cancel := context.WithTimeout(context.Background(), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	if err := handler.completeDeployment(writeCtx, job, application.SubsystemDeploymentStatusFailed, errorCode, errorMessage); err != nil {
		handler.logger.Error("failed to persist subsystem deployment failure after panic",
			"application_code", job.ApplicationCode, "environment", job.Environment, "error", err)
	}
}

func (handler *SubsystemOnboardingHandler) completeDeployment(ctx context.Context, job subsystemDeploymentJob, status, code, message string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.CompleteSubsystemDeployment(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.Generation, status, job.Operation, code, message, time.Now().UTC())
}

// writeProductionManifestTargetMissing 拒绝目标缺少已审核清单校验和的生产运行时操作。
// 触发场景：API 在 subsystems.d 中找不到该应用/环境，或 platform-api 与
// subsystem-provisioner 对部署模式的认定不一致。在此处失败可保持生命周期状态
// 不被触碰，并给操作者一个具体的修复方向。
func writeProductionManifestTargetMissing(writer stdhttp.ResponseWriter, request *stdhttp.Request, applicationCode, environment string) {
	httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.New(
		"IAM_SUBSYSTEM_TARGET_NOT_IN_PRODUCTION_MANIFEST",
		"服务器审核清单中没有该应用环境的受控部署目标，无法自动更新运行时",
		map[string]string{
			"application_code": applicationCode,
			"environment":      environment,
			"next_action": "请确认 platform-api 与 subsystem-provisioner 都使用同一组已审核的 subsystems.d 清单和 SUBSYSTEM_ONBOARDING_MODE=production，" +
				"并同步重启两个服务；若该应用环境确实不在清单内，请先通过代码评审发布对应清单",
		},
	))
}

func subsystemPrincipal(writer stdhttp.ResponseWriter, request *stdhttp.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}

func (handler *SubsystemOnboardingHandler) writeError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error, provisioningStages ...string) {
	var onboardingConflict *application.SubsystemOnboardingConflict
	switch {
	case errors.As(err, &onboardingConflict):
		handler.logger.Warn("subsystem onboarding skipped because environment already exists",
			"path", request.URL.Path,
			"application_code", onboardingConflict.ApplicationCode,
			"environment", onboardingConflict.Environment,
			"environment_status", onboardingConflict.Status,
		)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
			"IAM_SUBSYSTEM_ALREADY_ONBOARDED",
			"该应用环境已存在；接入脚本不会覆盖已有登录目标或 OAuth 客户端",
			map[string]string{
				"application_code": onboardingConflict.ApplicationCode,
				"environment":      onboardingConflict.Environment,
				"status":           onboardingConflict.Status,
				"next_action":      "该环境的控制面记录已存在。子系统日常代码、镜像、功能模块和业务迁移发布无需执行接入或撤销脚本；仅基础设施入口参数变更时使用环境、登录目标或 OAuth 客户端的受控更新接口。若此前部署 Agent 失败，请先查询 subsystem-status，再使用 subsystem-retry，不要重复 onboard",
			},
		))
	case errors.Is(err, errRelatedContractDependency):
		handler.logger.Warn("subsystem source credential dependency unavailable", "path", request.URL.Path, "source_application", "contract_management", "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New("IAM_SUBSYSTEM_CONTRACT_DEPENDENCY_REQUIRED", "结算接入需要已登记的合同事件机器凭据", map[string]string{"source_application": "contract_management", "next_action": "请先在同一租户和环境接入或受控更新合同管理系统，补齐 settlement_contract_ingest 用途凭据，再重试结算接入或更新"}))
	case errors.Is(err, application.ErrValidation), errors.Is(err, application.ErrManagementValidation):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound), errors.Is(err, application.ErrManagementNotFound):
		httpresponse.WriteError(writer, request, stdhttp.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict), errors.Is(err, application.ErrVersionConflict), errors.Is(err, application.ErrManagementConflict):
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrSubsystemProvisioningUnavailable):
		handler.logger.Error("subsystem automatic provisioning failed", "path", request.URL.Path, "error", err)
		message := httperror.DependencyUnavailable.Message
		if strings.Contains(strings.ToLower(err.Error()), "disabled") {
			message = "当前部署未启用受控部署 Agent，无法在平台内完成一键接入"
		}
		// 安全（SEC-B3）：details 只暴露稳定错误码与本地化指引，Agent/DB 错误原文
		// （表名、SQL state、路径）只进上面的结构化日志，不再附在响应里。
		httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.New(
			httperror.DependencyUnavailable.Code,
			message,
			map[string]string{
				"next_action": subsystemProvisioningNextAction(err, provisioningStages...),
				"error_code":  subsystemProvisioningErrorCode(err),
			},
		))
	default:
		handler.logger.Error("subsystem onboarding request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}

var (
	// internalErrorDetailPattern 匹配 SQL/ORM 关键词与 MySQL 错误号；命中即认为文本
	// 属于内部实现细节（SEC-B3/B9），不允许进入 HTTP details 或收件箱通知。
	internalErrorDetailPattern = regexp.MustCompile(`(?i)\b(select|insert|update|delete|where|from|join|table|database|schema|column|mysql|gorm|sqlstate|syntax|constraint|duplicate|foreign key|error\s*1\d{3})\b`)
	// errorPathPattern 匹配至少两段的文件系统路径（含 Windows 盘符）。
	errorPathPattern = regexp.MustCompile(`(?:[A-Za-z]:)?(?:/[\w.\-]+){2,}`)
	// stableDetailCodePattern 判断冒号前的前缀是否是稳定错误码。
	stableDetailCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{3,}$`)
)

// subsystemProvisioningErrorCode 把 Agent/部署错误归类为稳定错误码（SEC-B3）。
// 分类键与 subsystemProvisioningNextAction 的诊断键保持一致；错误原文只在本函数
// 内部用于匹配，绝不离开函数。
func subsystemProvisioningErrorCode(err error) string {
	if err == nil {
		return "PROVISIONING_FAILED"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "disabled"):
		return "PROVISIONING_AGENT_DISABLED"
	case strings.Contains(message, "deployment helper is unavailable"), strings.Contains(message, "read deployment response"), strings.Contains(message, "send deployment request"), strings.Contains(message, "docker service"):
		return "PROVISIONING_AGENT_UNREACHABLE"
	case strings.Contains(message, "target is not allowed"), strings.Contains(message, "tenant is not allowed"):
		return "PROVISIONING_TARGET_NOT_ALLOWED"
	case strings.Contains(message, "requires an approved manifest checksum"):
		return "PROVISIONING_MANIFEST_CHECKSUM_REQUIRED"
	case strings.Contains(message, "preflight values are inconsistent"), strings.Contains(message, "integration values are inconsistent"), strings.Contains(message, "deployment request is invalid"):
		return "PROVISIONING_REQUEST_MISMATCH"
	case strings.Contains(message, "secrets are incomplete"), strings.Contains(message, "credentials are incomplete"), strings.Contains(message, "credential is incomplete"), strings.Contains(message, "generated runtime secret is invalid"), strings.Contains(message, "contract_summary"):
		return "PROVISIONING_CREDENTIALS_INCOMPLETE"
	case strings.Contains(message, "immutable digest"):
		return "PROVISIONING_IMAGE_NOT_RELEASED"
	case strings.Contains(message, "production deployment lock is unavailable"):
		return "PROVISIONING_DEPLOYMENT_LOCK_BUSY"
	case strings.Contains(message, "migrate production subsystem database"):
		return "PROVISIONING_MIGRATION_FAILED"
	case strings.Contains(message, "backup production subsystem database"), strings.Contains(message, "prepare production subsystem backup"):
		return "PROVISIONING_BACKUP_FAILED"
	case strings.Contains(message, "start production subsystem dependencies"), strings.Contains(message, "start production subsystem services"), strings.Contains(message, "start subsystem containers"), strings.Contains(message, "rebuild subsystem containers"), strings.Contains(message, "release environment is unavailable"), strings.Contains(message, "production environment is unavailable"):
		return "PROVISIONING_RUNTIME_UNHEALTHY"
	case strings.Contains(message, "unavailable"):
		return "PROVISIONING_DEPENDENCY_UNAVAILABLE"
	default:
		return "PROVISIONING_FAILED"
	}
}

// subsystemLifecycleFailureDetail 生成站内通知用的失败摘要：稳定错误码 + 脱敏短摘要。
// 安全（SEC-B9）：含 SQL 关键词的原文整体丢弃，文件路径替换为占位符并截断；
// 完整 cause 只允许出现在结构化日志里。
func subsystemLifecycleFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	code := subsystemProvisioningErrorCode(err)
	summary := redactInternalText(err.Error())
	if summary == "" {
		return code
	}
	return code + ": " + summary
}

// redactInternalText 剥离内部细节：SQL 关键词命中时返回空串（不保留任何原文），
// 文件路径替换为 [internal]，压缩空白并限制长度。
func redactInternalText(raw string) string {
	message := strings.TrimSpace(raw)
	if message == "" {
		return ""
	}
	if internalErrorDetailPattern.MatchString(message) {
		return ""
	}
	message = errorPathPattern.ReplaceAllString(message, "[internal]")
	message = strings.Join(strings.Fields(message), " ")
	const limit = 200
	if len(message) > limit {
		message = message[:limit] + "..."
	}
	return message
}

// sanitizeLifecycleNotificationDetail 是通知入库前的最后一道防线：即使调用方误传
// 错误原文，含 SQL 关键词或文件路径的详情也只保留稳定错误码（或占位符）并限制长度。
func sanitizeLifecycleNotificationDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	if internalErrorDetailPattern.MatchString(detail) || errorPathPattern.MatchString(detail) {
		if code, _, ok := strings.Cut(detail, ":"); ok && stableDetailCodePattern.MatchString(strings.TrimSpace(code)) {
			return strings.TrimSpace(code)
		}
		return "[内部错误已脱敏]"
	}
	const limit = 300
	if len(detail) > limit {
		return detail[:limit] + "..."
	}
	return detail
}

func subsystemProvisioningNextAction(err error, stages ...string) string {
	message := strings.ToLower(err.Error())
	diagnosis := "平台部署 Agent 或目标 API 暂时不可用"
	switch {
	case strings.Contains(message, "disabled"):
		diagnosis = "当前环境未启用受控部署 Agent；请升级并重新发布生产部署资产，确认 platform-api 与 subsystem-provisioner 均健康"
	case strings.Contains(message, "deployment helper is unavailable"), strings.Contains(message, "read deployment response"), strings.Contains(message, "send deployment request"):
		diagnosis = "平台 API 无法连接生产部署 Agent；请检查 subsystem-provisioner 状态和启动日志，并确认 Agent 与 platform-api 使用同一版本"
	case strings.Contains(message, "target is not allowed"):
		diagnosis = "该应用/环境未被当前 Agent 的审核清单允许，或 Agent 仍运行旧版本；请同步最新 subsystems.d 清单并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "requires an approved manifest checksum"):
		diagnosis = "该应用/环境不在服务器 subsystems.d 审核清单中，或 platform-api 与 subsystem-provisioner 的部署模式不一致；请同步清单并确认两个服务都使用 SUBSYSTEM_ONBOARDING_MODE=production 后一同重启"
	case strings.Contains(message, "tenant is not allowed"):
		diagnosis = "当前租户不是该服务器绑定的生产租户；请核对 SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID，禁止用其他租户覆盖现有实例"
	case strings.Contains(message, "preflight values are inconsistent"), strings.Contains(message, "integration values are inconsistent"), strings.Contains(message, "deployment request is invalid"):
		diagnosis = "页面提交的应用编码、环境、回调地址或上游地址与服务器审核清单不一致；请刷新应用接入页面并重新选择服务器接入目标"
	case strings.Contains(message, "infrastructure secrets are incomplete"):
		diagnosis = "服务器仍运行会在预检阶段检查全部基础设施密钥的旧版 Agent；请同步最新生产资产并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "subsystem database credentials are incomplete"):
		diagnosis = "当前子系统实际使用的数据库凭据仍为空或占位值；这些凭据可能已绑定持久化数据，Agent 不会自动生成或轮换，请完成一次性数据库初始化"
	case strings.Contains(message, "generated runtime secret is invalid"):
		diagnosis = "目标 runtime 中已有业务密钥不是合法的 32 字节 base64 值；为避免误轮换，Agent 不会覆盖非占位值，请先备份并修正该异常值"
	case strings.Contains(message, "runtime secrets are incomplete"):
		diagnosis = "目标 runtime 仍缺少必须由前置子系统接入产生的凭据；请先完成依赖子系统接入，再重试当前目标"
	case strings.Contains(message, "runtime template"), strings.Contains(message, "runtime directory"):
		diagnosis = "Agent 无法从随发布包审核的模板初始化 runtime；请确认最新 *.env.example 已部署且 runtime 目录可写，Agent 会自动创建文件并收紧为 0600"
	case strings.Contains(message, "production environment is unavailable"):
		diagnosis = "目标子系统 runtime 尚不可用；新版 Agent 会从审核模板自动初始化，请确认 platform-api、subsystem-provisioner 和生产部署资产版本一致"
	case strings.Contains(message, "release environment is unavailable"):
		diagnosis = "目标子系统的 .release.env 不可读或缺失；请确认部署目录的 .release.env 存在且权限为 0600"
	case strings.Contains(message, "immutable digest"):
		diagnosis = "该子系统镜像尚未发布到服务器：.release.env 中对应镜像仍是占位值。请在部署目录依次执行 ./bin/deploy.sh import packages/<组件>-*.tar.gz 与 ./bin/deploy.sh prepare <子系统>（import 只登记摘要，把不可变 digest 写入 .release.env 的是 prepare），再在当前环境点击“重试”"
	case strings.Contains(message, "initial administrator role"):
		diagnosis = "目标运行时已启动，但权限目录中缺少可用的初始角色；请检查目标 API 的目录同步日志"
	case strings.Contains(message, "production deployment file"), strings.Contains(message, "production deployment directory"), strings.Contains(message, "production compose configuration"):
		diagnosis = "服务器生产部署资产缺失、路径不规范或 Compose 校验失败；请重新发布平台生产部署资产并确认 Agent 健康"
	case strings.Contains(message, "contract_summary_client_id"), strings.Contains(message, "contract_summary_client_secret"), strings.Contains(message, "contract_summary_url"):
		diagnosis = "合同摘要校验已开启但运行时凭据不完整；重试不会从平台数据库恢复 OAuth 明文，请先同步包含合同摘要服务绑定的生产接入资产，或关闭合同校验后在当前环境重试"
	case strings.Contains(message, "contract_client_secret"):
		diagnosis = "客户与商机系统的合同签约用途凭据未写入 runtime；该键来自 service.contract_opportunity_signed_write。请先将基础平台更新到包含该用途受控重试下发修复的版本并重新部署 platform，再在原环境点击“重试”；不要手工写入 runtime 或重复创建应用环境"
	case strings.Contains(message, "runtime environment") || strings.Contains(message, "runtime configuration"):
		diagnosis = "Agent 无法安全更新目标运行配置；请确认 runtime 目录可写且文件不是符号链接，普通权限过宽会由 Agent 自动收紧为 0600"
	case strings.Contains(message, "start production subsystem dependencies"):
		diagnosis = "目标子系统的数据库或依赖服务未通过健康检查；请查看 subsystem-provisioner、目标数据库和依赖容器日志"
	case strings.Contains(message, "backup production subsystem database"), strings.Contains(message, "prepare production subsystem backup"):
		diagnosis = "目标子系统数据库备份失败；请检查数据库健康状态与 backups 目录权限"
	case strings.Contains(message, "migrate production subsystem database"):
		diagnosis = "目标子系统数据库迁移失败；请查看对应 migrate 容器日志并处理迁移错误，必要时使用接入前备份恢复"
	case strings.Contains(message, "start production subsystem services"):
		diagnosis = "目标 API 未能启动或通过健康检查；请查看目标 API 容器日志、runtime 配置和目录同步日志"
	case strings.Contains(message, "write production subsystem runtime configuration"):
		diagnosis = "Agent 无法安全写入目标 runtime 环境文件；请检查文件属主、0600 权限和 runtime 目录可写性"
	case strings.Contains(message, "production deployment lock is unavailable"):
		diagnosis = "服务器上已有发布或接入任务占用部署锁；请等待该任务结束，不要并行执行发布"
	case strings.Contains(message, "service credential is incomplete"), strings.Contains(message, "integration credential is incomplete"):
		diagnosis = "服务器审核清单引用的用途凭据尚未由平台控制面创建或交付；请同步最新平台镜像与清单，并确认 Agent 与 platform-api 版本一致"
	case strings.Contains(message, "compose file"):
		diagnosis = "部署 Agent 未找到生产 docker-compose.yml；请重新发布完整 platform/deploy/production 资产，并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "environment template"):
		diagnosis = "部署 Agent 未找到目标运行配置模板；请重新发布完整生产部署资产并确认 runtime/*.env 已初始化"
	case strings.Contains(message, "docker service"):
		diagnosis = "Docker 服务不可用；请启动 Docker 并确认部署 Agent 可以访问 Docker Socket"
	case strings.Contains(message, "start subsystem containers"), strings.Contains(message, "rebuild subsystem containers"):
		diagnosis = "子系统构建或启动失败；请查看 subsystem-provisioner 与目标 API 容器日志"
	}
	if len(stages) > 0 && stages[0] == "preflight" {
		return diagnosis + "；修复后重新提交本次接入。该失败发生在预检阶段，平台尚未创建应用环境"
	}
	return diagnosis + "；修复后在当前环境点击“重试”，不要重复创建应用环境"
}

// waitLocalRuntimeConfirmation 只对本地模式生效：本地开发允许在购买强制校验未完成时
// 先行部署，改为观察真实的消费者确认，而不是要求业务活动证明。
func (handler *SubsystemOnboardingHandler) waitLocalRuntimeConfirmation(ctx context.Context, job subsystemDeploymentJob) error {
	if len(job.Input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
	provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider)
	if !ok || provider.Capabilities().Mode != "local" {
		return nil
	}
	if handler.runtimeLicenses == nil {
		return application.ErrValidation
	}
	err := handler.runtimeLicenses.WaitForRuntimeConfirmation(ctx, job.ApplicationCode, job.Environment, job.Input.RuntimeLicenseCredentials)
	if err != nil {
		handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_CONFIRMATION_FAILED", "运行组件尚未完成当期授权确认；请检查组件同步状态后重试", err, false)
	}
	return err
}
