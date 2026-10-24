package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxSubsystemAccessRequestBytes = 32 << 10

// subsystemAccessService 是统一接入向导暴露给 HTTP 的用例集合（窄接口，便于测试）。
type subsystemAccessService interface {
	Preview(context.Context, string, string, string, []string) (application.SubsystemAccessPreview, error)
	Submit(context.Context, application.SubsystemAccessSubmitRequest) (application.SubsystemAccessSubmitResult, error)
	Resume(context.Context, string, string) (application.SubsystemAccessOperationView, error)
	Describe(context.Context, string, string) (application.SubsystemAccessOperationView, error)
	FindByTarget(context.Context, string, string, string) (application.SubsystemAccessOperationView, bool, error)
}

// SubsystemAccessHandler 提供六个子系统共用的接入向导接口。
// 权限按"服务端判定的实际动作"检查：统一入口不降低既有端点的权限要求。
type SubsystemAccessHandler struct {
	service subsystemAccessService
	logger  *slog.Logger
}

// NewSubsystemAccessHandler 构造处理器；依赖缺失即失败。
func NewSubsystemAccessHandler(service subsystemAccessService, logger *slog.Logger) (*SubsystemAccessHandler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("subsystem access handler dependencies must not be nil")
	}
	return &SubsystemAccessHandler{service: service, logger: logger}, nil
}

type subsystemAccessPreviewRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
}

