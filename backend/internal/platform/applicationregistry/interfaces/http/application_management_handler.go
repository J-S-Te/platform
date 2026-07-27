// Package http contains net/http adapters that are mounted through the platform Gin router.
package http

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

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxApplicationManagementRequestBytes = 64 << 10

type managementApplicationService interface {
	ListApplications(context.Context, string, application.PageRequest) (application.PageResult[application.Application], error)
	CreateApplication(context.Context, application.ApplicationCreateInput) (application.Application, error)
	GetApplication(context.Context, string, string) (application.Application, error)
	UpdateApplication(context.Context, application.ApplicationUpdateInput) (application.Application, error)
	ListEnvironments(context.Context, string, string, application.PageRequest) (application.PageResult[application.Environment], error)
	CreateEnvironment(context.Context, application.EnvironmentCreateInput) (application.Environment, error)
	UpdateEnvironment(context.Context, application.EnvironmentUpdateInput) (application.Environment, error)
}

// ManagementHandler serves controlled application registrations and their environments. It
// deliberately has no OAuth client, callback, scope, or credential management endpoints.
type ManagementHandler struct {
	service managementApplicationService
	logger  *slog.Logger
}

// NewManagementHandler constructs the application-registry management HTTP adapter.
func NewManagementHandler(service managementApplicationService, logger *slog.Logger) (*ManagementHandler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("application management handler dependencies must not be nil")
	}
	return &ManagementHandler{service: service, logger: logger}, nil
}

type applicationCreateRequest struct {
	Code            string  `json:"code"`
	Name            string  `json:"name"`
	ApplicationType string  `json:"application_type"`
	OwnerOrgID      *string `json:"owner_org_id"`
	OwnerUserID     *string `json:"owner_user_id"`
	HomepageURL     *string `json:"homepage_url"`
	Description     *string `json:"description"`
	Status          string  `json:"status"`
}

type applicationUpdateRequest struct {
	Name            string  `json:"name"`
	ApplicationType string  `json:"application_type"`
	OwnerOrgID      *string `json:"owner_org_id"`
	OwnerUserID     *string `json:"owner_user_id"`
	HomepageURL     *string `json:"homepage_url"`
	Description     *string `json:"description"`
	Status          string  `json:"status"`
	Version         uint64  `json:"version"`
}

type environmentCreateRequest struct {
	Environment string          `json:"environment"`
	BaseURL     *string         `json:"base_url"`
	UpstreamURL *string         `json:"upstream_url"`
	PathPrefix  *string         `json:"path_prefix"`
	IssuerAlias *string         `json:"issuer_alias"`
	Metadata    json.RawMessage `json:"metadata"`
	Status      string          `json:"status"`
}

type environmentUpdateRequest struct {
	BaseURL     *string         `json:"base_url"`
	UpstreamURL *string         `json:"upstream_url"`
	PathPrefix  *string         `json:"path_prefix"`
	IssuerAlias *string         `json:"issuer_alias"`
	Metadata    json.RawMessage `json:"metadata"`
	Status      string          `json:"status"`
	Version     uint64          `json:"version"`
}

