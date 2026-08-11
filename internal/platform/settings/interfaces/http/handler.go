// Package settingshttp adapts typed platform settings use cases to HTTP.
package settingshttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	settingsapplication "github.com/J-S-Te/Basic-Platform/internal/platform/settings/application"
	settingsdomain "github.com/J-S-Te/Basic-Platform/internal/platform/settings/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxRequestBytes = 16 * 1024

type service interface {
	GetPlatformSettings(ctx context.Context, tenantID string) (settingsdomain.PlatformSettings, error)
	UpdatePlatformSettings(ctx context.Context, input settingsapplication.PlatformSettingsUpdateInput) (settingsdomain.PlatformSettings, error)
	GetNotificationSettings(ctx context.Context, tenantID string) (settingsdomain.NotificationSettings, error)
	UpdateNotificationSettings(ctx context.Context, input settingsapplication.NotificationSettingsUpdateInput) (settingsdomain.NotificationSettings, error)
	GetAccessSettings(ctx context.Context, tenantID string) (settingsdomain.AccessSettings, error)
	UpdateAccessSettings(ctx context.Context, input settingsapplication.AccessSettingsUpdateInput) (settingsdomain.AccessSettings, error)
	ApplyAccessSettings(ctx context.Context, tenantID string, applier settingsapplication.AccessApplier) (settingsdomain.AccessSettings, error)
}

// Handler provides authenticated tenant-scoped settings endpoints.
type Handler struct {
	service service
	applier settingsapplication.AccessApplier
	logger  *slog.Logger
}

// NewHandler validates dependencies and creates the settings HTTP adapter. The applier is
// optional: the public-access apply endpoint returns a clear error when no deployment agent
// (production/remote deployment) is wired.
func NewHandler(service service, applier settingsapplication.AccessApplier, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("settings HTTP handler dependencies must not be nil")
	}

	return &Handler{service: service, applier: applier, logger: logger}, nil
}

type platformSettingsPayload struct {
	OrganizationName  string `json:"organization_name"`
	OrganizationAlias string `json:"organization_alias"`
	Timezone          string `json:"timezone"`
	Qualification     string `json:"qualification"`
	Version           uint64 `json:"version"`
}

type platformSettingsResponse struct {
	ID                string `json:"id,omitempty"`
	OrganizationName  string `json:"organization_name"`
	OrganizationAlias string `json:"organization_alias"`
	Timezone          string `json:"timezone"`
	Qualification     string `json:"qualification"`
	Version           uint64 `json:"version"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

type notificationSettingsPayload struct {
	InboxEnabled      bool   `json:"inbox_enabled"`
	EmailEnabled      bool   `json:"email_enabled"`
	ReminderFrequency string `json:"reminder_frequency"`
	Version           uint64 `json:"version"`
}

type notificationSettingsResponse struct {
	ID                string `json:"id,omitempty"`
	InboxEnabled      bool   `json:"inbox_enabled"`
	EmailEnabled      bool   `json:"email_enabled"`
	ReminderFrequency string `json:"reminder_frequency"`
	Version           uint64 `json:"version"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

type accessSettingsPayload struct {
	PublicOrigin              string `json:"public_origin"`
	AllowInsecureHTTPRedirect bool   `json:"allow_insecure_http_redirect"`
	Version                   uint64 `json:"version"`
}

type accessSettingsResponse struct {
	ID                        string `json:"id,omitempty"`
	PublicOrigin              string `json:"public_origin"`
	AllowInsecureHTTPRedirect bool   `json:"allow_insecure_http_redirect"`
	Version                   uint64 `json:"version"`
	UpdatedAt                 string `json:"updated_at,omitempty"`
}

// GetPlatformSettings returns the tenant's typed management-console settings.
func (handler *Handler) GetPlatformSettings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	settings, err := handler.service.GetPlatformSettings(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusOK, "平台基础设置查询成功", platformSettingsToResponse(settings))
}

// UpdatePlatformSettings replaces typed management-console settings using optimistic locking.
func (handler *Handler) UpdatePlatformSettings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload platformSettingsPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}

	settings, err := handler.service.UpdatePlatformSettings(request.Context(), settingsapplication.PlatformSettingsUpdateInput{
		TenantID:          principal.Tenant.ID,
		OperatorID:        principal.User.ID,
		OrganizationName:  payload.OrganizationName,
		OrganizationAlias: payload.OrganizationAlias,
		Timezone:          payload.Timezone,
		Qualification:     payload.Qualification,
		Version:           payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusOK, "平台基础设置已保存", platformSettingsToResponse(settings))
}