type subsystemAccessSubmitRequest struct {
	ApplicationCode string `json:"application_code"`
	Environment     string `json:"environment"`
	PublicOrigin    string `json:"public_origin"`
	AuthProvider    string `json:"auth_provider"`
	InitialAdminID  string `json:"initial_admin_id"`
	PreviewVersion  string `json:"preview_version"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type subsystemAccessStageResponse struct {
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	AttemptCount uint32 `json:"attempt_count"`
	ErrorCode    string `json:"error_code,omitempty"`
	EvidenceRef  string `json:"evidence_ref,omitempty"`
}

type subsystemAccessOperationResponse struct {
	OperationID     string                               `json:"operation_id"`
	Action          string                               `json:"action"`
	Stage           string                               `json:"stage"`
	Status          string                               `json:"status"`
	ApplicationCode string                               `json:"application_code"`
	Environment     string                               `json:"environment"`
	PreviewVersion  string                               `json:"preview_version"`
	AttemptCount    uint32                               `json:"attempt_count"`
	ErrorCode       string                               `json:"error_code,omitempty"`
	ErrorMessage    string                               `json:"error_message,omitempty"`
	ResultRef       string                               `json:"result_ref,omitempty"`
	Stages          []subsystemAccessStageResponse       `json:"stages"`
	Missing         []application.SubsystemAccessMissing `json:"missing"`
}

type subsystemAccessPreviewResponse struct {
	Action         string                               `json:"action"`
	Reason         string                               `json:"reason"`
	DisplayOnly    bool                                 `json:"display_only"`
	NextStage      string                               `json:"next_stage"`
	Missing        []application.SubsystemAccessMissing `json:"missing"`
	RequiredPerms  []string                             `json:"required_permissions"`
	MissingPerms   []string                             `json:"missing_permissions"`
	ChangeSummary  []string                             `json:"change_summary"`
	PreviewVersion string                               `json:"preview_version"`
	ExecutionPath  string                               `json:"execution_path"`
	LegacyHint     string                               `json:"legacy_hint,omitempty"`
}

// Preview 只读检查：不创建资源、不部署。
func (handler *SubsystemAccessHandler) Preview(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload subsystemAccessPreviewRequest
	if !decodeSubsystemAccess(writer, request, &payload) {
		return
	}
	preview, err := handler.service.Preview(request.Context(), principal.Tenant.ID, payload.ApplicationCode, payload.Environment, principal.PermissionCodes)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "接入条件检查完成", subsystemAccessPreviewResponse{
		Action: string(preview.Decision.Action), Reason: preview.Decision.Reason, DisplayOnly: preview.Decision.DisplayOnly,
		NextStage: string(preview.NextStage), Missing: preview.Missing, RequiredPerms: preview.RequiredPerms,
		MissingPerms: preview.MissingPerms, ChangeSummary: preview.ChangeSummary, PreviewVersion: preview.PreviewVersion,
		ExecutionPath: preview.ExecutionPath, LegacyHint: preview.LegacyHint,
	})
}

// Submit 确认执行：先按服务端判定的动作检查权限，再创建操作并返回 202 与操作 ID。
func (handler *SubsystemAccessHandler) Submit(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload subsystemAccessSubmitRequest
	if !decodeSubsystemAccess(writer, request, &payload) {
		return
	}
	// 先取服务端判定的动作，再按该动作要求检查权限（不信任页面自述的动作）。
	preview, err := handler.service.Preview(request.Context(), principal.Tenant.ID, payload.ApplicationCode, payload.Environment, principal.PermissionCodes)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if !principalHasSubsystemAccessPermissions(principal, application.SubsystemAccessRequiredPermissions(preview.Decision.Action)) {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}
	result, err := handler.service.Submit(request.Context(), application.SubsystemAccessSubmitRequest{
		TenantID: principal.Tenant.ID, ApplicationCode: payload.ApplicationCode, EnvironmentCode: payload.Environment,
		PublicOrigin: payload.PublicOrigin, AuthProvider: payload.AuthProvider, InitialAdminID: payload.InitialAdminID,
		PreviewVersion: payload.PreviewVersion, IdempotencyKey: payload.IdempotencyKey, OperatorID: principal.User.ID,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if result.DisplayOnly {
		httpresponse.WriteSuccess(writer, request, http.StatusOK, "目标已接入且配置一致，无需重复操作", map[string]any{
			"action": string(result.Decision.Action), "reason": result.Decision.Reason, "display_only": true,
		})
		return
	}
	message := "接入操作已受理"
	if !result.Created {
		message = "相同请求已存在，返回原操作"
	}
	httpresponse.WriteSuccess(writer, request, http.StatusAccepted, message, map[string]any{
		"operation_id": result.Operation.ID, "action": string(result.Operation.Action),
		"stage": string(result.Operation.Stage), "status": string(result.Operation.Status), "created": result.Created,
	})
}

// Get 查询单个操作（阶段与分项结果）。
func (handler *SubsystemAccessHandler) Get(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	view, err := handler.service.Describe(request.Context(), principal.Tenant.ID, request.PathValue("operation_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "接入操作查询成功", subsystemAccessViewResponse(view))
}

// FindByTarget 供页面刷新或 CLI 重连按目标定位当前操作。
func (handler *SubsystemAccessHandler) FindByTarget(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	view, found, err := handler.service.FindByTarget(request.Context(), principal.Tenant.ID,
		request.URL.Query().Get("application_code"), request.URL.Query().Get("environment"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if !found {
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "目标当前操作查询成功", subsystemAccessViewResponse(view))
}

// Resume 在补齐条件后继续；权限同样按该操作的动作用户要求。
func (handler *SubsystemAccessHandler) Resume(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	operation, err := handler.service.Describe(request.Context(), principal.Tenant.ID, request.PathValue("operation_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	if !principalHasSubsystemAccessPermissions(principal, application.SubsystemAccessRequiredPermissions(operation.Operation.Action)) {
		httpresponse.WriteError(writer, request, http.StatusForbidden, httperror.Forbidden)
		return
	}
	view, err := handler.service.Resume(request.Context(), principal.Tenant.ID, request.PathValue("operation_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "恢复检查完成", subsystemAccessViewResponse(view))
}

// principalHasSubsystemAccessPermissions 要求主体持有该动作的**全部**权限（缺一即拒）。
func principalHasSubsystemAccessPermissions(principal authctx.Principal, required []string) bool {
	held := map[string]struct{}{}
	for _, permission := range principal.PermissionCodes {
		held[strings.TrimSpace(permission)] = struct{}{}
	}
	for _, permission := range required {
		if _, ok := held[permission]; !ok {
			return false
		}
	}
	return true
}

func decodeSubsystemAccess(writer http.ResponseWriter, request *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxSubsystemAccessRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}

func (handler *SubsystemAccessHandler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrSubsystemAccessInvalidRequest):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrSubsystemAccessOperationNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrSubsystemAccessOperationConflict), errors.Is(err, application.ErrSubsystemAccessOperationLeaseLost):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	default:
		handler.logger.Error("subsystem access request failed", "error", err, "path", request.URL.Path)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func subsystemAccessViewResponse(view application.SubsystemAccessOperationView) subsystemAccessOperationResponse {
	stages := make([]subsystemAccessStageResponse, 0, len(view.Stages))
	for _, stage := range view.Stages {
		stages = append(stages, subsystemAccessStageResponse{Stage: string(stage.Stage), Status: string(stage.Status),
			AttemptCount: stage.AttemptCount, ErrorCode: stage.ErrorCode, EvidenceRef: stage.EvidenceRef})
	}
	missing := view.Missing
	if missing == nil {
		missing = []application.SubsystemAccessMissing{}
	}
	return subsystemAccessOperationResponse{
		OperationID: view.Operation.ID, Action: string(view.Operation.Action), Stage: string(view.Operation.Stage),
		Status: string(view.Operation.Status), ApplicationCode: view.Operation.ApplicationCode, Environment: view.Operation.EnvironmentCode,
		PreviewVersion: view.Operation.PreviewVersion, AttemptCount: view.Operation.AttemptCount,
		ErrorCode: view.Operation.ErrorCode, ErrorMessage: view.Operation.ErrorMessage, ResultRef: view.Operation.ResultRef,
		Stages: stages, Missing: missing,
	}
}
