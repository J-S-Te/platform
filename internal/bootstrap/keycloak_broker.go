package bootstrap

import (
	"context"
	"fmt"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// keycloakBrokerRegistrar 将平台 OAuth 客户端生命周期限制在平台控制面内。
// Keycloak 只在内存中接收密钥；密钥不会序列化到浏览器或环境文件。
type keycloakBrokerRegistrar struct {
	applications *application.ManagementService
	oauth        *application.OAuthClientManagementService
	publicURL    string
	realm        string
	environment  string
}

const (
	platformBrokerClientID       = "keycloak-broker"
	customerPortalBrokerClientID = "keycloak-customer-portal-broker"
)

func keycloakBrokerEnvironment(appEnvironment string) string {
	if appEnvironment == "production" {
		return "prod"
	}
	return "dev"
}

func (registrar keycloakBrokerRegistrar) EnsureKeycloakBroker(ctx context.Context, tenantID string) (string, string, error) {
	return registrar.ensureBrokerClient(ctx, tenantID, "platform", platformBrokerClientID, "Keycloak Broker")
}

// EnsureCustomerPortalBroker provisions a separate upstream OAuth client for
// external customer authentication. It must not share the platform Broker
// client: the OAuth authorization resolver derives claims from the client's
// application, so sharing it would make either employees or customers fail
// closed in the other application's login flow.
func (registrar keycloakBrokerRegistrar) EnsureCustomerPortalBroker(ctx context.Context, tenantID string) (string, string, error) {
	return registrar.ensureBrokerClient(ctx, tenantID, "customer_portal", customerPortalBrokerClientID, "Customer Portal Keycloak Broker")
}

func (registrar keycloakBrokerRegistrar) ensureBrokerClient(ctx context.Context, tenantID, applicationCode, clientID, clientName string) (string, string, error) {
	apps, err := registrar.applications.ListApplications(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 100})
	if err != nil {
		return "", "", err
	}
	var targetApplication application.Application
	for _, item := range apps.Items {
		if item.Code == applicationCode {
			targetApplication = item
			break
		}
	}
	if targetApplication.ID == "" {
		return "", "", fmt.Errorf("%s application is not registered", applicationCode)
	}
	envs, err := registrar.applications.ListEnvironments(ctx, tenantID, targetApplication.ID, application.PageRequest{Page: 1, PageSize: 100})
	if err != nil {
		return "", "", err
	}
	var environment application.Environment
	environmentCode := registrar.environment
	if environmentCode == "" {
		environmentCode = "dev"
	}
	for _, item := range envs.Items {
		if item.Environment == environmentCode {
			environment = item
			break
		}
	}
	if environment.ID == "" {
		return "", "", fmt.Errorf("%s %s environment is not registered", applicationCode, environmentCode)
	}
	brokerAlias := "basic-platform"
	if applicationCode == "customer_portal" {
		brokerAlias = "basic-platform-customer"
	}
	redirect := registrar.publicURL + "/realms/" + registrar.realm + "/broker/" + brokerAlias + "/endpoint"
	clients, err := registrar.oauth.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return "", "", err
	}
	for _, client := range clients {
		if client.ClientID == clientID {
			// Keycloak 只保留一个当前密钥。每次 Worker 重启都新增活跃凭据，会让
			// Token 校验遍历无界增长的 bcrypt 摘要列表，最终超过 Keycloak 上游
			// HTTP 超时；因此轮换时不保留重叠有效期。
			secret, rotateErr := registrar.oauth.RotateOAuthClientSecret(ctx, application.OAuthClientSecretRotateInput{
				TenantID: tenantID, OAuthClientID: client.ID, OperatorID: "system-keycloak", OverlapSeconds: 0,
			})
			if rotateErr != nil {
				return "", "", fmt.Errorf("rotate keycloak broker secret: %w", rotateErr)
			}
			return client.ClientID, secret.PlaintextSecret, nil
		}
	}
	created, err := registrar.oauth.CreateOAuthClient(ctx, application.OAuthClientCreateInput{TenantID: tenantID, ApplicationID: targetApplication.ID, EnvironmentID: environment.ID, OperatorID: "system-keycloak", ClientID: clientID, ClientName: clientName, ClientType: "confidential", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 900, RefreshTokenTTLSeconds: 2592000, GrantTypes: []string{"authorization_code", "refresh_token"}, RequirePKCE: false, Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirect}})
	if err != nil {
		return "", "", fmt.Errorf("create keycloak broker OAuth client: %w", err)
	}
	return created.Client.ClientID, created.PlaintextSecret, nil
}
