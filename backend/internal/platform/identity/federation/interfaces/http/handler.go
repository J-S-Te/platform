// Package federationhttp exposes authenticated local federation-management endpoints.
package federationhttp

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

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxRequestBytes = 64 << 10

// ApplicationService is the transport-safe subset of the federation service.
type ApplicationService interface {
	ListProviders(context.Context, string, application.PageRequest) (application.PageResult[domain.Provider], error)
	GetProvider(context.Context, string, string) (domain.Provider, error)
	CreateProvider(context.Context, application.CreateProviderInput) (domain.Provider, error)
	UpdateProvider(context.Context, application.ProviderUpdate) (domain.Provider, error)
	ListBindings(context.Context, string, string) ([]domain.Binding, error)
	Bind(context.Context, application.BindInput) (domain.Binding, error)
	Unbind(context.Context, application.UnbindInput) (domain.Binding, error)
}

// Handler adapts the local federation management use cases to the platform HTTP contract. It has
// no endpoint for resolving raw external subjects: that operation is reserved for a future trusted
// assertion-validation adapter.
type Handler struct {
	service ApplicationService
	logger  *slog.Logger
}

func NewHandler(service ApplicationService, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("federation HTTP handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

type providerCreateRequest struct {
	Code                string   `json:"code"`
	Type                string   `json:"type"`
	Issuer              string   `json:"issuer"`
	ClientID            string   `json:"client_id"`
	ClientSecret        string   `json:"client_secret"`
	CallbackURI         string   `json:"callback_uri"`
	AuthorizationScopes []string `json:"authorization_scopes"`
	DisplayName         string   `json:"display_name"`
}

// Provider updates use pointer fields for optional runtime configuration: omitted values stay
// unchanged, while a provided client secret is encrypted by the application service and never
// appears in any response.
type providerUpdateRequest struct {
	DisplayName         string    `json:"display_name"`
	Status              string    `json:"status"`
	ClientID            *string   `json:"client_id"`
	ClientSecret        *string   `json:"client_secret"`
	CallbackURI         *string   `json:"callback_uri"`
	AuthorizationScopes *[]string `json:"authorization_scopes"`
	Version             uint64    `json:"version"`
}

// bindRequest is accepted only on an RBAC-protected management route. The raw subject is not
// echoed, stored in the HTTP response, or written to logs; callers must first validate it through
// a trusted upstream identity adapter.
type bindRequest struct {
	ProviderCode    string `json:"provider_code"`
	ExternalSubject string `json:"external_subject"`
}

type unbindRequest struct {
	Version uint64 `json:"version"`
}

type providerResponse struct {
	ProviderID             string     `json:"provider_id"`
	Code                   string     `json:"code"`
	Type                   string     `json:"type"`
	Issuer                 string     `json:"issuer"`
	ClientID               string     `json:"client_id"`
	CallbackURI            string     `json:"callback_uri"`
	AuthorizationScopes    []string   `json:"authorization_scopes"`
	ClientSecretConfigured bool       `json:"client_secret_configured"`
	ClientSecretUpdatedAt  *time.Time `json:"client_secret_updated_at"`
	DisplayName            string     `json:"display_name"`
	Status                 string     `json:"status"`
	Version                uint64     `json:"version"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type bindingResponse struct {
	BindingID  string     `json:"binding_id"`
	ProviderID string     `json:"provider_id"`
	UserID     string     `json:"user_id"`
	BoundAt    time.Time  `json:"bound_at"`
	UnboundAt  *time.Time `json:"unbound_at"`
	Status     string     `json:"status"`
	Version    uint64     `json:"version"`
}

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

func (handler *Handler) ListProviders(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	query, err := parsePageRequest(request)
	if err != nil {
		handler.validation(writer, request)
		return
	}
	result, err := handler.service.ListProviders(request.Context(), principal.Tenant.ID, query)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]providerResponse, 0, len(result.Items))
	for _, provider := range result.Items {
		items = append(items, toProviderResponse(provider))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", pageResponse[providerResponse]{
		Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total,
	})
}

func (handler *Handler) GetProvider(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	provider, err := handler.service.GetProvider(request.Context(), principal.Tenant.ID, request.PathValue("provider_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", toProviderResponse(provider))
}

func (handler *Handler) CreateProvider(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload providerCreateRequest
	if !decodeRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	provider, err := handler.service.CreateProvider(request.Context(), application.CreateProviderInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, Code: payload.Code, Type: payload.Type,
		Issuer: payload.Issuer, ClientID: payload.ClientID, ClientSecret: payload.ClientSecret,
		CallbackURI: payload.CallbackURI, AuthorizationScopes: payload.AuthorizationScopes, DisplayName: payload.DisplayName,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "外部身份提供商已创建", toProviderResponse(provider))
}

func (handler *Handler) UpdateProvider(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload providerUpdateRequest
	if !decodeRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	provider, err := handler.service.UpdateProvider(request.Context(), application.ProviderUpdate{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, ProviderID: request.PathValue("provider_id"), DisplayName: payload.DisplayName,
		Status: payload.Status, ClientID: payload.ClientID, ClientSecret: payload.ClientSecret, CallbackURI: payload.CallbackURI,
		AuthorizationScopes: payload.AuthorizationScopes, Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "外部身份提供商已更新", toProviderResponse(provider))
}

func (handler *Handler) ListUserBindings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	bindings, err := handler.service.ListBindings(request.Context(), principal.Tenant.ID, request.PathValue("user_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]bindingResponse, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, toBindingResponse(binding))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", items)
}

func (handler *Handler) BindUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload bindRequest
	if !decodeRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	binding, err := handler.service.Bind(request.Context(), application.BindInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, UserID: request.PathValue("user_id"),
		ProviderCode: payload.ProviderCode, ExternalSubject: payload.ExternalSubject,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "外部身份已绑定", toBindingResponse(binding))
}

func (handler *Handler) UnbindUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload unbindRequest
	if !decodeRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	binding, err := handler.service.Unbind(request.Context(), application.UnbindInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, UserID: request.PathValue("user_id"),
		BindingID: request.PathValue("binding_id"), Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "外部身份已解绑", toBindingResponse(binding))
}

func (handler *Handler) principal(writer http.ResponseWriter, request *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
	}
	return principal, ok
}

func (handler *Handler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidRequest):
		handler.validation(writer, request)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrVersionConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.VersionConflict)
	default:
		handler.logger.Error("federation management request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func (handler *Handler) validation(writer http.ResponseWriter, request *http.Request) {
	httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, destination any) bool {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	var extra any
	return decoder.Decode(&extra) == io.EOF
}

func parsePageRequest(request *http.Request) (application.PageRequest, error) {
	query := request.URL.Query()
	page, err := positiveQuery(query.Get("page"), 1)
	if err != nil {
		return application.PageRequest{}, err
	}
	pageSize, err := positiveQuery(query.Get("page_size"), 20)
	if err != nil || pageSize > 100 {
		return application.PageRequest{}, errors.New("invalid page size")
	}
	status := strings.TrimSpace(query.Get("filter[status]"))
	if status != "" && status != domain.ProviderStatusActive && status != domain.ProviderStatusDisabled {
		return application.PageRequest{}, errors.New("invalid status")
	}
	return application.PageRequest{Page: page, PageSize: pageSize, Keyword: strings.TrimSpace(query.Get("keyword")), Status: status}, nil
}

func positiveQuery(value string, fallback int) (int, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, errors.New("invalid positive query")
	}
	return parsed, nil
}

func toProviderResponse(provider domain.Provider) providerResponse {
	return providerResponse{
		ProviderID: provider.ID, Code: provider.Code, Type: provider.Type, Issuer: provider.Issuer,
		ClientID: provider.ClientID, CallbackURI: provider.CallbackURI, AuthorizationScopes: append([]string(nil), provider.AuthorizationScopes...),
		ClientSecretConfigured: provider.HasClientSecret(), ClientSecretUpdatedAt: provider.ClientSecretUpdatedAt,
		DisplayName: provider.DisplayName, Status: provider.Status, Version: provider.Version,
		CreatedAt: provider.CreatedAt, UpdatedAt: provider.UpdatedAt,
	}
}

func toBindingResponse(binding domain.Binding) bindingResponse {
	return bindingResponse{
		BindingID: binding.ID, ProviderID: binding.ProviderID, UserID: binding.UserID,
		BoundAt: binding.BoundAt, UnboundAt: binding.UnboundAt, Status: binding.Status, Version: binding.Version,
	}
}
