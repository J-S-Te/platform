// Package http exposes notification HTTP adapters for mounting on the shared Gin router.
package http

import (
	"encoding/json"
	"errors"
	app "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/application"
	domain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

const maxRequestBytes = 128 << 10

type Handler struct {
	service *app.Service
	logger  *slog.Logger
}

func NewHandler(service *app.Service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("notification handler dependencies must not be nil")
	}
	return &Handler{service, logger}, nil
}

type variablePayload struct {
	Name      string `json:"name"`
	Required  bool   `json:"required"`
	MaxLength int    `json:"max_length"`
}
type templatePayload struct {
	Code          string            `json:"code"`
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	TitleTemplate string            `json:"title_template"`
	BodyTemplate  string            `json:"body_template"`
	Variables     []variablePayload `json:"variables"`
}
type versionPayload struct {
	TitleTemplate string            `json:"title_template"`
	BodyTemplate  string            `json:"body_template"`
	Variables     []variablePayload `json:"variables"`
}
type statusPayload struct {
	Status  string `json:"status"`
	Version uint64 `json:"version"`
}
type targetPayload struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}
type messagePayload struct {
	TemplateCode   string            `json:"template_code"`
	Category       string            `json:"category"`
	Variables      map[string]string `json:"variables"`
	Recipients     []targetPayload   `json:"recipients"`
	TargetURL      string            `json:"target_url"`
	ReferenceType  string            `json:"reference_type"`
	ReferenceID    string            `json:"reference_id"`
	IdempotencyKey string            `json:"idempotency_key"`
}

func (h *Handler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var v templatePayload
	if !decode(w, r, &v) {
		return
	}
	t, ver, e := h.service.CreateTemplate(r.Context(), app.CreateTemplateInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, Code: v.Code, Name: v.Name, Status: domain.TemplateStatus(strings.ToUpper(strings.TrimSpace(v.Status))), TitleTemplate: v.TitleTemplate, BodyTemplate: v.BodyTemplate, Variables: variables(v.Variables)})
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusCreated, "站内信模板已创建", map[string]any{"template": t, "version": ver})
}
func (h *Handler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	out, e := h.service.ListTemplates(r.Context(), p.Tenant.ID, pagination(r))
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信模板查询成功", out)
}
func (h *Handler) CreateTemplateVersion(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var v versionPayload
	if !decode(w, r, &v) {
		return
	}
	t, ver, e := h.service.CreateTemplateVersion(r.Context(), app.CreateTemplateVersionInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, TemplateID: r.PathValue("template_id"), TitleTemplate: v.TitleTemplate, BodyTemplate: v.BodyTemplate, Variables: variables(v.Variables)})
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusCreated, "站内信模板版本已发布", map[string]any{"template": t, "version": ver})
}
func (h *Handler) ChangeTemplateStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var v statusPayload
	if !decode(w, r, &v) {
		return
	}
	out, e := h.service.ChangeTemplateStatus(r.Context(), app.ChangeTemplateStatusInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, TemplateID: r.PathValue("template_id"), Status: domain.TemplateStatus(strings.ToUpper(strings.TrimSpace(v.Status))), Version: v.Version})
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信模板状态已更新", out)
}
func (h *Handler) CreateMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var v messagePayload
	if !decode(w, r, &v) {
		return
	}
	targets := make([]domain.RecipientTarget, 0, len(v.Recipients))
	for _, x := range v.Recipients {
		targets = append(targets, domain.RecipientTarget{Type: domain.RecipientType(strings.ToUpper(strings.TrimSpace(x.Type))), ID: x.ID})
	}
	out, e := h.service.Create(r.Context(), app.CreateInput{TenantID: p.Tenant.ID, OperatorID: p.User.ID, TemplateCode: v.TemplateCode, Category: v.Category, Variables: v.Variables, Recipients: targets, TargetURL: v.TargetURL, ReferenceType: v.ReferenceType, ReferenceID: v.ReferenceID, IdempotencyKey: v.IdempotencyKey})
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusCreated, "站内信已创建", out)
}
func (h *Handler) ListInbox(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	out, e := h.service.ListInbox(r.Context(), p.Tenant.ID, p.User.ID, pagination(r))
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信列表查询成功", out)
}
func (h *Handler) GetInboxItem(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	out, e := h.service.GetInboxItem(r.Context(), p.Tenant.ID, p.User.ID, r.PathValue("delivery_id"))
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信详情查询成功", out)
}
func (h *Handler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	out, e := h.service.CountUnread(r.Context(), p.Tenant.ID, p.User.ID)
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "未读站内信数量查询成功", map[string]int64{"unread_count": out})
}
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	out, e := h.service.MarkRead(r.Context(), p.Tenant.ID, p.User.ID, r.PathValue("delivery_id"))
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信已读状态已更新", out)
}
func (h *Handler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	out, e := h.service.MarkAllRead(r.Context(), p.Tenant.ID, p.User.ID)
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信已全部标记为已读", map[string]int64{"updated_count": out})
}
func (h *Handler) ListDeliveries(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	status := domain.DeliveryStatus(strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status"))))
	out, e := h.service.ListDeliveries(r.Context(), p.Tenant.ID, status, pagination(r))
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信投递运营查询成功", out)
}

func (h *Handler) RetryFailed(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	limit, e := positive(r.URL.Query().Get("limit"), 20)
	if e != nil {
		h.writeError(w, r, app.ErrValidation)
		return
	}
	out, e := h.service.RetryFailedDeliveries(r.Context(), p.Tenant.ID, limit)
	if e != nil {
		h.writeError(w, r, e)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "站内信失败投递重试完成", out)
}
func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	p, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return p, true
}
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, e error) {
	switch {
	case errors.Is(e, app.ErrValidation), errors.Is(e, app.ErrNoRecipients):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(e, app.ErrNotFound):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(e, app.ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	case errors.Is(e, app.ErrVersionConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.VersionConflict)
	default:
		h.logger.Error("notification request failed", "error", e, "path", r.URL.Path)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(target); e != nil {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if e := d.Decode(&struct{}{}); !errors.Is(e, io.EOF) {
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}
func variables(input []variablePayload) []domain.VariableDefinition {
	out := make([]domain.VariableDefinition, 0, len(input))
	for _, v := range input {
		out = append(out, domain.VariableDefinition{Name: v.Name, Required: v.Required, MaxLength: v.MaxLength})
	}
	return out
}
func pagination(r *http.Request) app.PageRequest {
	p, _ := positive(r.URL.Query().Get("page"), 1)
	s, _ := positive(r.URL.Query().Get("page_size"), 20)
	return app.PageRequest{Page: p, PageSize: s}
}
func positive(raw string, fallback int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	v, e := strconv.Atoi(raw)
	if e != nil || v < 1 {
		return 0, errors.New("positive value required")
	}
	return v, nil
}
