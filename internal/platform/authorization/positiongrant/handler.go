package positiongrant

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxRequestBytes = 128 << 10

type Handler struct{ service *Service }

func NewHandler(service *Service) (*Handler, error) {
	if service == nil {
		return nil, errors.New("position authorization template HTTP handler service must not be nil")
	}
	return &Handler{service: service}, nil
}

func (h *Handler) ListAuthorizationTargets(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	items, err := h.service.ListAuthorizationTargets(r.Context(), principal.Tenant.ID)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	writeItems(w, r, items, "操作成功")
}

func (h *Handler) ListRoleInheritances(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	items, err := h.service.ListRoleInheritances(r.Context(), principal.Tenant.ID)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	writeItems(w, r, items, "操作成功")
}

func (h *Handler) ReplaceRoleInheritances(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	var payload RoleInheritanceReplaceInput
	if !decode(w, r, &payload) {
		return
	}
	items, err := h.service.ReplaceRoleInheritances(r.Context(), principal.Tenant.ID, principal.User.ID, payload)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	writeItems(w, r, items, "角色继承映射已保存")
}

func (h *Handler) ListAuthorizationPositions(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	items, err := h.service.ListAuthorizationPositions(r.Context(), principal.Tenant.ID)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	writeItems(w, r, items, "操作成功")
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	items, err := h.service.List(r.Context(), principal.Tenant.ID)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	writeItems(w, r, items, "操作成功")
}
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	item, err := h.service.Get(r.Context(), principal.Tenant.ID, pathValue(r, "template_id"))
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", item)
}
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	var payload TemplateInput
	if !decode(w, r, &payload) {
		return
	}
	item, err := h.service.Create(r.Context(), principal.Tenant.ID, principal.User.ID, payload)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusCreated, "岗位授权模板已创建", item)
}
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	var payload TemplateInput
	if !decode(w, r, &payload) {
		return
	}
	item, err := h.service.Update(r.Context(), principal.Tenant.ID, principal.User.ID, pathValue(r, "template_id"), payload)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "岗位授权模板已更新", item)
}
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseUint(strings.TrimSpace(r.URL.Query().Get("version")), 10, 64)
	if err != nil || version == 0 {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	if err := h.service.Delete(r.Context(), principal.Tenant.ID, principal.User.ID, pathValue(r, "template_id"), version); err != nil {
		writeHTTPError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "岗位授权模板已停用", map[string]any{"template_id": pathValue(r, "template_id")})
}
func (h *Handler) ListPositionAssignments(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	items, err := h.service.ListPositionAssignments(r.Context(), principal.Tenant.ID, pathValue(r, "position_id"))
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	writeItems(w, r, items, "操作成功")
}
func (h *Handler) ReplacePositionAssignments(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	var payload ReplaceAssignmentsInput
	if !decode(w, r, &payload) {
		return
	}
	items, err := h.service.ReplacePositionAssignments(r.Context(), principal.Tenant.ID, principal.User.ID, pathValue(r, "position_id"), payload)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	writeItems(w, r, items, "岗位授权模板已更新")
}
func (h *Handler) Preview(w http.ResponseWriter, r *http.Request) {
	principal, ok := principal(w, r)
	if !ok {
		return
	}
	var payload PreviewInput
	if !decode(w, r, &payload) {
		return
	}
	result, err := h.service.Preview(r.Context(), principal.Tenant.ID, payload)
	if err != nil {
		writeHTTPError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "授权预览已生成", result)
}

func principal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	value, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
	}
	return value, ok
}

func pathValue(r *http.Request, name string) string {
	return strings.TrimSpace(r.PathValue(name))
}

func writeItems(w http.ResponseWriter, r *http.Request, items any, message string) {
	// All collection endpoints in this handler expose the same bounded response shape.
	// Keeping it here prevents individual handlers from drifting in item/total formatting.
	total := 0
	switch values := items.(type) {
	case []TemplateView:
		total = len(values)
	case []AssignmentView:
		total = len(values)
	case []AuthorizationTargetView:
		total = len(values)
	case []AuthorizationPositionView:
		total = len(values)
	case []RoleInheritanceView:
		total = len(values)
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, message, map[string]any{"items": items, "total": total})
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
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
func writeHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, ErrValidation):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.VersionConflict)
	default:
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}
