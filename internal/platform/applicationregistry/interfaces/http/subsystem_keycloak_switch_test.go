package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
)

func TestKeycloakCapabilitiesExposeFourFailClosedSwitchGates(t *testing.T) {
	t.Parallel()
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"https://portal.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.ConfigureKeycloak(true, "https://sso.example.com", "basic-platform")
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/subsystem-capabilities", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.GetSubsystemCapabilities(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			AuthenticationProviders []struct {
				Alias       string               `json:"alias"`
				SwitchReady bool                 `json:"switch_ready"`
				SwitchGates []KeycloakSwitchGate `json:"switch_gates"`
			} `json:"authentication_providers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	keycloakIndex := -1
	for index, provider := range envelope.Data.AuthenticationProviders {
		if provider.Alias == "keycloak" {
			keycloakIndex = index
			break
		}
	}
	if keycloakIndex < 0 {
		t.Fatalf("Keycloak provider missing: %#v", envelope.Data.AuthenticationProviders)
	}
	keycloak := envelope.Data.AuthenticationProviders[keycloakIndex]
	if keycloak.SwitchReady || len(keycloak.SwitchGates) != 4 {
		t.Fatalf("unexpected Keycloak gate contract: %#v", keycloak)
	}
	for _, gate := range keycloak.SwitchGates {
		if gate.Passed || gate.NextAction == "" {
			t.Fatalf("unverified gate must remain blocked and actionable: %#v", gate)
		}
	}
}

func TestUpdateSubsystemRejectsKeycloakWithoutAuthoritativeGateEvidence(t *testing.T) {
	t.Parallel()
	provisioner := &recordingHTTPSubsystemProvisioner{capabilities: application.SubsystemProvisioningCapabilities{Enabled: true}}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{},
		"https://portal.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.ConfigureKeycloak(true, "https://sso.example.com", "basic-platform")
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-update", strings.NewReader(`{"application_code":"contract_management","environment":"prod","issuer_alias":"keycloak"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)

	if response.Code != stdhttp.StatusConflict || !strings.Contains(response.Body.String(), "IAM_AUTH_PROVIDER_SWITCH_NOT_READY") || !strings.Contains(response.Body.String(), "broker_login_verified") {
		t.Fatalf("unexpected blocked-switch response: %d %s", response.Code, response.Body.String())
	}
	if provisioner.input.Issuer != "" {
		t.Fatalf("provisioner received a blocked Keycloak switch: %#v", provisioner.input)
	}
}

type recordingKeycloakReadiness struct {
	verification KeycloakBrokerLoginVerification
	called       bool
}

func (readiness *recordingKeycloakReadiness) InspectKeycloakSwitchReadiness(context.Context, string, string, string) (KeycloakSwitchReadiness, error) {
	return unverifiedKeycloakSwitchReadiness(), nil
}

func (readiness *recordingKeycloakReadiness) RecordBrokerLoginVerification(_ context.Context, verification KeycloakBrokerLoginVerification) error {
	readiness.called = true
	readiness.verification = verification
	return nil
}

func TestVerifyKeycloakBrokerLoginBindsAuthenticatedSessionToExactTarget(t *testing.T) {
	t.Parallel()
	service := &stubSubsystemOnboardingService{portalItems: []application.PortalApplication{{ApplicationID: "app-1", EnvironmentID: "env-1", Code: "contract_management", Environment: "prod"}}}
	handler, err := NewSubsystemOnboardingHandler(service, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{}, "https://portal.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	readiness := &recordingKeycloakReadiness{}
	handler.ConfigureKeycloak(true, "https://sso.example.com", "basic-platform")
	handler.ConfigureKeycloakSwitchReadinessInspector(readiness)
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/keycloak/broker-login-verifications", strings.NewReader(`{"application_code":"contract_management","environment":"prod","identity_id":"user-1","issuer":"https://sso.example.com/","client_id":"contract-prod-web"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(keycloakctx.WithBrokerClaims(request.Context(), keycloakctx.BrokerClaims{Issuer: "https://sso.example.com", SessionID: "session-1", TenantID: "tenant-1", IdentityID: "user-1", Audience: []string{"contract-prod-web"}}))
	response := httptest.NewRecorder()

	handler.VerifyKeycloakBrokerLogin(response, request)

	if response.Code != stdhttp.StatusOK || !readiness.called {
		t.Fatalf("response = %d %s, called = %v", response.Code, response.Body.String(), readiness.called)
	}
	if got := readiness.verification; got.TenantID != "tenant-1" || got.ApplicationID != "app-1" || got.EnvironmentID != "env-1" || got.IdentityID != "user-1" || got.VerifiedByID != "user-1" || got.SessionID != "session-1" || got.Issuer != "https://sso.example.com" || got.ClientID != "contract-prod-web" {
		t.Fatalf("unexpected bound verification: %#v", got)
	}
}

func TestVerifyKeycloakBrokerLoginRejectsForgedIdentity(t *testing.T) {
	t.Parallel()
	handler, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{}, "https://portal.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	readiness := &recordingKeycloakReadiness{}
	handler.ConfigureKeycloak(true, "https://sso.example.com", "basic-platform")
	handler.ConfigureKeycloakSwitchReadinessInspector(readiness)
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/keycloak/broker-login-verifications", strings.NewReader(`{"application_code":"contract_management","environment":"prod","identity_id":"victim-1","issuer":"https://sso.example.com","client_id":"contract-prod-web"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(keycloakctx.WithBrokerClaims(request.Context(), keycloakctx.BrokerClaims{Issuer: "https://sso.example.com", SessionID: "session-1", TenantID: "tenant-1", IdentityID: "user-1", Audience: []string{"contract-prod-web"}}))
	response := httptest.NewRecorder()

	handler.VerifyKeycloakBrokerLogin(response, request)

	if response.Code != stdhttp.StatusUnprocessableEntity || readiness.called {
		t.Fatalf("response = %d %s, recorder called = %v", response.Code, response.Body.String(), readiness.called)
	}
}
