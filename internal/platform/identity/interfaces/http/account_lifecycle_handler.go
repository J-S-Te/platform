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

// AccountLifecycleHandler exposes normal local-account provisioning and password lifecycle
// endpoints. Plaintext passwords are deliberately confined to request handling and the immediate
// reset response; this handler never logs or audits them.
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

// NewAccountLifecycleHandler constructs the HTTP adapter for local account lifecycle operations.
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

// CreateLocalAccount provisions a HUMAN/LOCAL account and its initial password credential.
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

// InitializePassword sets a password only for a local account that does not yet have a credential.
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

// ResetPassword creates a strong server-generated password for offline delivery by the
// administrator. The value is returned exactly once and is not written to logs or audit records.
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

	// Do not add the generated password to any log, audit event, retry cache, or response header.
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "密码已重置，请立即复制并线下交付", passwordResetResponse{
		AccountID: result.AccountID, TemporaryPassword: result.TemporaryPassword,
	})
}

// ChangeOwnPassword changes the authenticated account's password and clears its browser session
// cookie after revoking all server-side sessions.
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
