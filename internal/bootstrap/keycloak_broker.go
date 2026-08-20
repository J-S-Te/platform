package bootstrap

import (
	"context"
	"fmt"

	application "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// keycloakBrokerRegistrar keeps the platform OAuth client lifecycle inside the
// platform control plane. Keycloak receives its secret directly in memory; the
// secret is never serialized to the browser or an environment file.
type keycloakBrokerRegistrar struct {
	applications *application.ManagementService
	oauth        *application.OAuthClientManagementService
	publicURL    string
	realm        string
}

func (registrar keycloakBrokerRegistrar) EnsureKeycloakBroker(ctx context.Context, tenantID string) (string, string, error) {
	apps, err := registrar.applications.ListApplications(ctx, tenantID, application.PageRequest{Page: 1, PageSize: 100})
	if err != nil {
		return "", "", err
	}
	var platform application.Application
	for _, item := range apps.Items {
		if item.Code == "platform" {
			platform = item
			break
		}
	}
	if platform.ID == "" {
		return "", "", fmt.Errorf("platform application is not registered")
	}
	envs, err := registrar.applications.ListEnvironments(ctx, tenantID, platform.ID, application.PageRequest{Page: 1, PageSize: 100})
	if err != nil {
		return "", "", err
	}
	var environment application.Environment
	for _, item := range envs.Items {
		if item.Environment == "dev" {
			environment = item
			break
		}
	}
	if environment.ID == "" {
		return "", "", fmt.Errorf("platform dev environment is not registered")
	}
	redirect := registrar.publicURL + "/realms/" + registrar.realm + "/broker/basic-platform/endpoint"
	clients, err := registrar.oauth.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return "", "", err
	}
	for _, client := range clients {
		if client.ClientID == "keycloak-broker" {
			// Keycloak uses one current secret. Creating an additional active
			// credential on every worker restart makes token validation walk an
			// unbounded list of bcrypt hashes and eventually exceed Keycloak's
			// upstream HTTP timeout. Rotate with zero overlap instead.
			secret, rotateErr := registrar.oauth.RotateOAuthClientSecret(ctx, application.OAuthClientSecretRotateInput{
				TenantID: tenantID, OAuthClientID: client.ID, OperatorID: "system-keycloak", OverlapSeconds: 0,
			})
			if rotateErr != nil {
				return "", "", fmt.Errorf("rotate keycloak broker secret: %w", rotateErr)
			}
			return client.ClientID, secret.PlaintextSecret, nil
		}
	}
	created, err := registrar.oauth.CreateOAuthClient(ctx, application.OAuthClientCreateInput{TenantID: tenantID, ApplicationID: platform.ID, EnvironmentID: environment.ID, OperatorID: "system-keycloak", ClientID: "keycloak-broker", ClientName: "Keycloak Broker", ClientType: "confidential", TokenAuthMethod: "client_secret_basic", AccessTokenTTLSeconds: 900, RefreshTokenTTLSeconds: 2592000, GrantTypes: []string{"authorization_code", "refresh_token"}, RequirePKCE: false, Scopes: []string{"openid", "profile"}, RedirectURIs: []string{redirect}})
	if err != nil {
		return "", "", fmt.Errorf("create keycloak broker OAuth client: %w", err)
	}
	return created.Client.ClientID, created.PlaintextSecret, nil
}
