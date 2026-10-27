package bootstrap

import (
	"context"
	"errors"
	registry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	registryhttp "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/interfaces/http"
	"testing"
)

type accessBrowserProviderStub string

func (s accessBrowserProviderStub) ResolveEnvironmentIssuerAlias(context.Context, string, string, string) (string, error) {
	return string(s), nil
}

type accessBrowserKeycloakStub struct{ fail bool }

func (s accessBrowserKeycloakStub) EnsureSubsystemAccessBrowserCredential(_ context.Context, id, redirect string) (registry.SubsystemUpdateBrowserCredential, error) {
	if s.fail {
		return registry.SubsystemUpdateBrowserCredential{}, errors.New("keycloak unavailable")
	}
	return registry.SubsystemUpdateBrowserCredential{ClientID: id, ClientSecret: "existing-keycloak-secret"}, nil
}

type accessBrowserMappingStub struct{}

func (accessBrowserMappingStub) ResolveEffectiveKeycloakClient(context.Context, string, string, string, string) (registryhttp.KeycloakClientResolution, error) {
	return registryhttp.KeycloakClientResolution{ClientID: "existing-compatible-client"}, nil
}
func TestAccessBrowserCredentialsPreserveKeycloakIdentity(t *testing.T) {
	op := registry.SubsystemAccessOperation{TenantID: "tenant", ApplicationID: "app", EnvironmentID: "env", ApplicationCode: "customer_portal", EnvironmentCode: "prod", AuthProvider: "keycloak"}
	port := subsystemAccessBrowserCredentialPort{provider: accessBrowserProviderStub("keycloak"), keycloak: accessBrowserKeycloakStub{}, mappings: accessBrowserMappingStub{}}
	credential, err := port.EnsureForProvider(context.Background(), op, "http://platform.example/customer-portal/auth/callback")
	if err != nil || credential.ClientID != "existing-compatible-client" || credential.ClientSecret != "existing-keycloak-secret" {
		t.Fatalf("wrong RP credential: %v", err)
	}
	port.keycloak = accessBrowserKeycloakStub{fail: true}
	if _, err := port.EnsureForProvider(context.Background(), op, "http://platform.example/callback"); err == nil {
		t.Fatal("failed Keycloak read must not fall back to platform credentials")
	}
	port.provider = accessBrowserProviderStub("platform")
	if _, err := port.EnsureForProvider(context.Background(), op, "http://platform.example/callback"); err == nil {
		t.Fatal("concurrent provider change accepted")
	}
}
