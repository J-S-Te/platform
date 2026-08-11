package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// loginTargetManagementService is intentionally small so the HTTP adapter cannot reach runtime
// resolver internals or OAuth redirect URI registration operations.
type loginTargetManagementService interface {
	ListLoginTargets(context.Context, string, string, string, application.PageRequest) (application.PageResult[application.LoginTargetManagementItem], error)
	CreateLoginTarget(context.Context, application.LoginTargetCreateInput) (application.LoginTargetManagementItem, error)
	GetLoginTarget(context.Context, string, string, string, string) (application.LoginTargetManagementItem, error)
	UpdateLoginTarget(context.Context, application.LoginTargetUpdateInput) (application.LoginTargetManagementItem, error)
}

// LoginTargetManagementHandler exposes protected application login-target control-plane APIs. It
// is a net/http adapter mounted only through the shared Gin router's adapter function.
type LoginTargetManagementHandler struct {
	service loginTargetManagementService
	logger  *slog.Logger
}

// NewLoginTargetManagementHandler constructs the protected login-target control-plane adapter.
func NewLoginTargetManagementHandler(service loginTargetManagementService, logger *slog.Logger) (*LoginTargetManagementHandler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("application login target management handler dependencies must not be nil")
	}
	return &LoginTargetManagementHandler{service: service, logger: logger}, nil
}

type loginTargetCreateRequest struct {
	TargetCode string `json:"target_code"`
	Name       string `json:"name"`
	TargetURI  string `json:"target_uri"`
	Status     string `json:"status"`
}

type loginTargetUpdateRequest struct {
	Name      string `json:"name"`
	TargetURI string `json:"target_uri"`
	Status    string `json:"status"`
	Version   uint64 `json:"version"`
}

type loginTargetResponse struct {
	LoginTargetID string    `json:"login_target_id"`
	ApplicationID string    `json:"application_id"`
	EnvironmentID string    `json:"environment_id"`
	TargetCode    string    `json:"target_code"`
	Name          string    `json:"name"`
	TargetURI     string    `json:"target_uri"`
	Status        string    `json:"status"`
	Version       uint64    `json:"version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ListLoginTargets handles GET /api/v1/applications/:application_id/environments/:environment_id/login-targets.
func (handler *LoginTargetManagementHandler) ListLoginTargets(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	result, err := handler.service.ListLoginTargets(request.Context(), principal.Tenant.ID, request.PathValue("application_id"), request.PathValue("environment_id"), applicationManagementPageQuery(request))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]loginTargetResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, loginTargetToResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用登录目标查询成功", managementPageResponse[loginTargetResponse]{
		Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total,
	})
}

// CreateLoginTarget handles POST /api/v1/applications/:application_id/environments/:environment_id/login-targets.
func (handler *LoginTargetManagementHandler) CreateLoginTarget(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload loginTargetCreateRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}

	item, err := handler.service.CreateLoginTarget(request.Context(), application.LoginTargetCreateInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, ApplicationID: request.PathValue("application_id"),
		EnvironmentID: request.PathValue("environment_id"), TargetCode: payload.TargetCode, Name: payload.Name,
		TargetURI: payload.TargetURI, Status: payload.Status,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "应用登录目标已创建", loginTargetToResponse(item))
}

// GetLoginTarget handles GET /api/v1/applications/:application_id/environments/:environment_id/login-targets/:login_target_id.
func (handler *LoginTargetManagementHandler) GetLoginTarget(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	item, err := handler.service.GetLoginTarget(request.Context(), principal.Tenant.ID, request.PathValue("application_id"), request.PathValue("environment_id"), request.PathValue("login_target_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用登录目标查询成功", loginTargetToResponse(item))
}

// UpdateLoginTarget handles PATCH /api/v1/applications/:application_id/environments/:environment_id/login-targets/:login_target_id.
func (handler *LoginTargetManagementHandler) UpdateLoginTarget(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload loginTargetUpdateRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}

	item, err := handler.service.UpdateLoginTarget(request.Context(), application.LoginTargetUpdateInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, ApplicationID: request.PathValue("application_id"),
		EnvironmentID: request.PathValue("environment_id"), LoginTargetID: request.PathValue("login_target_id"),
		Name: payload.Name, TargetURI: payload.TargetURI, Status: payload.Status, Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用登录目标已更新", loginTargetToResponse(item))
}

func (handler *LoginTargetManagementHandler) principal(writer http.ResponseWriter, request *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}

func (handler *LoginTargetManagementHandler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrVersionConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.VersionConflict)
	default:
		handler.logger.Error("application login target management request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func loginTargetToResponse(item application.LoginTargetManagementItem) loginTargetResponse {
	return loginTargetResponse{
		LoginTargetID: item.ID, ApplicationID: item.ApplicationID, EnvironmentID: item.EnvironmentID, TargetCode: item.TargetCode,
		Name: item.Name, TargetURI: item.TargetURI, Status: item.Status, Version: item.Version,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}
