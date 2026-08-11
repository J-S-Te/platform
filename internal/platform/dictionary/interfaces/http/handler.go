// Package dictionaryhttp exposes business dictionary management through the platform response envelope.
package dictionaryhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	dictionaryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/application"
	dictionarydomain "github.com/J-S-Te/Basic-Platform/internal/platform/dictionary/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxRequestBytes = 32 * 1024

type service interface {
	ListDictionaries(ctx context.Context, tenantID string, query dictionaryapplication.PageRequest) (dictionaryapplication.PageResult[dictionarydomain.Dictionary], error)
	CreateDictionary(ctx context.Context, input dictionaryapplication.DictionaryCreateInput) (dictionarydomain.Dictionary, error)
	GetDictionary(ctx context.Context, tenantID, dictionaryID string) (dictionarydomain.Dictionary, error)
	UpdateDictionary(ctx context.Context, input dictionaryapplication.DictionaryUpdateInput) (dictionarydomain.Dictionary, error)
	ListItems(ctx context.Context, tenantID, dictionaryID string, query dictionaryapplication.PageRequest) (dictionaryapplication.PageResult[dictionarydomain.Item], error)
	CreateItem(ctx context.Context, input dictionaryapplication.ItemCreateInput) (dictionarydomain.Item, error)
	UpdateItem(ctx context.Context, input dictionaryapplication.ItemUpdateInput) (dictionarydomain.Item, error)
	ListActiveItemsByCode(ctx context.Context, tenantID, code string, query dictionaryapplication.PageRequest) (dictionaryapplication.PageResult[dictionarydomain.Item], error)
}

// Handler provides authenticated, tenant-scoped dictionary endpoints.
type Handler struct {
	service service
	logger  *slog.Logger
}

// NewHandler validates dependencies and creates the dictionary HTTP adapter.
func NewHandler(service service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("dictionary HTTP handler dependencies must not be nil")
	}

	return &Handler{service: service, logger: logger}, nil
}

type dictionaryPayload struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Version     uint64 `json:"version"`
}

type itemPayload struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	SortOrder uint   `json:"sort_order"`
	Status    string `json:"status"`
	Version   uint64 `json:"version"`
}

