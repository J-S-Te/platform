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
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type statusKeycloakMappingStore struct {
	mapping KeycloakClientMapping
}

type keycloakProjectionOperationsStub struct {
	replay projectionapplication.ReplayInput
}

type keycloakRollbackLifecycleStub struct{}

func (keycloakRollbackLifecycleStub) GetKeycloakCutoverLifecycle(context.Context, string, string, string) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, nil
}

func (keycloakRollbackLifecycleStub) ListKeycloakCutoverTimeline(context.Context, string, string, string, int) ([]KeycloakCutoverTimelineEvent, error) {
	return nil, nil
}

func (keycloakRollbackLifecycleStub) StartKeycloakObservation(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, nil
}

func (keycloakRollbackLifecycleStub) CanKeycloakCutover(context.Context, string, string, string) error {
	return nil
}

func (keycloakRollbackLifecycleStub) CanKeycloakRollback(context.Context, string, string, string) error {
	return nil
}

func (keycloakRollbackLifecycleStub) ConfirmKeycloakCutover(context.Context, string, string, string, string, time.Duration) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, nil
}

func (keycloakRollbackLifecycleStub) RecordKeycloakRollback(context.Context, string, string, string, string) (KeycloakCutoverLifecycle, error) {
	return KeycloakCutoverLifecycle{}, nil
}

func (stub *keycloakProjectionOperationsStub) ListFailed(context.Context, string, projectionapplication.FailurePageRequest) (projectionapplication.FailurePageResult, error) {
	return projectionapplication.FailurePageResult{Items: []projectionapplication.FailedProjection{{EventID: "event-1", IdentityID: "identity-1", ApplicationCode: "crm", Environment: "prod", EventType: "AUTHORIZATION_CHANGED", Attempts: 5, FailedAt: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC), ErrorCode: "KEYCLOAK_SYNC_RETRY_EXHAUSTED", ErrorMessage: "Keycloak unavailable", BlocksCutover: true}}, Page: 1, PageSize: 20, Total: 1}, nil
}
func (stub *keycloakProjectionOperationsStub) AlertStatus(context.Context, string) (projectionapplication.AlertStatus, error) {
	return projectionapplication.AlertStatus{Severity: "CRITICAL", State: "ACTIVE", FailedCount: 1, Summary: "存在死信"}, nil
}
func (stub *keycloakProjectionOperationsStub) Replay(_ context.Context, input projectionapplication.ReplayInput) (projectionapplication.ReplayResult, error) {
	stub.replay = input
	return projectionapplication.ReplayResult{EventID: input.EventID, Replayed: true, AvailableAt: time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)}, nil
}

func (store *statusKeycloakMappingStore) SaveKeycloakClientMapping(context.Context, string, string, string, string, string) error {
	return nil
}

func (store *statusKeycloakMappingStore) BackfillKeycloakAuthorization(context.Context, string, string, string) error {
	return nil
}

func (store *statusKeycloakMappingStore) GetKeycloakClientMapping(context.Context, string, string, string) (KeycloakClientMapping, error) {
	return store.mapping, nil
}

func TestNewKeycloakIntegrationHandlerRequiresSubsystemControlPlane(t *testing.T) {
	t.Parallel()
	if _, err := NewKeycloakIntegrationHandler(nil); err == nil {
		t.Fatal("expected nil subsystem control plane to be rejected")
	}
}

func TestKeycloakIntegrationRollbackPinsPlatformIssuer(t *testing.T) {
	t.Parallel()
	provisioner := &recordingHTTPSubsystemProvisioner{}
	stateStore := &keycloakIssuerStateStore{
		recordingSubsystemDeploymentStateStore: recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{ApplicationID: "app-1"}},
		issuerAlias:                            "keycloak",
	}
	subsystems, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{result: application.SubsystemOnboardingResult{
			Application: application.Application{ID: "app-1"}, Environment: application.Environment{ID: "env-1"},
		}},
		provisioner,
		&recordingSubsystemAccessManager{},
		"https://platform.example.com",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		stateStore,
	)
	if err != nil {
		t.Fatal(err)
	}
	subsystems.ConfigureKeycloakCutoverLifecycle(keycloakRollbackLifecycleStub{})
	integration, err := NewKeycloakIntegrationHandler(subsystems)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/keycloak-integration/rollback", strings.NewReader(`{"application_code":"contract_management","environment":"prod","issuer_alias":"keycloak"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	integration.Rollback(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if provisioner.input.Issuer != "https://platform.example.com" {
		t.Fatalf("rollback issuer = %q, want Basic Platform issuer", provisioner.input.Issuer)
	}
	if provisioner.input.ClientID != "" || provisioner.input.ClientSecret != "" {
		t.Fatalf("rollback must not mint or expose a Keycloak client credential: %#v", provisioner.input)
	}
}

