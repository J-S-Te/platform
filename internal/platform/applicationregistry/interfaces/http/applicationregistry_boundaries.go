package http

import stdhttp "net/http"

// KeycloakIntegrationUseCases 定义认证集成 HTTP 适配器依赖的最小用例边界。
//
// 该接口刻意只包含 HTTP 适配器需要的行为，不把整个应用接入 Handler 暴露给
// KeycloakIntegrationHandler。后续可以将 Broker、客户端同步、投影运维和切换
// 生命周期分别替换为独立应用服务，而不会改变路由层的响应协议。
type KeycloakIntegrationUseCases interface {
	GetSubsystemCapabilities(stdhttp.ResponseWriter, *stdhttp.Request)
	GetKeycloakIntegrationStatus(stdhttp.ResponseWriter, *stdhttp.Request)
	SyncKeycloakClient(stdhttp.ResponseWriter, *stdhttp.Request)
	SwitchToKeycloak(stdhttp.ResponseWriter, *stdhttp.Request)
	RollbackToPlatform(stdhttp.ResponseWriter, *stdhttp.Request)
	ListKeycloakProjectionFailures(stdhttp.ResponseWriter, *stdhttp.Request)
	GetKeycloakProjectionAlerts(stdhttp.ResponseWriter, *stdhttp.Request)
	ReplayKeycloakProjectionFailure(stdhttp.ResponseWriter, *stdhttp.Request)
	GetKeycloakSyncStatus(stdhttp.ResponseWriter, *stdhttp.Request)
	GetSubsystemHealthDashboard(stdhttp.ResponseWriter, *stdhttp.Request)
	StartKeycloakObservation(stdhttp.ResponseWriter, *stdhttp.Request)
	VerifyKeycloakBrokerLogin(stdhttp.ResponseWriter, *stdhttp.Request)
}

// BrokerHTTPHandler 是 Broker 登录验证边界，避免路由层依赖完整应用接入 Handler。
// Broker 凭据、Keycloak 管理连接和应用接入编排属于不同生命周期，不能在此接口混用。
type BrokerHTTPHandler interface {
	VerifyKeycloakBrokerLogin(stdhttp.ResponseWriter, *stdhttp.Request)
}

// DeploymentStatusHTTPHandler 是部署状态查询边界。部署状态只读，不应隐式触发
// 容器重建、凭据轮换或 Keycloak 配置写入。
type DeploymentStatusHTTPHandler interface {
	GetSubsystemStatus(stdhttp.ResponseWriter, *stdhttp.Request)
	GetSubsystemHealthDashboard(stdhttp.ResponseWriter, *stdhttp.Request)
	GetKeycloakSyncStatus(stdhttp.ResponseWriter, *stdhttp.Request)
}

// OAuthClientHTTPHandler 是 OAuth Client 管理边界，供组合根按能力注册路由。
// 具体客户端权限和租户校验仍由 OAuthClientManagementHandler 内部完成。
type OAuthClientHTTPHandler interface {
	ListOAuthClients(stdhttp.ResponseWriter, *stdhttp.Request)
	CreateOAuthClient(stdhttp.ResponseWriter, *stdhttp.Request)
	GetOAuthClient(stdhttp.ResponseWriter, *stdhttp.Request)
	UpdateOAuthClientScopes(stdhttp.ResponseWriter, *stdhttp.Request)
	UpdateOAuthClientRedirectURIs(stdhttp.ResponseWriter, *stdhttp.Request)
	GetOAuthClientPostLogoutRedirectURIs(stdhttp.ResponseWriter, *stdhttp.Request)
	UpdateOAuthClientPostLogoutRedirectURIs(stdhttp.ResponseWriter, *stdhttp.Request)
	GetOAuthClientJWKs(stdhttp.ResponseWriter, *stdhttp.Request)
	UpdateOAuthClientJWKs(stdhttp.ResponseWriter, *stdhttp.Request)
	DisableOAuthClient(stdhttp.ResponseWriter, *stdhttp.Request)
	CreateCredential(stdhttp.ResponseWriter, *stdhttp.Request)
	RotateCredential(stdhttp.ResponseWriter, *stdhttp.Request)
	DisableCredential(stdhttp.ResponseWriter, *stdhttp.Request)
}

var _ KeycloakIntegrationUseCases = (*SubsystemOnboardingHandler)(nil)
var _ BrokerHTTPHandler = (*SubsystemOnboardingHandler)(nil)
var _ DeploymentStatusHTTPHandler = (*SubsystemOnboardingHandler)(nil)
var _ OAuthClientHTTPHandler = (*OAuthClientManagementHandler)(nil)
