package identityhttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// AccountLifecycleHandler 暴露常规本地账号开通与密码生命周期端点。
// 明文密码被刻意限制在请求处理与紧随其后的重置响应内；本 handler
// 绝不记录或审计它们。
type AccountLifecycleHandler struct {
	service     accountLifecycleApplicationService
	authHandler *Handler
	logger      *slog.Logger
}

type accountLifecycleApplicationService interface {
	CreateLocalAccount(context.Context, application.LocalAccountCreateInput) (domain.Account, error)
	InitializePassword(context.Context, application.PasswordInitializeInput) (domain.Account, error)
	ResetPassword(context.Context, application.PasswordResetInput) (application.PasswordResetResult, error)
	ChangeOwnPassword(context.Context, application.PasswordChangeInput) error
}

// NewAccountLifecycleHandler 构造本地账号生命周期操作的 HTTP 适配器。
func NewAccountLifecycleHandler(service accountLifecycleApplicationService, authHandler *Handler, logger *slog.Logger) (*AccountLifecycleHandler, error) {
	if service == nil || authHandler == nil || logger == nil {
		return nil, errors.New("account lifecycle handler dependencies must not be nil")
	}
	return &AccountLifecycleHandler{service: service, authHandler: authHandler, logger: logger}, nil
}

type localAccountCreateRequest struct {
	UserID          string     `json:"user_id"`
	AccountName     string     `json:"account_name"`
	InitialPassword string     `json:"initial_password"`
	ValidUntil      *time.Time `json:"valid_until"`
}

type passwordInitializeRequest struct {
	Password string `json:"password"`
	Version  uint64 `json:"version"`
}

type passwordResetRequest struct {
	Version uint64 `json:"version"`
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type passwordResetResponse struct {
	AccountID         string `json:"account_id"`
	TemporaryPassword string `json:"temporary_password"`
}

// CreateLocalAccount 开通 HUMAN/LOCAL 账号及其初始密码凭据。
func (handler *AccountLifecycleHandler) CreateLocalAccount(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}

	var payload localAccountCreateRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	account, err := handler.service.CreateLocalAccount(request.Context(), application.LocalAccountCreateInput{
		TenantID:        principal.Tenant.ID,
		OperatorID:      principal.User.ID,
		UserID:          payload.UserID,
		AccountName:     payload.AccountName,
		InitialPassword: payload.InitialPassword,
		ValidUntil:      payload.ValidUntil,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "账号已创建", toAccountResponse(account))
}

// InitializePassword 仅为尚无凭据的本地账号设置密码。
func (handler *AccountLifecycleHandler) InitializePassword(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}

	var payload passwordInitializeRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	account, err := handler.service.InitializePassword(request.Context(), application.PasswordInitializeInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, AccountID: request.PathValue("account_id"), Password: payload.Password, Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "密码已初始化", toAccountResponse(account))
}

// ResetPassword 生成由管理员线下交付的高强度服务端随机密码。该值仅返回一次，
// 不写入日志或审计记录。
func (handler *AccountLifecycleHandler) ResetPassword(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}

	var payload passwordResetRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	result, err := handler.service.ResetPassword(request.Context(), application.PasswordResetInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, AccountID: request.PathValue("account_id"), Version: payload.Version,
	})
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}

	// 生成的密码不得进入任何日志、审计事件、重试缓存或响应头。
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "密码已重置，请立即复制并线下交付", passwordResetResponse{
		AccountID: result.AccountID, TemporaryPassword: result.TemporaryPassword,
	})
}

// ChangeOwnPassword 修改已认证账号的密码；吊销全部服务端会话后清除其浏览器
// 会话 cookie。
func (handler *AccountLifecycleHandler) ChangeOwnPassword(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		handler.unauthenticated(writer, request)
		return
	}

	var payload passwordChangeRequest
	if !decodeManagementRequest(writer, request, &payload) {
		handler.validation(writer, request)
		return
	}
	if err := handler.service.ChangeOwnPassword(request.Context(), application.PasswordChangeInput{
		TenantID: principal.Tenant.ID, AccountID: principal.Account.ID, OperatorID: principal.User.ID, CurrentPassword: payload.CurrentPassword, NewPassword: payload.NewPassword,
	}); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	handler.authHandler.ClearSessionCookie(writer)
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "密码已修改，请重新登录", map[string]any{})
}

func (handler *AccountLifecycleHandler) writeError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrValidation):
		handler.validation(writer, request)
	case errors.Is(err, application.ErrNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrVersionConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.VersionConflict)
	case errors.Is(err, application.ErrExternalIdentityUnavailable):
		httpresponse.WriteError(writer, request, http.StatusBadGateway, httperror.DependencyUnavailable)
	case errors.Is(err, application.ErrUnauthenticated):
		handler.unauthenticated(writer, request)
	default:
		handler.logger.Error("identity account lifecycle request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func (handler *AccountLifecycleHandler) validation(writer http.ResponseWriter, request *http.Request) {
	httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
}

func (handler *AccountLifecycleHandler) unauthenticated(writer http.ResponseWriter, request *http.Request) {
	httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
}
