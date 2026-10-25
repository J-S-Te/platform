package http

import (
	"context"
	"errors"
	"fmt"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"log/slog"
	stdhttp "net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
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

// keycloakClientMappingInspector 刻意做成可选，旧测试与部署适配器因此保持兼容。
// 缺失按"映射未配置"报告；它绝不开任何切换门禁。
type keycloakClientMappingInspector interface {
	GetKeycloakClientMapping(context.Context, string, string, string) (KeycloakClientMapping, error)
}

// KeycloakSwitchGate 是非敏感、可审计的切换前置条件。值为 false 表示前置条件
// 未满足，或本服务器无法独立核验；客户端必须把两种情况都当作阻断。

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

// keycloakSwitchReadinessUpdater 刻意比校验器更窄：只有服务端同步成功
// 才能打开其前两道门禁。
type keycloakSwitchReadinessUpdater interface {
	MarkKeycloakClientAndRoleCatalogSynced(context.Context, string, string, string) error
}

// keycloakBrokerLoginVerificationRecorder 持久化由当前已认证会话的校验端点
// 提供的证据，供切换门禁审计。

// keycloakBrokerLoginVerificationRecorder 持久化由当前已认证会话的校验端点
// 提供的证据，供切换门禁审计。
type keycloakBrokerLoginVerificationRecorder interface {
	RecordBrokerLoginVerification(context.Context, KeycloakBrokerLoginVerification) error
}

// keycloakProjectionOperations 把死信管理收敛到 Keycloak 授权应用服务之后。
// 仅在组合根配置；接触不到任何 Secret、Keycloak Admin 凭据或任意队列变更方法。

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

// subsystemInitialAccessManager 把子系统专属授权放在应用登记之外管理。
// 角色编码为空表示该已登记子系统还没有受管角色目录。
type subsystemInitialAccessManager interface {
	AssignInitialAdministrator(context.Context, string, string, string, string) (string, error)
}

// keycloakBrokerProvisioner 创建 Keycloak 作为上游 broker 使用的唯一专用平台
// OAuth 客户端；其 Secret 绝不经 HTTP 暴露。

// keycloakBrokerProvisioner 创建 Keycloak 作为上游 broker 使用的唯一专用平台
// OAuth 客户端；其 Secret 绝不经 HTTP 暴露。
type keycloakBrokerProvisioner interface {
	EnsureKeycloakBroker(context.Context, string) (string, string, error)
	RecoverKeycloakBroker(context.Context, string, string) (string, string, error)
}

// keycloakControlPlaneOperations 只包含子系统生命周期用到的 Keycloak 管理动作。
// 保持该边界显式，使恢复行为无需真实 Keycloak 服务即可测试。

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

// subsystemNotificationSink 发送租户内站内通知，用于把子系统接入生命周期结果通知给操作人。
// 该接口由基础平台实现，子系统代码无需改动；nil 时处理器保持轻量测试可用。
type subsystemNotificationSink interface {
	SendSubsystemLifecycle(ctx context.Context, input SubsystemLifecycleNotification) error
}

// SubsystemLifecycleNotification 描述一次子系统接入/重试/更新的结果通知，供装配层适配器使用。

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

// ConfigureKeycloakTransportPolicy 控制未来切换是否强制 HTTPS。false 刻意兼容
// 当前 HTTP 部署；preflight 仍由公开 origin 推导 Redirect URI 与安全 Cookie，
// 使三项设置不会漂移。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakTransportPolicy(requireHTTPS bool) {
	handler.keycloakRequireHTTPS = requireHTTPS
}

// ConfigureDefaultIssuerAlias 控制接入请求缺省 issuer_alias 时使用的提供方；
// 用户的显式选择始终优先于该默认值。

// ConfigureDefaultIssuerAlias 控制接入请求缺省 issuer_alias 时使用的提供方；
// 用户的显式选择始终优先于该默认值。
func (handler *SubsystemOnboardingHandler) ConfigureDefaultIssuerAlias(alias string) {
	handler.defaultIssuerAlias = strings.ToLower(strings.TrimSpace(alias))
}

// ConfigureKeycloakControlPlane 仅允许组合根调用；浏览器永远拿不到该对象
// 及其 Keycloak 管理凭据。

// ConfigureKeycloakControlPlane 仅允许组合根调用；浏览器永远拿不到该对象
// 及其 Keycloak 管理凭据。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakControlPlane(control keycloakControlPlaneOperations) {
	handler.keycloakControl = control
}

// ConfigureKeycloakBroker 挂接 Keycloak broker 专用 OAuth 客户端的创建与恢复能力。
// 仅组合根可调用；未挂接时 Keycloak 同步与切换一律不可用（失败关闭）。

// ConfigureKeycloakBroker 挂接 Keycloak broker 专用 OAuth 客户端的创建与恢复能力。
// 仅组合根可调用；未挂接时 Keycloak 同步与切换一律不可用（失败关闭）。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakBroker(provisioner keycloakBrokerProvisioner) {
	handler.keycloakBroker = provisioner
}

