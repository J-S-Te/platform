package bootstrap

import (
	"context"
	"fmt"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	keycloakauthorizationinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/infrastructure"
)

type keycloakClientStartupReconcileDependencies struct {
	markPending   func(context.Context, string, string, string) error
	ensureClient  func(context.Context, string, string, string) (string, error)
	listRoleCodes func(context.Context, string, string) ([]string, error)
	ensureRoles   func(context.Context, string, []string) error
	saveMapping   func(context.Context, string, string, string, string, string) error
	markSynced    func(context.Context, string, string, string) error
}

func reconcileStoredKeycloakClient(
	ctx context.Context,
	mapping keycloakauthorizationinfrastructure.StoredKeycloakClientMapping,
	requireHTTPS bool,
	dependencies keycloakClientStartupReconcileDependencies,
) error {
	scope := fmt.Sprintf("%s/%s/%s", mapping.TenantID, mapping.ApplicationID, mapping.EnvironmentID)
	if err := dependencies.markPending(ctx, mapping.TenantID, mapping.ApplicationID, mapping.EnvironmentID); err != nil {
		return fmt.Errorf("invalidate stored Keycloak Client readiness %s: %w", scope, err)
	}
	transport, err := applicationregistryapplication.ValidateKeycloakCutoverTransport(mapping.BaseURL, mapping.PathPrefix, requireHTTPS)
	if err != nil {
		return fmt.Errorf("validate stored Keycloak Client transport %s: %w", scope, err)
	}
	clientID, err := dependencies.ensureClient(ctx, mapping.ClientID, mapping.ApplicationName, transport.RedirectURI)
	if err != nil {
		return fmt.Errorf("reconcile stored Keycloak Client %s: %w", scope, err)
	}
	roleCodes, err := dependencies.listRoleCodes(ctx, mapping.TenantID, mapping.ApplicationID)
	if err != nil {
		return fmt.Errorf("load stored Keycloak Client roles %s: %w", scope, err)
	}
	if err := dependencies.ensureRoles(ctx, clientID, roleCodes); err != nil {
		return fmt.Errorf("reconcile stored Keycloak Client roles %s: %w", scope, err)
	}
	if err := dependencies.saveMapping(ctx, mapping.TenantID, mapping.ApplicationID, mapping.EnvironmentID, mapping.Realm, clientID); err != nil {
		return fmt.Errorf("save reconciled Keycloak Client mapping %s: %w", scope, err)
	}
	if err := dependencies.markSynced(ctx, mapping.TenantID, mapping.ApplicationID, mapping.EnvironmentID); err != nil {
		return fmt.Errorf("mark reconciled Keycloak Client ready %s: %w", scope, err)
	}
	return nil
}
