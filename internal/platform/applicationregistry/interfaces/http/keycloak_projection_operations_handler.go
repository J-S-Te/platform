package http

import (
	"errors"
	stdhttp "net/http"
	"strconv"
	"strings"

	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// keycloakProjectionReplayRequest is intentionally small. The server owns the
// target event ID (path), tenant (session) and replay time; an operator must
// echo the event ID and provide a human-readable reason to prevent accidental
// replay from a stale management page.
type keycloakProjectionReplayRequest struct {
	Confirmation string `json:"confirmation"`
	Reason       string `json:"reason"`
}

// projectionFailureResponse keeps JSON tags visible and avoids returning any
// outbox payload/credential data. Time is left as its standard RFC3339 value.
type projectionFailureResponse struct {
	EventID         string `json:"event_id"`
	IdentityID      string `json:"identity_id"`
	ApplicationID   string `json:"application_id"`
	EnvironmentID   string `json:"environment_id,omitempty"`
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment,omitempty"`
	EventType       string `json:"event_type"`
	Attempts        uint   `json:"attempts"`
	FailedAt        string `json:"failed_at"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
	BlocksCutover   bool   `json:"blocks_cutover"`
}

// ListKeycloakProjectionFailures serves the FAILED/dead-letter management
// page. It is read-only and returns only durable diagnostic fields.
func (handler *SubsystemOnboardingHandler) ListKeycloakProjectionFailures(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	if handler.keycloakOperations == nil {
		writeKeycloakProjectionOperationsUnavailable(writer, request)
		return
	}
	result, err := handler.keycloakOperations.ListFailed(request.Context(), principal.Tenant.ID, projectionapplication.FailurePageRequest{
		Page: applicationRegistryPositiveInt(request.URL.Query().Get("page")), PageSize: applicationRegistryPositiveInt(request.URL.Query().Get("page_size")),
		ApplicationCode: request.URL.Query().Get("application_code"), Environment: request.URL.Query().Get("environment"),
	})
	if err != nil {
		handler.writeKeycloakProjectionOperationError(writer, request, err)
		return
	}
	items := make([]projectionFailureResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, projectionFailureResponse{
			EventID: item.EventID, IdentityID: item.IdentityID, ApplicationID: item.ApplicationID, EnvironmentID: item.EnvironmentID,
			ApplicationCode: item.ApplicationCode, Environment: item.Environment, EventType: item.EventType, Attempts: item.Attempts,
			FailedAt: item.FailedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"), ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage,
			BlocksCutover: item.BlocksCutover,
		})
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 授权投影死信查询成功", map[string]any{
		"items": items, "page": result.Page, "page_size": result.PageSize, "total": result.Total,
	})
}

// GetKeycloakProjectionAlerts is the small, poll-friendly operations alert
// contract. A FAILED event is elevated to CRITICAL because cutover remains
// blocked until it has been replayed and processed successfully.
func (handler *SubsystemOnboardingHandler) GetKeycloakProjectionAlerts(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	if handler.keycloakOperations == nil {
		writeKeycloakProjectionOperationsUnavailable(writer, request)
		return
	}
	status, err := handler.keycloakOperations.AlertStatus(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeKeycloakProjectionOperationError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 授权投影告警状态查询成功", status)
}

// ReplayKeycloakProjectionFailure performs a conditional FAILED -> PENDING
// transition. Repeating the same POST after a network timeout is idempotent:
// it reports already_pending and never inserts a duplicate authorization job.
func (handler *SubsystemOnboardingHandler) ReplayKeycloakProjectionFailure(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	if handler.keycloakOperations == nil {
		writeKeycloakProjectionOperationsUnavailable(writer, request)
		return
	}
	var payload keycloakProjectionReplayRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	result, err := handler.keycloakOperations.Replay(request.Context(), projectionapplication.ReplayInput{
		TenantID: principal.Tenant.ID, EventID: request.PathValue("event_id"), OperatorID: principal.User.ID,
		Confirmation: payload.Confirmation, Reason: payload.Reason,
	})
	if err != nil {
		handler.writeKeycloakProjectionOperationError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "Keycloak 授权投影已受控重放", map[string]any{
		"event_id": result.EventID, "replayed": result.Replayed, "already_pending": result.AlreadyPending, "available_at": result.AvailableAt,
		"next_action": "等待 Keycloak 授权投影 Worker 处理；成功后刷新告警与切换门禁。",
	})
}

func (handler *SubsystemOnboardingHandler) writeKeycloakProjectionOperationError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error) {
	switch {
	case errors.Is(err, projectionapplication.ErrOperationsValidation):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, projectionapplication.ErrProjectionNotFound):
		httpresponse.WriteError(writer, request, stdhttp.StatusNotFound, httperror.NotFound)
	case errors.Is(err, projectionapplication.ErrProjectionConflict):
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New("KEYCLOAK_PROJECTION_REPLAY_CONFLICT", "该授权投影当前不能重放", map[string]string{"next_action": "刷新死信列表；仅 FAILED 状态允许受控重放。"}))
	default:
		handler.logger.Error("Keycloak projection operation failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}

func writeKeycloakProjectionOperationsUnavailable(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.New("KEYCLOAK_PROJECTION_OPERATIONS_UNAVAILABLE", "Keycloak 授权投影运维能力当前不可用", map[string]string{"next_action": "确认 platform-api 已使用包含 Keycloak 授权 Worker 的版本并完成数据库迁移。"}))
}

func applicationRegistryPositiveInt(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 {
		return 0
	}
	return parsed
}
