package bootstrap

import (
	"context"
	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	registryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	"strings"
)

type subsystemAccessBrowserCredentialPort struct {
	provider interface {
		ResolveEnvironmentIssuerAlias(context.Context, string, string, string) (string, error)
	}
	platform interface {
		Ensure(context.Context, string, string, string, string, string, string, string) (registry.SubsystemUpdateBrowserCredential, error)
		ReadSubsystemAccessBrowserClientID(context.Context, string, string, string, string, string) (string, error)
	}
	mappings interface {
		ResolveEffectiveKeycloakClient(context.Context, string, string, string, string) (registryhttp.KeycloakClientResolution, error)
	}
	keycloak interface {
		EnsureSubsystemAccessBrowserCredential(context.Context, string, string) (registry.SubsystemUpdateBrowserCredential, error)
	}
}

func (p *subsystemAccessBrowserCredentialPort) EnsureForProvider(ctx context.Context, op registry.SubsystemAccessOperation, redirect string) (registry.SubsystemUpdateBrowserCredential, error) {
	provider, err := p.provider.ResolveEnvironmentIssuerAlias(ctx, op.TenantID, op.ApplicationCode, op.EnvironmentCode)
	if err != nil {
		return registry.SubsystemUpdateBrowserCredential{}, err
	}
	if provider != op.AuthProvider {
		return registry.SubsystemUpdateBrowserCredential{}, registry.ErrConflict
	}
	if provider == "platform" {
		if p.platform == nil {
			return registry.SubsystemUpdateBrowserCredential{}, registry.ErrSubsystemAccessStageNotWired
		}
		return p.platform.Ensure(ctx, op.TenantID, op.OperatorID, op.ApplicationID, op.EnvironmentID, op.ApplicationCode, op.EnvironmentCode, redirect)
	}
	if provider != "keycloak" || p.keycloak == nil {
		return registry.SubsystemUpdateBrowserCredential{}, registry.ErrSubsystemAccessStageNotWired
	}
	id, err := p.keycloakClientID(ctx, op.TenantID, op.ApplicationID, op.EnvironmentID, op.ApplicationCode, op.EnvironmentCode)
	if err != nil {
		return registry.SubsystemUpdateBrowserCredential{}, err
	}
	return p.keycloak.EnsureSubsystemAccessBrowserCredential(ctx, id, redirect)
}
func (p *subsystemAccessBrowserCredentialPort) keycloakClientID(ctx context.Context, tenant, appID, envID, app, env string) (string, error) {
	if p.mappings == nil {
		return "", registry.ErrSubsystemAccessStageNotWired
	}
	canonical := strings.ToLower(app) + "-" + strings.ToLower(env) + "-web"
	resolution, err := p.mappings.ResolveEffectiveKeycloakClient(ctx, tenant, appID, envID, canonical)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(resolution.ClientID) == "" {
		return "", registry.ErrSubsystemProvisioningUnavailable
	}
	return resolution.ClientID, nil
}
func (p *subsystemAccessBrowserCredentialPort) ReadSubsystemAccessBrowserClientID(ctx context.Context, tenant, appID, envID, app, env string) (string, error) {
	provider, err := p.provider.ResolveEnvironmentIssuerAlias(ctx, tenant, app, env)
	if err != nil {
		return "", err
	}
	if provider == "keycloak" {
		return p.keycloakClientID(ctx, tenant, appID, envID, app, env)
	}
	if provider != "platform" || p.platform == nil {
		return "", registry.ErrSubsystemAccessStageNotWired
	}
	return p.platform.ReadSubsystemAccessBrowserClientID(ctx, tenant, appID, envID, app, env)
}
