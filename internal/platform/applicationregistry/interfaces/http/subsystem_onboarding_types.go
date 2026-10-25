package http

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	stdhttp "net/http"
	"time"
)

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

const subsystemDeploymentHTTPTimeout = 16 * time.Minute

// 失败状态写入必须能在浏览器断连或反向代理超时后完成。保持较短的超时，让降级的
// 数据库不能无限占住请求 handler，同时部署生命周期仍能到达终态。

// 失败状态写入必须能在浏览器断连或反向代理超时后完成。保持较短的超时，让降级的
// 数据库不能无限占住请求 handler，同时部署生命周期仍能到达终态。
const subsystemDeploymentFailureWriteTimeout = 10 * time.Second

// 部署状态会在长耗时 Agent 调用之前写入。若 API 进程在该调用期间被终止，
// 状态不能永远停留在非终态：管理 UI 只对失败的尝试暴露重试。该阈值刻意略高于
// 同步请求超时，使真正缓慢但仍存活的部署不会被状态轮询打断。

// 部署状态会在长耗时 Agent 调用之前写入。若 API 进程在该调用期间被终止，
// 状态不能永远停留在非终态：管理 UI 只对失败的尝试暴露重试。该阈值刻意略高于
// 同步请求超时，使真正缓慢但仍存活的部署不会被状态轮询打断。
const subsystemDeploymentStaleAfter = application.SubsystemDeploymentStaleAfter

// extendSubsystemDeploymentWriteDeadline 让同步控制面请求在 Agent 有界的 15 分钟
// 部署窗口内保持存活。ResponseController 解包 Gin writer 直达底层网络连接；
// httptest 与不支持的 writer 安全忽略该能力。

// extendSubsystemDeploymentWriteDeadline 让同步控制面请求在 Agent 有界的 15 分钟
// 部署窗口内保持存活。ResponseController 解包 Gin writer 直达底层网络连接；
// httptest 与不支持的 writer 安全忽略该能力。
func extendSubsystemDeploymentWriteDeadline(writer stdhttp.ResponseWriter) {
	controller := stdhttp.NewResponseController(writer)
	_ = controller.SetWriteDeadline(time.Now().Add(subsystemDeploymentHTTPTimeout))
}
