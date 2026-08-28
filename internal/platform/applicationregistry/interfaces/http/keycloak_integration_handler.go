package http

import (
	"bytes"
	"encoding/json"
	"errors"
	stdhttp "net/http"
)

// KeycloakIntegrationHandler is the authentication-integration boundary for
// application environments. It intentionally delegates all work to the
// existing subsystem control plane: application directory, role catalog and
// final authorization remain owned by Basic Platform, while this facade owns
// only Keycloak client synchronization and issuer cutover operations.
//
// It never exposes Keycloak Admin credentials, client secrets or deployment
// runtime credentials. The legacy /subsystem-* endpoints remain available for
// existing console versions and automation clients.
type KeycloakIntegrationHandler struct {
	subsystems KeycloakIntegrationUseCases
}

func NewKeycloakIntegrationHandler(subsystems KeycloakIntegrationUseCases) (*KeycloakIntegrationHandler, error) {
	if subsystems == nil {
		return nil, errors.New("subsystem onboarding handler is required")
	}
	return &KeycloakIntegrationHandler{subsystems: subsystems}, nil
}

// Capabilities exposes only public provider state and cutover gates.
func (handler *KeycloakIntegrationHandler) Capabilities(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetSubsystemCapabilities(writer, request)
}

// Status returns the durable, non-sensitive authentication-integration state
// for one application environment. It deliberately does not proxy Keycloak
// Admin API objects, Client secrets or runtime credentials to the browser.
func (handler *KeycloakIntegrationHandler) Status(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetKeycloakIntegrationStatus(writer, request)
}

// SyncClient creates or updates the server-side Keycloak client and the
// platform-owned claims projection. Client secrets stay between the platform,
// Keycloak and the deployment Agent.
func (handler *KeycloakIntegrationHandler) SyncClient(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.SyncKeycloakClient(writer, request)
}

// Switch moves an environment to the Keycloak issuer. The issuer alias is
// fixed by this route rather than accepted from the browser payload.
func (handler *KeycloakIntegrationHandler) Switch(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.SwitchToKeycloak(writer, request)
}

// Rollback returns an environment to the Basic Platform issuer. As with
// Switch, the target issuer is fixed server-side.
func (handler *KeycloakIntegrationHandler) Rollback(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.RollbackToPlatform(writer, request)
}

// ListProjectionFailures powers the Keycloak authorization projection
// management page. It lists only FAILED/dead-letter records.
func (handler *KeycloakIntegrationHandler) ListProjectionFailures(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.ListKeycloakProjectionFailures(writer, request)
}

// ProjectionAlerts exposes a compact alert/metric state for failed projection
// records without leaking a queue payload, Client secret or token.
func (handler *KeycloakIntegrationHandler) ProjectionAlerts(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	// Keep the public integration contract independent from the internal
	// operations representation.  In particular, browsers and automation rely
	// on these lower snake_case keys and must not need to infer Go field names.
	captured := newProjectionAlertsResponseCapture()
	handler.subsystems.GetKeycloakProjectionAlerts(captured, request)
	writeProjectionAlertsContract(writer, captured)
}

type projectionAlertsResponseCapture struct {
	header stdhttp.Header
	status int
	body   bytes.Buffer
}

func newProjectionAlertsResponseCapture() *projectionAlertsResponseCapture {
	return &projectionAlertsResponseCapture{header: make(stdhttp.Header)}
}

func (capture *projectionAlertsResponseCapture) Header() stdhttp.Header { return capture.header }

func (capture *projectionAlertsResponseCapture) WriteHeader(status int) { capture.status = status }

func (capture *projectionAlertsResponseCapture) Write(payload []byte) (int, error) {
	if capture.status == 0 {
		capture.status = stdhttp.StatusOK
	}
	return capture.body.Write(payload)
}

type projectionAlertsEnvelope struct {
	Code      string                `json:"code"`
	Message   string                `json:"message"`
	RequestID string                `json:"request_id"`
	Data      projectionAlertsState `json:"data"`
}

type projectionAlertsState struct {
	Severity                 string `json:"severity"`
	State                    string `json:"state"`
	Summary                  string `json:"summary"`
	FailedCount              int64  `json:"failed_count"`
	AffectedEnvironmentCount int64  `json:"affected_environment_count"`
	OldestFailedAt           any    `json:"oldest_failed_at,omitempty"`
}

func writeProjectionAlertsContract(writer stdhttp.ResponseWriter, captured *projectionAlertsResponseCapture) {
	for key, values := range captured.header {
		writer.Header()[key] = append([]string(nil), values...)
	}
	status := captured.status
	if status == 0 {
		status = stdhttp.StatusOK
	}
	if status != stdhttp.StatusOK {
		writer.WriteHeader(status)
		_, _ = writer.Write(captured.body.Bytes())
		return
	}
	var response projectionAlertsEnvelope
	if err := json.Unmarshal(captured.body.Bytes(), &response); err != nil || response.Code != "OK" {
		writer.WriteHeader(status)
		_, _ = writer.Write(captured.body.Bytes())
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(response)
}

// ReplayProjectionFailure is a guarded, idempotent FAILED -> PENDING replay.
func (handler *KeycloakIntegrationHandler) ReplayProjectionFailure(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.ReplayKeycloakProjectionFailure(writer, request)
}

// SyncStatus returns a focused synchronization status for one subsystem
// environment, including drift detection results. It is designed for
// operational dashboards and automated health checks.
func (handler *KeycloakIntegrationHandler) SyncStatus(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetKeycloakSyncStatus(writer, request)
}

// HealthDashboard returns the integration health of all subsystems.
func (handler *KeycloakIntegrationHandler) HealthDashboard(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.GetSubsystemHealthDashboard(writer, request)
}

// StartObservation opens the mandatory seven-day evidence window before an
// environment can cut over to Keycloak.
func (handler *KeycloakIntegrationHandler) StartObservation(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.StartKeycloakObservation(writer, request)
}

// VerifyBrokerLogin keeps the Keycloak JWT verification step available under
// the dedicated integration namespace. Router middleware, not this facade,
// enforces the narrow Keycloak bearer-token boundary.
func (handler *KeycloakIntegrationHandler) VerifyBrokerLogin(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	handler.subsystems.VerifyKeycloakBrokerLogin(writer, request)
}
