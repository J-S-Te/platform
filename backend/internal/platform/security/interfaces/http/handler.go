// Package securityhttp adapts login-security use cases to the platform HTTP contract.
package securityhttp

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

	securityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/application"
	securitydomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxRequestBytes = 16 * 1024

// service defines the security use cases exposed by this HTTP adapter.
type service interface {
	GetLoginPolicy(ctx context.Context, tenantID string) (securitydomain.LoginPolicy, error)
	UpdateLoginPolicy(ctx context.Context, input securityapplication.LoginPolicyUpdateInput) (securitydomain.LoginPolicy, error)
	ListLockedAccounts(ctx context.Context, tenantID string, query securityapplication.PageRequest) (securityapplication.PageResult[securitydomain.LockedAccount], error)
	UnlockAccount(ctx context.Context, input securityapplication.UnlockInput) (securitydomain.LockedAccount, error)
	ListRiskEvents(ctx context.Context, tenantID string, query securityapplication.PageRequest) (securityapplication.PageResult[securitydomain.RiskEvent], error)
	ResolveRiskEvent(ctx context.Context, input securityapplication.RiskEventResolveInput) (securitydomain.RiskEvent, error)
}

// Handler provides tenant-scoped login-security HTTP endpoints.
type Handler struct {
	service service
	logger  *slog.Logger
}

