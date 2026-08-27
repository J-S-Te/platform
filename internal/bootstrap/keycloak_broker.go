package bootstrap

import (
	"context"
	"errors"
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

// RecoverKeycloakBroker rotates the platform Broker client through the OAuth
// control plane when Keycloak has lost its non-retrievable client secret. The
// plaintext exists only for the immediate IdP reconciliation and is never
// persisted or logged.
func (registrar keycloakBrokerRegistrar) RecoverKeycloakBroker(ctx context.Context, tenantID, operatorID string) (string, string, error) {
	return registrar.recoverBrokerClient(ctx, tenantID, operatorID, platformBrokerClientID)
}

// EnsureCustomerPortalBroker provisions a separate upstream OAuth client for
// external customer authentication. It must not share the platform Broker
// client: the OAuth authorization resolver derives claims from the client's
// application, so sharing it would make either employees or customers fail
// closed in the other application's login flow.
func (registrar keycloakBrokerRegistrar) EnsureCustomerPortalBroker(ctx context.Context, tenantID string) (string, string, error) {
	return registrar.ensureBrokerClient(ctx, tenantID, "customer_portal", customerPortalBrokerClientID, "Customer Portal Keycloak Broker")
}

func (registrar keycloakBrokerRegistrar) recoverBrokerClient(ctx context.Context, tenantID, operatorID, clientID string) (string, string, error) {
	existing, err := registrar.oauth.GetOAuthClientByClientID(ctx, tenantID, clientID)
	if err != nil {
		return "", "", fmt.Errorf("lookup keycloak broker OAuth client for recovery: %w", err)
	}
	secret, err := registrar.oauth.RotateOAuthClientSecret(ctx, application.OAuthClientSecretRotateInput{
		TenantID: tenantID, OAuthClientID: existing.ID, OperatorID: operatorID,
		// Keep the previous credential briefly valid until Keycloak receives the
		// new one. This makes a transient Keycloak Admin failure non-disruptive.
		OverlapSeconds: 300,
	})
	if err != nil {
		return "", "", fmt.Errorf("rotate keycloak broker OAuth client secret: %w", err)
	}
	return existing.ClientID, secret.PlaintextSecret, nil
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
	// Use direct client_id lookup instead of full tenant scan to avoid
	// O(n) query on every worker restart when there are many OAuth clients.
	existing, lookupErr := registrar.oauth.GetOAuthClientByClientID(ctx, tenantID, clientID)
	if lookupErr == nil {
		// Only rotate if the client has no active credentials. Previously every
		// worker restart would rotate, accumulating bcrypt hashes in Keycloak
		// and invalidating the IdP config mid-flight. Now we trust that an
		// existing active credential is still valid; the Broker reconciliation
		// in EnsureBroker will detect and repair a broken IdP config.
		hasActiveCredential := false
		for _, cred := range existing.Credentials {
			if cred.Status == "ACTIVE" && cred.RevokedAt == nil {
				hasActiveCredential = true
				break
			}
		}
		if !hasActiveCredential {
			secret, createErr := registrar.oauth.CreateOAuthClientSecret(ctx, application.OAuthClientSecretCreateInput{
				TenantID: tenantID, OAuthClientID: existing.ID, OperatorID: "system-keycloak",
			})
			if createErr != nil {
				return "", "", fmt.Errorf("create keycloak broker secret: %w", createErr)
			}
			return existing.ClientID, secret.PlaintextSecret, nil
		}
		return existing.ClientID, "", nil
	}
	if !errors.Is(lookupErr, application.ErrManagementNotFound) {
		return "", "", fmt.Errorf("lookup keycloak broker OAuth client: %w", lookupErr)
	}
	created, err := registrar.oauth.CreateOAuthClient(ctx, application.OAuthClientCreateInput{TenantID: tenantID, ApplicationID: targetApplication.ID, EnvironmentID: environment.ID, OperatorID: "system-keycloak", ClientID: clientID, ClientName: clientName, ClientType: "confidential", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 900, RefreshTokenTTLSeconds: 2592000, GrantTypes: []string{"authorization_code", "refresh_token"}, RequirePKCE: false, Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirect}})
	if err != nil {
		return "", "", fmt.Errorf("create keycloak broker OAuth client: %w", err)
	}
	return created.Client.ClientID, created.PlaintextSecret, nil
}
