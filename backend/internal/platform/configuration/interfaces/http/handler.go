// Package http exposes configuration management through the platform response envelope.
package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/configuration/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxRequestBytes = 64 << 10

type Handler struct {
	service *application.Service
	logger  *slog.Logger
}

func NewHandler(service *application.Service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("configuration HTTP handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}
type referenceResponse struct {
	ID   string `json:"id"`
	Code string `json:"code,omitempty"`
	Name string `json:"name"`
}
type namespaceResponse struct {
	ID          string            `json:"namespace_id"`
	Application referenceResponse `json:"application"`
	Code        string            `json:"code"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Version     uint64            `json:"version"`
}
type itemResponse struct {
	ID        string            `json:"item_id"`
	Namespace referenceResponse `json:"namespace"`
	Key       string            `json:"key"`
	ValueType string            `json:"value_type"`
	Value     any               `json:"value"`
	Secret    bool              `json:"secret"`
	Version   uint64            `json:"version"`
	UpdatedAt string            `json:"updated_at"`
}
type releaseResponse struct {
	ID          string            `json:"release_id"`
	Namespace   referenceResponse `json:"namespace"`
	VersionNo   uint64            `json:"version_no"`
	Status      string            `json:"status"`
	Comment     string            `json:"comment,omitempty"`
	CreatedAt   string            `json:"created_at"`
	PublishedAt *string           `json:"published_at,omitempty"`
}
type publishedResponse struct {
	ApplicationCode string         `json:"application_code"`
	NamespaceCode   string         `json:"namespace_code"`
	ReleaseVersion  uint64         `json:"release_version"`
	Values          map[string]any `json:"values"`
}
type namespaceCreatePayload struct {
	ApplicationCode string `json:"application_code"`
	Code            string `json:"code"`
	Name            string `json:"name"`
	Description     string `json:"description"`
}
type itemCreatePayload struct {
	NamespaceID string `json:"namespace_id"`
	Key         string `json:"key"`
	ValueType   string `json:"value_type"`
	Value       any    `json:"value"`
	Secret      bool   `json:"secret"`
}
type itemUpdatePayload struct {
	itemCreatePayload
	Version uint64 `json:"version"`
}
type versionedItemPayload struct {
	ItemID  string `json:"item_id"`
	Version uint64 `json:"version"`
}
type releaseCreatePayload struct {
	NamespaceID  string                 `json:"namespace_id"`
	ItemVersions []versionedItemPayload `json:"item_versions"`
	Comment      string                 `json:"comment"`
}

func (h *Handler) ListNamespaces(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	result, err := h.service.ListNamespaces(r.Context(), principal.Tenant.ID, pageQuery(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	values := make([]namespaceResponse, 0, len(result.Items))
	for _, item := range result.Items {
		values = append(values, namespaceToResponse(item))
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", pageResponse[namespaceResponse]{Items: values, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}
func (h *Handler) CreateNamespace(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	var payload namespaceCreatePayload
	if !decode(w, r, &payload) {
		return
	}
	result, err := h.service.CreateNamespace(r.Context(), application.NamespaceCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, ApplicationCode: payload.ApplicationCode, Code: payload.Code, Name: payload.Name, Description: payload.Description})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusCreated, "配置命名空间已创建", namespaceToResponse(result))
}
func (h *Handler) ListItems(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	result, err := h.service.ListItems(r.Context(), principal.Tenant.ID, pageQuery(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	values := make([]itemResponse, 0, len(result.Items))
	for _, item := range result.Items {
		values = append(values, itemToResponse(item))
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "操作成功", pageResponse[itemResponse]{Items: values, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}
func (h *Handler) CreateItem(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	var payload itemCreatePayload
	if !decode(w, r, &payload) {
		return
	}
	result, err := h.service.CreateItem(r.Context(), application.ItemCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, NamespaceID: payload.NamespaceID, Key: payload.Key, ValueType: payload.ValueType, Value: payload.Value, Secret: payload.Secret})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusCreated, "配置项已创建", itemToResponse(result))
}
func (h *Handler) UpdateItem(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	var payload itemUpdatePayload
	if !decode(w, r, &payload) {
		return
	}
	result, err := h.service.UpdateItem(r.Context(), application.ItemUpdateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, ItemID: r.PathValue("item_id"), NamespaceID: payload.NamespaceID, Key: payload.Key, ValueType: payload.ValueType, Value: payload.Value, Secret: payload.Secret, Version: payload.Version})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "配置项已更新", itemToResponse(result))
}
func (h *Handler) CreateRelease(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	var payload releaseCreatePayload
	if !decode(w, r, &payload) {
		return
	}
	versions := make([]domain.VersionedItem, 0, len(payload.ItemVersions))
	for _, version := range payload.ItemVersions {
		versions = append(versions, domain.VersionedItem{ItemID: version.ItemID, Version: version.Version})
	}
	result, err := h.service.CreateRelease(r.Context(), application.ReleaseCreateInput{TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, NamespaceID: payload.NamespaceID, Comment: payload.Comment, ItemVersions: versions})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusAccepted, "配置版本已发布", releaseToResponse(result))
}
func (h *Handler) GetRelease(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	result, err := h.service.GetRelease(r.Context(), principal.Tenant.ID, r.PathValue("release_id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "配置发布记录查询成功", releaseToResponse(result))
}
func (h *Handler) GetPublished(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.principal(w, r)
	if !ok {
		return
	}
	result, err := h.service.GetPublished(r.Context(), principal.Tenant.ID, r.PathValue("application_code"), r.PathValue("namespace_code"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpresponse.WriteSuccess(w, r, http.StatusOK, "已发布配置查询成功", publishedResponse{ApplicationCode: result.ApplicationCode, NamespaceCode: result.NamespaceCode, ReleaseVersion: result.ReleaseVersion, Values: result.Values})
}
func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(r.Context())
	if !ok {
		httpresponse.WriteError(w, r, http.StatusUnauthorized, httperror.Unauthenticated)
	}
	return principal, ok
}
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation):
		httpresponse.WriteError(w, r, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrVersionConflict):
		httpresponse.WriteError(w, r, http.StatusConflict, httperror.VersionConflict)
	default:
		h.logger.Error("configuration HTTP operation failed", "error", err, "path", r.URL.Path)
		httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
	}
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
func pageQuery(r *http.Request) application.PageRequest {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	return application.PageRequest{Page: page, PageSize: size, Keyword: strings.TrimSpace(r.URL.Query().Get("keyword")), NamespaceID: strings.TrimSpace(r.URL.Query().Get("filter[namespace_id]"))}
}
func referenceToResponse(value domain.Reference) referenceResponse {
	return referenceResponse{ID: value.ID, Code: value.Code, Name: value.Name}
}
func namespaceToResponse(value domain.Namespace) namespaceResponse {
	return namespaceResponse{ID: value.ID, Application: referenceToResponse(value.Application), Code: value.Code, Name: value.Name, Description: value.Description, Version: value.Version}
}
func itemToResponse(value domain.Item) itemResponse {
	return itemResponse{ID: value.ID, Namespace: referenceToResponse(value.Namespace), Key: value.Key, ValueType: value.ValueType, Value: value.Value, Secret: value.Secret, Version: value.Version, UpdatedAt: value.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")}
}
func releaseToResponse(value domain.Release) releaseResponse {
	response := releaseResponse{ID: value.ID, Namespace: referenceToResponse(value.Namespace), VersionNo: value.VersionNo, Status: value.Status, Comment: value.Comment, CreatedAt: value.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")}
	if value.PublishedAt != nil {
		published := value.PublishedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
		response.PublishedAt = &published
	}
	return response
}