// GetNotificationSettings returns settings for only the supported inbox and email channels.
func (handler *Handler) GetNotificationSettings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	settings, err := handler.service.GetNotificationSettings(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusOK, "通知设置查询成功", notificationSettingsToResponse(settings))
}

// UpdateNotificationSettings saves tenant-level inbox/email preferences and reminder frequency.
func (handler *Handler) UpdateNotificationSettings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}

	var payload notificationSettingsPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}

	settings, err := handler.service.UpdateNotificationSettings(request.Context(), settingsapplication.NotificationSettingsUpdateInput{
		TenantID:          principal.Tenant.ID,
		OperatorID:        principal.User.ID,
		InboxEnabled:      payload.InboxEnabled,
		EmailEnabled:      payload.EmailEnabled,
		ReminderFrequency: settingsdomain.ReminderFrequency(strings.ToUpper(strings.TrimSpace(payload.ReminderFrequency))),
		Version:           payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	httpresponse.WriteSuccess(writer, request, http.StatusOK, "通知设置已保存", notificationSettingsToResponse(settings))
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
	case errors.Is(err, settingsapplication.ErrValidation):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, settingsapplication.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, settingsapplication.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, settingsapplication.ErrVersionConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.VersionConflict)
	default:
		handler.logger.Error("settings request failed", "error", err, "path", request.URL.Path)
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

func platformSettingsToResponse(settings settingsdomain.PlatformSettings) platformSettingsResponse {
	response := platformSettingsResponse{
		ID:                settings.ID,
		OrganizationName:  settings.OrganizationName,
		OrganizationAlias: settings.OrganizationAlias,
		Timezone:          settings.Timezone,
		Qualification:     settings.Qualification,
		Version:           settings.Version,
	}
	if !settings.UpdatedAt.IsZero() {
		response.UpdatedAt = settings.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}

	return response
}

func notificationSettingsToResponse(settings settingsdomain.NotificationSettings) notificationSettingsResponse {
	response := notificationSettingsResponse{
		ID:                settings.ID,
		InboxEnabled:      settings.InboxEnabled,
		EmailEnabled:      settings.EmailEnabled,
		ReminderFrequency: string(settings.ReminderFrequency),
		Version:           settings.Version,
	}
	if !settings.UpdatedAt.IsZero() {
		response.UpdatedAt = settings.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}

	return response
}

// GetAccessSettings returns the tenant's public-access configuration.
func (handler *Handler) GetAccessSettings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	settings, err := handler.service.GetAccessSettings(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "对外访问配置查询成功", accessSettingsToResponse(settings))
}

// UpdateAccessSettings replaces the tenant's public-access configuration using optimistic locking.
func (handler *Handler) UpdateAccessSettings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload accessSettingsPayload
	if !decodeJSON(writer, request, &payload) {
		return
	}
	settings, err := handler.service.UpdateAccessSettings(request.Context(), settingsapplication.AccessSettingsUpdateInput{
		TenantID:                  principal.Tenant.ID,
		OperatorID:                principal.User.ID,
		PublicOrigin:              payload.PublicOrigin,
		AllowInsecureHTTPRedirect: payload.AllowInsecureHTTPRedirect,
		Version:                   payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "对外访问配置已保存", accessSettingsToResponse(settings))
}

// ApplyAccessSettings applies the saved public-access configuration through the deployment agent.
func (handler *Handler) ApplyAccessSettings(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	if handler.applier == nil {
		httpresponse.WriteError(writer, request, http.StatusServiceUnavailable, httperror.New(
			"PLATFORM_DEPENDENCY_UNAVAILABLE",
			"该环境未启用部署 Agent，不支持在界面上应用对外访问配置",
			map[string]string{"next_action": "生产/远程环境请直接修改服务器 docker/.env.lan 或使用 lan-access.sh；本地开发请确认 subsystem-provisioner 已随 docker-local.sh 启动"},
		))
		return
	}
	settings, err := handler.service.ApplyAccessSettings(request.Context(), principal.Tenant.ID, handler.applier)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "对外访问配置已应用", accessSettingsToResponse(settings))
}

func accessSettingsToResponse(settings settingsdomain.AccessSettings) accessSettingsResponse {
	return accessSettingsResponse{
		ID:                        settings.ID,
		PublicOrigin:              settings.PublicOrigin,
		AllowInsecureHTTPRedirect: settings.AllowInsecureHTTPRedirect,
		Version:                   settings.Version,
		UpdatedAt:                 settings.UpdatedAt.Format(time.RFC3339),
	}
}