type dictionaryResponse struct {
	ID          string `json:"dictionary_id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	ItemCount   int64  `json:"item_count"`
	Version     uint64 `json:"version"`
	UpdatedAt   string `json:"updated_at"`
}

type itemResponse struct {
	ID           string `json:"item_id"`
	DictionaryID string `json:"dictionary_id"`
	Code         string `json:"code"`
	Label        string `json:"label"`
	Value        string `json:"value"`
	SortOrder    uint   `json:"sort_order"`
	Status       string `json:"status"`
	Version      uint64 `json:"version"`
	UpdatedAt    string `json:"updated_at"`
}

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// ListDictionaries lists dictionaries that belong to the current tenant.
func (handler *Handler) ListDictionaries(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	result, err := handler.service.ListDictionaries(request.Context(), principal.Tenant.ID, pageQuery(request))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	values := make([]dictionaryResponse, 0, len(result.Items))
	for _, dictionary := range result.Items {
		values = append(values, dictionaryToResponse(dictionary))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "业务字典查询成功", pageResponse[dictionaryResponse]{
		Items: values, Page: result.Page, PageSize: result.PageSize, Total: result.Total,
	})
}

// CreateDictionary creates a tenant-scoped business dictionary.
func (handler *Handler) CreateDictionary(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload dictionaryPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}

	result, err := handler.service.CreateDictionary(request.Context(), dictionaryapplication.DictionaryCreateInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, Code: payload.Code, Name: payload.Name,
		Description: payload.Description, Status: dictionarydomain.Status(payload.Status),
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "业务字典已创建", dictionaryToResponse(result))
}

// GetDictionary returns one dictionary in the current tenant.
func (handler *Handler) GetDictionary(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	result, err := handler.service.GetDictionary(request.Context(), principal.Tenant.ID, request.PathValue("dictionary_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusOK, "业务字典查询成功", dictionaryToResponse(result))
}

// UpdateDictionary replaces a dictionary under optimistic locking.
func (handler *Handler) UpdateDictionary(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload dictionaryPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}

	result, err := handler.service.UpdateDictionary(request.Context(), dictionaryapplication.DictionaryUpdateInput{
		TenantID: principal.Tenant.ID, DictionaryID: request.PathValue("dictionary_id"), OperatorID: principal.User.ID,
		Code: payload.Code, Name: payload.Name, Description: payload.Description, Status: dictionarydomain.Status(payload.Status), Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusOK, "业务字典已更新", dictionaryToResponse(result))
}

// ListItems lists all items, including disabled items, for administration.
func (handler *Handler) ListItems(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	result, err := handler.service.ListItems(request.Context(), principal.Tenant.ID, request.PathValue("dictionary_id"), pageQuery(request))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	values := make([]itemResponse, 0, len(result.Items))
	for _, item := range result.Items {
		values = append(values, itemToResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "字典项查询成功", pageResponse[itemResponse]{
		Items: values, Page: result.Page, PageSize: result.PageSize, Total: result.Total,
	})
}

// CreateItem creates a dictionary item under the requested dictionary.
func (handler *Handler) CreateItem(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload itemPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}

	result, err := handler.service.CreateItem(request.Context(), dictionaryapplication.ItemCreateInput{
		TenantID: principal.Tenant.ID, DictionaryID: request.PathValue("dictionary_id"), OperatorID: principal.User.ID,
		Code: payload.Code, Label: payload.Label, Value: payload.Value, SortOrder: payload.SortOrder, Status: dictionarydomain.Status(payload.Status),
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "字典项已创建", itemToResponse(result))
}

// UpdateItem replaces a dictionary item under optimistic locking.
func (handler *Handler) UpdateItem(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload itemPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}

	result, err := handler.service.UpdateItem(request.Context(), dictionaryapplication.ItemUpdateInput{
		TenantID: principal.Tenant.ID, DictionaryID: request.PathValue("dictionary_id"), ItemID: request.PathValue("item_id"),
		OperatorID: principal.User.ID, Code: payload.Code, Label: payload.Label, Value: payload.Value, SortOrder: payload.SortOrder,
		Status: dictionarydomain.Status(payload.Status), Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusOK, "字典项已更新", itemToResponse(result))
}

// ListActiveItemsByCode returns active values for business read-only selection controls.
func (handler *Handler) ListActiveItemsByCode(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	result, err := handler.service.ListActiveItemsByCode(request.Context(), principal.Tenant.ID, request.PathValue("dictionary_code"), pageQuery(request))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	values := make([]itemResponse, 0, len(result.Items))
	for _, item := range result.Items {
		values = append(values, itemToResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "启用字典项查询成功", pageResponse[itemResponse]{
		Items: values, Page: result.Page, PageSize: result.PageSize, Total: result.Total,
	})
}

func (handler *Handler) principal(writer http.ResponseWriter, request *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}

	return principal, true
}

func (handler *Handler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, dictionaryapplication.ErrValidation):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, dictionaryapplication.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, dictionaryapplication.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, dictionaryapplication.ErrVersionConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.VersionConflict)
	default:
		handler.logger.Error("dictionary request failed", "error", err, "path", request.URL.Path)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}

	return true
}

func pageQuery(request *http.Request) dictionaryapplication.PageRequest {
	query := request.URL.Query()
	return dictionaryapplication.PageRequest{
		Page:     parsePositiveInt(query.Get("page")),
		PageSize: parsePositiveInt(query.Get("page_size")),
		Keyword:  query.Get("keyword"),
		Status:   query.Get("status"),
	}
}

func parsePositiveInt(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0
	}

	return parsed
}

func dictionaryToResponse(dictionary dictionarydomain.Dictionary) dictionaryResponse {
	return dictionaryResponse{
		ID:          dictionary.ID,
		Code:        dictionary.Code,
		Name:        dictionary.Name,
		Description: dictionary.Description,
		Status:      string(dictionary.Status),
		ItemCount:   dictionary.ItemCount,
		Version:     dictionary.Version,
		UpdatedAt:   dictionary.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func itemToResponse(item dictionarydomain.Item) itemResponse {
	return itemResponse{
		ID:           item.ID,
		DictionaryID: item.DictionaryID,
		Code:         item.Code,
		Label:        item.Label,
		Value:        item.Value,
		SortOrder:    item.SortOrder,
		Status:       string(item.Status),
		Version:      item.Version,
		UpdatedAt:    item.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