type applicationResponse struct {
	ApplicationID   string    `json:"application_id"`
	Code            string    `json:"code"`
	Name            string    `json:"name"`
	ApplicationType string    `json:"application_type"`
	OwnerOrgID      *string   `json:"owner_org_id,omitempty"`
	OwnerUserID     *string   `json:"owner_user_id,omitempty"`
	HomepageURL     *string   `json:"homepage_url,omitempty"`
	Description     *string   `json:"description,omitempty"`
	Status          string    `json:"status"`
	Version         uint64    `json:"version"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type environmentResponse struct {
	EnvironmentID string          `json:"environment_id"`
	ApplicationID string          `json:"application_id"`
	Environment   string          `json:"environment"`
	BaseURL       *string         `json:"base_url,omitempty"`
	UpstreamURL   *string         `json:"upstream_url,omitempty"`
	PathPrefix    *string         `json:"path_prefix,omitempty"`
	IssuerAlias   *string         `json:"issuer_alias,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	Status        string          `json:"status"`
	Version       uint64          `json:"version"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type managementPageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// ListApplications handles GET /api/v1/applications.
func (handler *ManagementHandler) ListApplications(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	result, err := handler.service.ListApplications(request.Context(), principal.Tenant.ID, applicationManagementPageQuery(request))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	items := make([]applicationResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, applicationToResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用查询成功", managementPageResponse[applicationResponse]{
		Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total,
	})
}

// CreateApplication handles POST /api/v1/applications.
func (handler *ManagementHandler) CreateApplication(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload applicationCreateRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}

	created, err := handler.service.CreateApplication(request.Context(), application.ApplicationCreateInput{
		TenantID:        principal.Tenant.ID,
		OperatorID:      principal.User.ID,
		Code:            payload.Code,
		Name:            payload.Name,
		ApplicationType: payload.ApplicationType,
		OwnerOrgID:      payload.OwnerOrgID,
		OwnerUserID:     payload.OwnerUserID,
		HomepageURL:     payload.HomepageURL,
		Description:     payload.Description,
		Status:          payload.Status,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "应用已创建", applicationToResponse(created))
}

// GetApplication handles GET /api/v1/applications/:application_id.
func (handler *ManagementHandler) GetApplication(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	item, err := handler.service.GetApplication(request.Context(), principal.Tenant.ID, request.PathValue("application_id"))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用查询成功", applicationToResponse(item))
}

// UpdateApplication handles PATCH /api/v1/applications/:application_id.
func (handler *ManagementHandler) UpdateApplication(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload applicationUpdateRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}

	updated, err := handler.service.UpdateApplication(request.Context(), application.ApplicationUpdateInput{
		TenantID:        principal.Tenant.ID,
		ApplicationID:   request.PathValue("application_id"),
		OperatorID:      principal.User.ID,
		Name:            payload.Name,
		ApplicationType: payload.ApplicationType,
		OwnerOrgID:      payload.OwnerOrgID,
		OwnerUserID:     payload.OwnerUserID,
		HomepageURL:     payload.HomepageURL,
		Description:     payload.Description,
		Status:          payload.Status,
		Version:         payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用已更新", applicationToResponse(updated))
}

// ListEnvironments handles GET /api/v1/applications/:application_id/environments.
func (handler *ManagementHandler) ListEnvironments(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	result, err := handler.service.ListEnvironments(
		request.Context(), principal.Tenant.ID, request.PathValue("application_id"), applicationManagementPageQuery(request),
	)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	items := make([]environmentResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, environmentToResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用环境查询成功", managementPageResponse[environmentResponse]{
		Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total,
	})
}

// CreateEnvironment handles POST /api/v1/applications/:application_id/environments.
func (handler *ManagementHandler) CreateEnvironment(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload environmentCreateRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}

	created, err := handler.service.CreateEnvironment(request.Context(), application.EnvironmentCreateInput{
		TenantID:      principal.Tenant.ID,
		ApplicationID: request.PathValue("application_id"),
		OperatorID:    principal.User.ID,
		Environment:   payload.Environment,
		BaseURL:       payload.BaseURL,
		UpstreamURL:   payload.UpstreamURL,
		PathPrefix:    payload.PathPrefix,
		IssuerAlias:   payload.IssuerAlias,
		Metadata:      payload.Metadata,
		Status:        payload.Status,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "应用环境已创建", environmentToResponse(created))
}

// UpdateEnvironment handles PATCH /api/v1/applications/:application_id/environments/:environment_id.
func (handler *ManagementHandler) UpdateEnvironment(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload environmentUpdateRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}

	updated, err := handler.service.UpdateEnvironment(request.Context(), application.EnvironmentUpdateInput{
		TenantID:      principal.Tenant.ID,
		ApplicationID: request.PathValue("application_id"),
		EnvironmentID: request.PathValue("environment_id"),
		OperatorID:    principal.User.ID,
		BaseURL:       payload.BaseURL,
		UpstreamURL:   payload.UpstreamURL,
		PathPrefix:    payload.PathPrefix,
		IssuerAlias:   payload.IssuerAlias,
		Metadata:      payload.Metadata,
		Status:        payload.Status,
		Version:       payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "应用环境已更新", environmentToResponse(updated))
}

func (handler *ManagementHandler) principal(writer http.ResponseWriter, request *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}

func (handler *ManagementHandler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
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
		handler.logger.Error("application management request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func decodeApplicationManagementJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxApplicationManagementRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}

func applicationManagementPageQuery(request *http.Request) application.PageRequest {
	query := request.URL.Query()
	return application.PageRequest{
		Page:     applicationManagementPositiveInt(query.Get("page")),
		PageSize: applicationManagementPositiveInt(query.Get("page_size")),
		Keyword:  query.Get("keyword"),
		Status:   query.Get("status"),
	}
}

func applicationManagementPositiveInt(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 {
		return 0
	}
	return parsed
}

func applicationToResponse(item application.Application) applicationResponse {
	return applicationResponse{
		ApplicationID: item.ID, Code: item.Code, Name: item.Name, ApplicationType: item.ApplicationType,
		OwnerOrgID: item.OwnerOrgID, OwnerUserID: item.OwnerUserID, HomepageURL: item.HomepageURL, Description: item.Description,
		Status: item.Status, Version: item.Version, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func environmentToResponse(item application.Environment) environmentResponse {
	return environmentResponse{
		EnvironmentID: item.ID, ApplicationID: item.ApplicationID, Environment: item.Environment,
		BaseURL: item.BaseURL, UpstreamURL: item.UpstreamURL, PathPrefix: item.PathPrefix,
		IssuerAlias: item.IssuerAlias, Metadata: item.Metadata, Status: item.Status,
		Version: item.Version, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}
