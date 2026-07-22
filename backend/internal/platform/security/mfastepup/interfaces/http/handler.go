// Package http exposes authenticated MFA step-up challenge and grant endpoints.
package http

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	stdhttp "net/http"
	"strings"
	"time"

	mfadomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/mfastepup/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxRequestBytes = 8 << 10

type createChallengeRequest struct {
	FactorID string `json:"factor_id"`
}

type verifyChallengeRequest struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

type createChallengeResponse struct {
	ChallengeID string    `json:"challenge_id"`
	Challenge   string    `json:"challenge"`
	ExpiresAt   time.Time `json:"expires_at"`
	MaxAttempts uint16    `json:"max_attempts"`
}

type verifyChallengeResponse struct {
	Grant              string    `json:"grant"`
	ExpiresAt          time.Time `json:"expires_at"`
	VerificationMethod string    `json:"verification_method"`
}

// Handler translates the step-up application boundary into the platform HTTP envelope. It never
// writes challenge, grant, TOTP or recovery-code values to logs.
type Handler struct {
	service *application.Service
	logger  *slog.Logger
}

// NewHandler validates dependencies and constructs the HTTP adapter.
func NewHandler(service *application.Service, logger *slog.Logger) (*Handler, error) {
	if service == nil {
		return nil, errors.New("MFA step-up HTTP service must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{service: service, logger: logger}, nil
}

// CreateChallenge starts an MFA challenge bound to the current authenticated session.
func (handler *Handler) CreateChallenge(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload createChallengeRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.CreateChallenge(request.Context(), application.CreateChallengeInput{
		TenantID: principal.Tenant.ID, AccountID: principal.Account.ID, SessionID: principal.SessionID, FactorID: payload.FactorID,
	})
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusCreated, "高风险操作 MFA 挑战创建成功", createChallengeResponse{
		ChallengeID: result.ChallengeID, Challenge: result.Challenge, ExpiresAt: result.ExpiresAt, MaxAttempts: result.MaxAttempts,
	})
}

// VerifyChallenge verifies a current-session challenge and returns a one-time grant for the next
// protected operation. The client must send the grant in X-MFA-Step-Up-Grant.
func (handler *Handler) VerifyChallenge(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload verifyChallengeRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.VerifyChallenge(request.Context(), application.VerifyChallengeInput{
		TenantID: principal.Tenant.ID, AccountID: principal.Account.ID, SessionID: principal.SessionID,
		Challenge: payload.Challenge, Code: payload.Code,
	})
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "高风险操作 MFA 验证成功", verifyChallengeResponse{
		Grant: result.Grant, ExpiresAt: result.ExpiresAt, VerificationMethod: result.VerificationMethod,
	})
}

func (handler *Handler) principal(writer stdhttp.ResponseWriter, request *stdhttp.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.Account.ID) == "" || strings.TrimSpace(principal.SessionID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}

func decodeJSON(writer stdhttp.ResponseWriter, request *stdhttp.Request, target any) bool {
	request.Body = stdhttp.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}

func (handler *Handler) writeApplicationError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, mfadomain.ErrInvalidInput):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, mfadomain.ErrFactorNotFound), errors.Is(err, domain.ErrChallengeNotFound), errors.Is(err, mfadomain.ErrChallengeNotFound):
		httpresponse.WriteError(writer, request, stdhttp.StatusNotFound, httperror.NotFound)
	case errors.Is(err, domain.ErrChallengeBinding), errors.Is(err, domain.ErrChallengeNotPending), errors.Is(err, mfadomain.ErrChallengeConsumed):
		httpresponse.WriteError(writer, request, stdhttp.StatusForbidden, httperror.New("AUTH_MFA_STEP_UP_REQUIRED", "该高风险操作需要当前会话完成 MFA 二次验证", nil))
	case errors.Is(err, domain.ErrChallengeExpired), errors.Is(err, mfadomain.ErrChallengeExpired):
		httpresponse.WriteError(writer, request, stdhttp.StatusGone, httperror.New("AUTH_MFA_STEP_UP_EXPIRED", "MFA 二次验证挑战已过期，请重新发起验证", nil))
	case errors.Is(err, mfadomain.ErrChallengeAttemptsExceeded):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.New("AUTH_MFA_ATTEMPTS_EXCEEDED", "MFA 挑战尝试次数已用尽", nil))
	case errors.Is(err, mfadomain.ErrInvalidVerificationCode), errors.Is(err, mfadomain.ErrFactorUnavailable):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.New("AUTH_MFA_VERIFICATION_FAILED", "动态验证码或恢复码错误", nil))
	default:
		handler.logger.Error("MFA step-up HTTP request failed", "error", err, "path", request.URL.Path)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}