// NewHandler constructs a login-security HTTP handler.
func NewHandler(service service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("security HTTP handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

type loginPolicyPayload struct {
	MaxFailedAttempts         uint   `json:"max_failed_attempts"`
	LockoutDurationSeconds    uint   `json:"lockout_duration_seconds"`
	FailureResetWindowSeconds uint   `json:"failure_reset_window_seconds"`
	Version                   uint64 `json:"version"`
}

type loginPolicyResponse struct {
	TenantID                  string `json:"tenant_id"`
	MaxFailedAttempts         uint   `json:"max_failed_attempts"`
	LockoutDurationSeconds    uint   `json:"lockout_duration_seconds"`
	FailureResetWindowSeconds uint   `json:"failure_reset_window_seconds"`
	Version                   uint64 `json:"version"`
	UpdatedAt                 string `json:"updated_at,omitempty"`
}

type lockedAccountResponse struct {
	AccountID    string  `json:"account_id"`
	AccountName  string  `json:"account_name"`
	UserID       string  `json:"user_id,omitempty"`
	UserName     string  `json:"user_name,omitempty"`
	LockedUntil  *string `json:"locked_until"`
	LastFailedAt *string `json:"last_failed_at"`
	FailureCount uint    `json:"failure_count"`
}

type riskEventResolvePayload struct {
	ResolutionComment string `json:"resolution_comment"`
	Version           uint64 `json:"version"`
}

type riskEventResponse struct {
	RiskEventID       string         `json:"risk_event_id"`
	AccountID         *string        `json:"account_id,omitempty"`
	AccountName       *string        `json:"account_name,omitempty"`
	EventType         string         `json:"event_type"`
	SubjectType       string         `json:"subject_type"`
	SubjectID         string         `json:"subject_id"`
	RiskLevel         string         `json:"risk_level"`
	RiskScore         uint           `json:"risk_score"`
	SourceIP          *string        `json:"source_ip,omitempty"`
	DetectionRule     string         `json:"detection_rule"`
	Status            string         `json:"status"`
	OccurredAt        string         `json:"occurred_at"`
	ResolvedAt        *string        `json:"resolved_at"`
	ResolvedBy        *string        `json:"resolved_by,omitempty"`
	ResolutionComment *string        `json:"resolution_comment,omitempty"`
	Metadata          map[string]any `json:"metadata"`
	Version           uint64         `json:"version"`
}

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// GetLoginPolicy returns the current tenant login policy, including the documented default values
// before a tenant has saved a custom policy.
func (handler *Handler) GetLoginPolicy(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	result, err := handler.service.GetLoginPolicy(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "登录安全策略查询成功", loginPolicyToResponse(result))
}

// UpdateLoginPolicy replaces the tenant login policy through optimistic locking.
func (handler *Handler) UpdateLoginPolicy(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload loginPolicyPayload
	if !decode(writer, request, &payload) {
		return
	}
	result, err := handler.service.UpdateLoginPolicy(request.Context(), securityapplication.LoginPolicyUpdateInput{
		TenantID:                  principal.Tenant.ID,
		OperatorID:                principal.User.ID,
		MaxFailedAttempts:         payload.MaxFailedAttempts,
		LockoutDurationSeconds:    payload.LockoutDurationSeconds,
		FailureResetWindowSeconds: payload.FailureResetWindowSeconds,
		Version:                   payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "登录安全策略已更新", loginPolicyToResponse(result))
}

// ListLockedAccounts lists active account locks for the current tenant.
func (handler *Handler) ListLockedAccounts(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	result, err := handler.service.ListLockedAccounts(request.Context(), principal.Tenant.ID, pageQuery(request))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]lockedAccountResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, lockedAccountToResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "锁定账号查询成功", pageResponse[lockedAccountResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// UnlockAccount clears one active lock and resets its active failed-login counter.
func (handler *Handler) UnlockAccount(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	result, err := handler.service.UnlockAccount(request.Context(), securityapplication.UnlockInput{
		TenantID: principal.Tenant.ID, AccountID: request.PathValue("account_id"), OperatorID: principal.User.ID,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "账号已解锁", lockedAccountToResponse(result))
}

// ListRiskEvents returns tenant-scoped security risk events without exposing credentials or tokens.
func (handler *Handler) ListRiskEvents(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	result, err := handler.service.ListRiskEvents(request.Context(), principal.Tenant.ID, pageQuery(request))
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	items := make([]riskEventResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, riskEventToResponse(item))
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "风险事件查询成功", pageResponse[riskEventResponse]{Items: items, Page: result.Page, PageSize: result.PageSize, Total: result.Total})
}

// ResolveRiskEvent records an administrator's disposition for an open risk event.
func (handler *Handler) ResolveRiskEvent(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload riskEventResolvePayload
	if !decode(writer, request, &payload) {
		return
	}
	result, err := handler.service.ResolveRiskEvent(request.Context(), securityapplication.RiskEventResolveInput{
		TenantID: principal.Tenant.ID, RiskEventID: request.PathValue("risk_event_id"), OperatorID: principal.User.ID,
		ResolutionComment: payload.ResolutionComment, Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "风险事件已处置", riskEventToResponse(result))
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
	case errors.Is(err, securityapplication.ErrValidation):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, securityapplication.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, securityapplication.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	default:
		handler.logger.Error("login security HTTP operation failed", "error", err, "path", request.URL.Path)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func decode(writer http.ResponseWriter, request *http.Request, target any) bool {
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

func pageQuery(request *http.Request) securityapplication.PageRequest {
	page, _ := strconv.Atoi(request.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(request.URL.Query().Get("page_size"))
	return securityapplication.PageRequest{
		Page: page, PageSize: pageSize, Keyword: strings.TrimSpace(request.URL.Query().Get("keyword")), Status: strings.TrimSpace(request.URL.Query().Get("status")),
	}
}

func loginPolicyToResponse(value securitydomain.LoginPolicy) loginPolicyResponse {
	response := loginPolicyResponse{TenantID: value.TenantID, MaxFailedAttempts: value.MaxFailedAttempts, LockoutDurationSeconds: value.LockoutDurationSeconds, FailureResetWindowSeconds: value.FailureResetWindowSeconds, Version: value.Version}
	if !value.UpdatedAt.IsZero() {
		response.UpdatedAt = value.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	return response
}

func lockedAccountToResponse(value securitydomain.LockedAccount) lockedAccountResponse {
	return lockedAccountResponse{AccountID: value.AccountID, AccountName: value.AccountName, UserID: value.UserID, UserName: value.UserName, LockedUntil: timeToString(value.LockedUntil), LastFailedAt: timeToString(value.LastFailedAt), FailureCount: value.FailureCount}
}

func riskEventToResponse(value securitydomain.RiskEvent) riskEventResponse {
	return riskEventResponse{RiskEventID: value.RiskEventID, AccountID: value.AccountID, AccountName: value.AccountName, EventType: value.EventType, SubjectType: value.SubjectType, SubjectID: value.SubjectID, RiskLevel: value.RiskLevel, RiskScore: value.RiskScore, SourceIP: value.SourceIP, DetectionRule: value.DetectionRule, Status: value.Status, OccurredAt: value.OccurredAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"), ResolvedAt: timeToString(value.ResolvedAt), ResolvedBy: value.ResolvedBy, ResolutionComment: value.ResolutionComment, Metadata: value.Metadata, Version: value.Version}
}

func timeToString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return &formatted
}
