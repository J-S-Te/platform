package http

import (
	stdhttp "net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// StartKeycloakObservation opens the required seven-day, per-environment
// observation window.  It is deliberately not implicit in Client sync: the
// operator must first see all server-verified projection and Broker-login
// gates, then explicitly begin the measurable migration period.
func (handler *SubsystemOnboardingHandler) StartKeycloakObservation(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	if handler.keycloakCutover == nil || !handler.keycloakEnabled {
		handler.writeError(writer, request, application.ErrSubsystemProvisioningUnavailable)
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if strings.TrimSpace(payload.ApplicationCode) == "" || strings.TrimSpace(payload.Environment) == "" {
		handler.writeError(writer, request, application.ErrValidation)
		return
	}
	readiness := handler.keycloakSwitchReadiness(request.Context(), principal.Tenant.ID, payload.ApplicationCode, payload.Environment)
	if !readiness.SwitchReady {
		writeKeycloakSwitchBlocked(writer, request, readiness)
		return
	}
	applicationID, environmentID, found := handler.resolveApplicationContext(writer, request, payload.ApplicationCode, payload.Environment)
	if !found {
		handler.writeError(writer, request, application.ErrNotFound)
		return
	}
	lifecycle, err := handler.keycloakCutover.StartKeycloakObservation(request.Context(), principal.Tenant.ID, applicationID, environmentID, principal.User.ID, keycloakObservationWindow)
	if err != nil {
		handler.logger.Warn("start Keycloak observation rejected", "application_code", payload.ApplicationCode, "environment", payload.Environment, "error", err)
		writeKeycloakObservationBlocked(writer, request, err.Error())
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusAccepted, "Keycloak 七天观察期已开始；观察期结束前不会切换运行时 Issuer", lifecycle)
}
