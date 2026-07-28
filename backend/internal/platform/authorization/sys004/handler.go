package sys004

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxAccessRequestBytes = 32 << 10

type Handler struct {
	service *Service
	logger  *slog.Logger
}

func NewHandler(service *Service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("SYS-004 HTTP handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

type updateAccessPayload struct {
	RoleCode          string   `json:"role_code"`
	CustomPermissions []string `json:"custom_permissions"`
}

func (h *Handler) GetAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	access, err := h.service.GetAccess(r.Context(), principal.Tenant.ID, strings.TrimSpace(r.PathValue("user_id")))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", access)
}

func (h *Handler) UpdateAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	var payload updateAccessPayload
	if !decodeAccessPayload(w, r, &payload) {
		return
	}
	access, err := h.service.UpdateAccess(r.Context(), UpdateAccessInput{
		TenantID:          principal.Tenant.ID,
		UserID:            strings.TrimSpace(r.PathValue("user_id")),
		OperatorID:        principal.User.ID,
		RoleCode:          payload.RoleCode,
		CustomPermissions: payload.CustomPermissions,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "合同系统权限已更新", access)
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrValidation):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotConfigured):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, ErrIntegrity):
		h.logger.ErrorContext(r.Context(), "SYS-004 integrity check rejected request", "error", err, "path", r.URL.Path)
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	default:
		h.logger.ErrorContext(r.Context(), "SYS-004 HTTP operation failed", "error", err, "path", r.URL.Path)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}

func decodeAccessPayload(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAccessRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}