// reconcileKeycloakBroker 保持 Keycloak IdP 与其平台 OAuth 客户端一致。
// 既有有效凭据刻意没有可取回的明文，因此 IdP 健康时只做核验；
// 只有 Keycloak 确定性地报告 IdP 配置不完整时才生成新 Secret——
// 连接、认证与权限错误保持失败关闭，绝不触发凭据轮换。

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

// ConfigureKeycloakAuthorizationCatalog 挂接应用角色目录的读取来源；
// 未挂接时 Client 同步与 Keycloak 投影不可用。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakAuthorizationCatalog(catalog keycloakAuthorizationCatalog) {
	handler.keycloakCatalog = catalog
}

// ConfigureKeycloakClientMappingStore 挂接 Client 映射持久化与授权投影回填；
// 未挂接时同步请求按校验失败拒绝，不产生任何 Keycloak 侧写入。

// ConfigureKeycloakClientMappingStore 挂接 Client 映射持久化与授权投影回填；
// 未挂接时同步请求按校验失败拒绝，不产生任何 Keycloak 侧写入。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakClientMappingStore(store keycloakClientMappingStore) {
	handler.keycloakMappings = store
}

// ConfigureKeycloakSwitchReadinessInspector 挂接权威的服务端投影与 broker 登录
// 证据来源。为向后兼容做成可选；未挂接时所有 Keycloak 切换保持关闭。

// ConfigureKeycloakSwitchReadinessInspector 挂接权威的服务端投影与 broker 登录
// 证据来源。为向后兼容做成可选；未挂接时所有 Keycloak 切换保持关闭。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakSwitchReadinessInspector(inspector keycloakSwitchReadinessInspector) {
	handler.keycloakReadiness = inspector
}

// ConfigureKeycloakProjectionOperations 启用租户范围内的 FAILED 投影检查与受控重放。
// 传 nil 时运维端点刻意保持不可用，而不是暴露一条不完整的恢复路径。

// ConfigureKeycloakProjectionOperations 启用租户范围内的 FAILED 投影检查与受控重放。
// 传 nil 时运维端点刻意保持不可用，而不是暴露一条不完整的恢复路径。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakProjectionOperations(operations keycloakProjectionOperations) {
	handler.keycloakOperations = operations
}

// ConfigureKeycloakCutoverLifecycle 挂接持久化的按环境七天观察期与回滚窗口控制面。

// ConfigureKeycloakCutoverLifecycle 挂接持久化的按环境七天观察期与回滚窗口控制面。
func (handler *SubsystemOnboardingHandler) ConfigureKeycloakCutoverLifecycle(store keycloakCutoverLifecycleStore) {
	handler.keycloakCutover = store
}

// ConfigureSubsystemServiceCredentials 为旧版本创建的环境启用新用途绑定机器凭据的
// 幂等迁移。

// ConfigureSubsystemServiceCredentials 为旧版本创建的环境启用新用途绑定机器凭据的
// 幂等迁移。
func (handler *SubsystemOnboardingHandler) ConfigureSubsystemServiceCredentials(manager subsystemServiceCredentialManager) {
	handler.serviceCredentials = manager
}

// 仅组合根可启用商业授权登记；普通 HTTP 请求无法提供组件审批或另选凭据来源。

// 仅组合根可启用商业授权登记；普通 HTTP 请求无法提供组件审批或另选凭据来源。
func (handler *SubsystemOnboardingHandler) ConfigureRuntimeLicenseEnrollment(service *application.RuntimeLicenseEnrollmentService) {
	handler.runtimeLicenses = service
}

// ConfigureRuntimeLicenseSettings 挂接运行时授权所需的实例级设置读取。仅组合根可
// 调用；商业授权登记已启用而该读取缺失或失败时，部署编排按
// LICENSE_RUNTIME_BINDING_FAILED 收口失败，绝不无设置放行。

// ConfigureRuntimeLicenseSettings 挂接运行时授权所需的实例级设置读取。仅组合根可
// 调用；商业授权登记已启用而该读取缺失或失败时，部署编排按
// LICENSE_RUNTIME_BINDING_FAILED 收口失败，绝不无设置放行。
func (handler *SubsystemOnboardingHandler) ConfigureRuntimeLicenseSettings(provider func(context.Context) (application.RuntimeLicenseSettings, error)) {
	handler.runtimeLicenseSettings = provider
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

// NewSubsystemOnboardingHandler 构造子系统接入 HTTP 适配器。可选状态存储保持
// 轻量调用方/测试兼容；生产装配传入持久化控制面仓储。
func NewSubsystemOnboardingHandler(service subsystemOnboardingService, provisioner application.SubsystemProvisioner, access subsystemInitialAccessManager, oidcIssuer string, logger *slog.Logger, stateStores ...application.SubsystemDeploymentStateStore) (*SubsystemOnboardingHandler, error) {
	return NewSubsystemOnboardingHandlerWithNotifications(service, provisioner, access, oidcIssuer, logger, nil, stateStores...)
}

// NewSubsystemOnboardingHandlerWithNotifications 额外注入站内通知发送器（可选）。

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

// allowedServiceBindingsForTarget 返回服务器清单为该应用/环境声明的服务用途白名单；
// 未声明时返回 nil，应用层回退到平台硬编码默认。

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
