package http

import (
	stdhttp "net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// StartKeycloakObservation 开启每个环境必需的七天观察窗口。刻意不随客户端同步
// 隐式启动：操作者必须先看到全部服务端校验的投影与 Broker 登录门禁，
// 再显式开始这段可度量的迁移观察期，保证窗口起点的证据可审计。
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
		// 安全（SEC-B3）：cause 已在上一行进结构化日志，这里只回传稳定错误码。
		writeKeycloakObservationBlocked(writer, request, keycloakObservationReasonCode(err))
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusAccepted, "Keycloak 七天观察期已开始；观察期结束前不会切换运行时 Issuer", lifecycle)
}
