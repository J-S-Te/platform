package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type runtimeCredentialControl struct {
	keycloakControlStub
	requestedID string
	resultID    string
	secret      string
	err         error
}

func (control *runtimeCredentialControl) EnsureClient(_ context.Context, id, _, _ string) (keycloakClientResult, error) {
	control.requestedID = id
	if control.resultID != "" {
		id = control.resultID
	}
	return keycloakClientResult{ClientID: id, ClientSecret: control.secret}, control.err
}

type runtimeCredentialMapping struct {
	statusKeycloakMappingStore
	resolution                       KeycloakClientResolution
	err                              error
	tenant, application, environment string
}

func (mapping *runtimeCredentialMapping) ResolveEffectiveKeycloakClient(_ context.Context, tenant, app, env, _ string) (KeycloakClientResolution, error) {
	mapping.tenant, mapping.application, mapping.environment = tenant, app, env
	return mapping.resolution, mapping.err
}

func TestKeycloakRuntimeRebuildRefreshesCurrentCredential(t *testing.T) {
	for _, operation := range []string{"UPDATE", "ADOPT", "RETRY"} {
		for _, legacy := range []bool{false, true} {
			t.Run(operation+map[bool]string{false: "/canonical", true: "/legacy"}[legacy], func(t *testing.T) {
				control := &runtimeCredentialControl{secret: "current-keycloak-secret"}
				mapping := &runtimeCredentialMapping{}
				if legacy {
					mapping.resolution.ClientID = "approved-legacy-web"
				}
				handler, provisioner := newCredentialRefreshTestHandler(t, control, mapping)
				response := performCredentialRefresh(t, handler, operation)
				if response.Code != stdhttp.StatusAccepted {
					t.Fatalf("status = %d; body = %s", response.Code, response.Body.String())
				}
				wantID := "contract_management-prod-web"
				if legacy {
					wantID = mapping.resolution.ClientID
				}
				if control.requestedID != wantID || provisioner.input.ClientID != wantID || provisioner.input.ClientSecret != control.secret || !provisioner.input.AuthenticationRuntimeUpdate {
					t.Fatal("controlled rebuild did not receive the current mapped RP credential")
				}
				if mapping.tenant != "01K10A00000000000000000001" || mapping.application != "app" || mapping.environment != "env" {
					t.Fatal("credential resolution crossed deployment binding")
				}
				if strings.Contains(response.Body.String(), control.secret) {
					t.Fatal("credential exposed to browser")
				}
			})
		}
	}
}

func TestKeycloakRuntimeCredentialRefreshFailsClosed(t *testing.T) {
	for _, scenario := range []string{"unavailable", "empty-secret", "wrong-client", "mapping-error", "missing-control"} {
		t.Run(scenario, func(t *testing.T) {
			control := &runtimeCredentialControl{secret: "current-keycloak-secret"}
			mapping := &runtimeCredentialMapping{}
			switch scenario {
			case "unavailable":
				control.err = errors.New("keycloak unavailable")
			case "empty-secret":
				control.secret = ""
			case "wrong-client":
				control.resultID = "unrelated-client"
			case "mapping-error":
				mapping.err = errors.New("mapping unavailable")
			}
			handler, provisioner := newCredentialRefreshTestHandler(t, control, mapping)
			if scenario == "missing-control" {
				handler.keycloakControl = nil
			}
			response := performCredentialRefresh(t, handler, "UPDATE")
			if response.Code < 400 || provisioner.input.ClientSecret != "" {
				t.Fatalf("invalid credential started rebuild: status %d", response.Code)
			}
			if strings.Contains(response.Body.String(), "current-keycloak-secret") {
				t.Fatal("credential exposed in error")
			}
		})
	}
}

func TestCRMUpdateRepairsKeycloakRuntimeCredential(t *testing.T) {
	control := &runtimeCredentialControl{secret: "current-crm-keycloak-secret"}
	mapping := &runtimeCredentialMapping{}
	handler, provisioner := newCredentialRefreshTestHandler(t, control, mapping)
	request := lifecycletestRequest(t, "/api/v1/subsystem-update", `{"application_code":"customer_and_opportunity","environment":"dev","public_base_url":"http://localhost:8081","upstream_url":"http://customer-api:8090","path_prefix":"/customer-opportunity"}`)
	response := httptest.NewRecorder()
	handler.UpdateSubsystem(response, request)
	handler.waitForDeploymentJobs()
	if response.Code != stdhttp.StatusAccepted {
		t.Fatalf("CRM update status = %d; body = %s", response.Code, response.Body.String())
	}
	if control.requestedID != "customer_and_opportunity-dev-web" || provisioner.input.ClientID != control.requestedID || provisioner.input.ClientSecret != control.secret || !provisioner.input.AuthenticationRuntimeUpdate {
		t.Fatal("CRM update retained the stale credential")
	}
	if strings.Contains(response.Body.String(), control.secret) {
		t.Fatal("CRM credential exposed to browser")
	}
}

func newCredentialRefreshTestHandler(t *testing.T, control *runtimeCredentialControl, mapping *runtimeCredentialMapping) (*SubsystemOnboardingHandler, *recordingHTTPSubsystemProvisioner) {
	t.Helper()
	store := &keycloakIssuerStateStore{recordingSubsystemDeploymentStateStore: recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{ApplicationID: "app", EnvironmentID: "env", Status: application.SubsystemDeploymentStatusReady, AppliedManifestChecksum: "sha256:old"}}, issuerAlias: "keycloak"}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), store)
	if err != nil {
		t.Fatal(err)
	}
	handler.keycloakEnabled, handler.keycloakIssuer, handler.keycloakRealm = true, "http://sso.example.com/realms/test", "test"
	handler.defaultIssuerAlias = "keycloak"
	handler.keycloakControl = control
	handler.keycloakMappings = mapping
	handler.serviceCredentials = &serviceCredentialManagerStub{}
	return handler, provisioner
}

func performCredentialRefresh(t *testing.T, handler *SubsystemOnboardingHandler, operation string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/subsystem-update"
	if operation == "RETRY" {
		path = "/api/v1/subsystem-retry"
	}
	request := lifecycletestRequest(t, path, `{"application_code":"contract_management","environment":"prod","public_base_url":"http://localhost:8081","upstream_url":"http://contract-api:8081","path_prefix":"/contract_management"}`)
	response := httptest.NewRecorder()
	if operation == "ADOPT" {
		handler.AdoptSubsystem(response, request)
	} else {
		handler.UpdateSubsystem(response, request)
	}
	handler.waitForDeploymentJobs()
	return response
}
