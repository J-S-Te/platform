// Package securityhttp 将登录安全用例适配为平台 HTTP 契约（租户范围、错误码映射统一走 httperror）。
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

	securityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/security/application"
	securitydomain "github.com/J-S-Te/Basic-Platform/internal/platform/security/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const maxRequestBytes = 16 * 1024

// service 约束本 HTTP 适配器暴露的登录安全用例集合，防止适配器越界依赖应用层实现细节。
type service interface {
	GetLoginPolicy(ctx context.Context, tenantID string) (securitydomain.LoginPolicy, error)
	UpdateLoginPolicy(ctx context.Context, input securityapplication.LoginPolicyUpdateInput) (securitydomain.LoginPolicy, error)
	ListLockedAccounts(ctx context.Context, tenantID string, query securityapplication.PageRequest) (securityapplication.PageResult[securitydomain.LockedAccount], error)
	UnlockAccount(ctx context.Context, input securityapplication.UnlockInput) (securitydomain.LockedAccount, error)
}

// Handler 提供租户范围的登录安全 HTTP 端点；数据范围由认证中间件注入的租户身份决定，
// 不接受请求参数指定租户。
type Handler struct {
	service service
	logger  *slog.Logger
}

// NewHandler 构造登录安全 HTTP 处理器；依赖缺失时失败关闭。
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
	IdleTimeoutSeconds        uint   `json:"idle_timeout_seconds"`
	Version                   uint64 `json:"version"`
}

type loginPolicyResponse struct {
	TenantID                  string `json:"tenant_id"`
	MaxFailedAttempts         uint   `json:"max_failed_attempts"`
	LockoutDurationSeconds    uint   `json:"lockout_duration_seconds"`
	FailureResetWindowSeconds uint   `json:"failure_reset_window_seconds"`
	IdleTimeoutSeconds        uint   `json:"idle_timeout_seconds"`
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

type pageResponse[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// GetLoginPolicy 返回当前租户登录策略；租户尚未保存自定义策略时返回文档化默认值。
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

// UpdateLoginPolicy 通过乐观锁整体替换租户登录策略，版本冲突时返回 409。
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
		IdleTimeoutSeconds:        payload.IdleTimeoutSeconds,
		Version:                   payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "登录安全策略已更新", loginPolicyToResponse(result))
}

// ListLockedAccounts 分页列出当前租户仍生效的账号锁定。
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

// UnlockAccount 解除一个生效中的锁定并清零其活跃失败计数，不恢复已被撤销的会话。
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
		Page: page, PageSize: pageSize, Keyword: strings.TrimSpace(request.URL.Query().Get("keyword")),
	}
}

func loginPolicyToResponse(value securitydomain.LoginPolicy) loginPolicyResponse {
	response := loginPolicyResponse{TenantID: value.TenantID, MaxFailedAttempts: value.MaxFailedAttempts, LockoutDurationSeconds: value.LockoutDurationSeconds, FailureResetWindowSeconds: value.FailureResetWindowSeconds, IdleTimeoutSeconds: value.IdleTimeoutSeconds, Version: value.Version}
	if !value.UpdatedAt.IsZero() {
		response.UpdatedAt = value.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	return response
}

func lockedAccountToResponse(value securitydomain.LockedAccount) lockedAccountResponse {
	return lockedAccountResponse{AccountID: value.AccountID, AccountName: value.AccountName, UserID: value.UserID, UserName: value.UserName, LockedUntil: timeToString(value.LockedUntil), LastFailedAt: timeToString(value.LastFailedAt), FailureCount: value.FailureCount}
}

func timeToString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return &formatted
}