func TestKeycloakIntegrationStatusReturnsPersistedSafeClientMapping(t *testing.T) {
	t.Parallel()
	service := &stubSubsystemOnboardingService{result: application.SubsystemOnboardingResult{
		Application: application.Application{ID: "app-1", Code: "contract_management"},
		Environment: application.Environment{ID: "env-1", Environment: "prod"},
	}}
	subsystems, err := NewSubsystemOnboardingHandler(
		service,
		&recordingHTTPSubsystemProvisioner{},
		&recordingSubsystemAccessManager{},
		"https://platform.example.com",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	subsystems.ConfigureKeycloak(true, "https://sso.example.com/realms/basic-platform", "basic-platform")
	subsystems.ConfigureKeycloakClientMappingStore(&statusKeycloakMappingStore{mapping: KeycloakClientMapping{
		Realm: "basic-platform", ClientID: "contract_management-prod-web", Status: "SYNCED", Exists: true,
	}})
	integration, err := NewKeycloakIntegrationHandler(subsystems)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/keycloak-integration/status?application_code=contract_management&environment=prod", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	integration.Status(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{`"client_id":"contract_management-prod-web"`, `"client_state":"SYNCED"`, `"claims_state":"MAPPERS_SYNCED"`, `"realm":"basic-platform"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("status response missing %s: %s", expected, body)
		}
	}
	for _, forbidden := range []string{"client_secret", "password", "metadata"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("status response leaked %s: %s", forbidden, body)
		}
	}
}

func TestKeycloakIntegrationProjectionOperationsExposeSafeFailureAndControlledReplay(t *testing.T) {
	t.Parallel()
	operations := &keycloakProjectionOperationsStub{}
	subsystems, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{}, "https://platform.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	subsystems.ConfigureKeycloakProjectionOperations(operations)
	integration, err := NewKeycloakIntegrationHandler(subsystems)
	if err != nil {
		t.Fatal(err)
	}
	principal := authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"}}

	listRequest := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/keycloak-integration/projection-failures", nil).WithContext(authctx.WithPrincipal(context.Background(), principal))
	listResponse := httptest.NewRecorder()
	integration.ListProjectionFailures(listResponse, listRequest)
	if listResponse.Code != stdhttp.StatusOK || !strings.Contains(listResponse.Body.String(), `"blocks_cutover":true`) || strings.Contains(listResponse.Body.String(), "client_secret") {
		t.Fatalf("list response=%d %s", listResponse.Code, listResponse.Body.String())
	}
	alertsRequest := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/keycloak-integration/projection-alerts", nil)
	alertsRequest = alertsRequest.WithContext(authctx.WithPrincipal(alertsRequest.Context(), principal))
	alertsResponse := httptest.NewRecorder()
	integration.ProjectionAlerts(alertsResponse, alertsRequest)
	if alertsResponse.Code != stdhttp.StatusOK {
		t.Fatalf("alerts response=%d %s", alertsResponse.Code, alertsResponse.Body.String())
	}
	var alerts struct {
		Data struct {
			Severity                 string `json:"severity"`
			State                    string `json:"state"`
			Summary                  string `json:"summary"`
			FailedCount              int64  `json:"failed_count"`
			AffectedEnvironmentCount int64  `json:"affected_environment_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(alertsResponse.Body.Bytes(), &alerts); err != nil {
		t.Fatalf("decode alerts response: %v; body=%s", err, alertsResponse.Body.String())
	}
	if alerts.Data.Severity != "CRITICAL" || alerts.Data.State != "ACTIVE" || alerts.Data.Summary != "存在死信" || alerts.Data.FailedCount != 1 || alerts.Data.AffectedEnvironmentCount != 0 {
		t.Fatalf("unexpected snake_case alerts payload: %#v", alerts.Data)
	}
	for _, forbidden := range []string{`"Severity"`, `"State"`, `"Summary"`, "client_secret", "password"} {
		if strings.Contains(alertsResponse.Body.String(), forbidden) {
			t.Fatalf("alerts response leaked or used unstable field %s: %s", forbidden, alertsResponse.Body.String())
		}
	}

	replayRequest := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/keycloak-integration/projection-failures/event-1/replay", strings.NewReader(`{"confirmation":"event-1","reason":"已确认 Keycloak 管理端恢复"}`))
	replayRequest.Header.Set("Content-Type", "application/json")
	replayRequest.SetPathValue("event_id", "event-1")
	replayRequest = replayRequest.WithContext(authctx.WithPrincipal(replayRequest.Context(), principal))
	replayResponse := httptest.NewRecorder()
	integration.ReplayProjectionFailure(replayResponse, replayRequest)
	if replayResponse.Code != stdhttp.StatusOK || operations.replay.Confirmation != "event-1" || operations.replay.OperatorID != "user-1" || !strings.Contains(replayResponse.Body.String(), `"replayed":true`) {
		t.Fatalf("replay response=%d %s input=%#v", replayResponse.Code, replayResponse.Body.String(), operations.replay)
	}
}
