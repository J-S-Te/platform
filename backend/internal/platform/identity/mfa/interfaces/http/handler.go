// Package mfahttp adapts MFA application use cases to the platform's net/http response contract.
package mfahttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
)

const maxRequestBytes = 8 * 1024

// service is the MFA use-case boundary exposed through this HTTP adapter.
type service interface {
	PrepareTOTP(context.Context, application.PrepareTOTPInput) (application.PreparedTOTP, error)
	ConfirmTOTP(context.Context, application.ConfirmTOTPInput) (application.ConfirmedTOTP, error)
	DisableTOTP(context.Context, application.DisableTOTPInput) (domain.TOTPFactor, error)
	CreateChallenge(context.Context, application.CreateChallengeInput) (application.CreatedChallenge, error)
	VerifyChallenge(context.Context, application.VerifyChallengeInput) (application.ChallengeVerification, error)
}

// Handler provides standalone net/http handlers. Router/bootstrap integration deliberately remains
// outside this package so the identity enhancement can be composed by the application owner.
type Handler struct {
	service service
	logger  *slog.Logger
}

// NewHandler validates dependencies and constructs the MFA HTTP adapter.
func NewHandler(service service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("MFA HTTP handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

type prepareTOTPRequest struct {
	DisplayName string `json:"display_name"`
}

type prepareTOTPResponse struct {
	FactorID        string    `json:"factor_id"`
	Secret          string    `json:"secret"`
	ProvisioningURI string    `json:"provisioning_uri"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type confirmTOTPRequest struct {
	FactorID string `json:"factor_id"`
	Code     string `json:"code"`
}

type confirmTOTPResponse struct {
	FactorID      string    `json:"factor_id"`
	EnrolledAt    time.Time `json:"enrolled_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Version       uint64    `json:"version"`
	RecoveryCodes []string  `json:"recovery_codes"`
}

type disableTOTPRequest struct {
	FactorID string `json:"factor_id"`
	Version  uint64 `json:"version"`
}

type disabledTOTPResponse struct {
	FactorID   string    `json:"factor_id"`
	Status     string    `json:"status"`
	DisabledAt time.Time `json:"disabled_at"`
	Version    uint64    `json:"version"`
}

type createChallengeRequest struct {
	FactorID string `json:"factor_id"`
}

type createChallengeResponse struct {
	ChallengeID string    `json:"challenge_id"`
	Challenge   string    `json:"challenge"`
	ExpiresAt   time.Time `json:"expires_at"`
	MaxAttempts uint16    `json:"max_attempts"`
}

type verifyChallengeRequest struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

type verifyChallengeResponse struct {
	ChallengeID        string     `json:"challenge_id"`
	Verified           bool       `json:"verified"`
	VerificationMethod string     `json:"verification_method,omitempty"`
	AttemptsRemaining  uint16     `json:"attempts_remaining"`
	VerifiedAt         *time.Time `json:"verified_at,omitempty"`
	Status             string     `json:"status"`
}

// PrepareTOTP creates a pending enrollment for the current authenticated account. Its secret and
// provisioning URI are intentionally returned only here and are never logged by this adapter.
func (handler *Handler) PrepareTOTP(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload prepareTOTPRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.PrepareTOTP(request.Context(), application.PrepareTOTPInput{TenantID: principal.Tenant.ID, AccountID: principal.Account.ID, AccountLabel: accountLabel(principal), DisplayName: payload.DisplayName})
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "TOTP 注册准备成功", prepareTOTPResponse{FactorID: result.FactorID, Secret: result.Secret, ProvisioningURI: result.ProvisioningURI, ExpiresAt: result.ExpiresAt})
}

// ConfirmTOTP validates the authenticator code and returns recovery codes exactly once.
func (handler *Handler) ConfirmTOTP(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload confirmTOTPRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.ConfirmTOTP(request.Context(), application.ConfirmTOTPInput{TenantID: principal.Tenant.ID, AccountID: principal.Account.ID, FactorID: payload.FactorID, Code: payload.Code})
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "TOTP 注册确认成功", confirmTOTPResponse{FactorID: result.FactorID, EnrolledAt: result.EnrolledAt, ExpiresAt: result.ExpiresAt, Version: result.Version, RecoveryCodes: result.RecoveryCodes})
}

// DisableTOTP disables one current-account factor using the submitted optimistic-lock version.
func (handler *Handler) DisableTOTP(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload disableTOTPRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.DisableTOTP(request.Context(), application.DisableTOTPInput{TenantID: principal.Tenant.ID, AccountID: principal.Account.ID, FactorID: payload.FactorID, ExpectedVersion: payload.Version})
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	disabledAt := time.Time{}
	if result.DisabledAt != nil {
		disabledAt = *result.DisabledAt
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "TOTP 已停用", disabledTOTPResponse{FactorID: result.ID, Status: result.Status, DisabledAt: disabledAt, Version: result.Version})
}

// CreateChallenge creates a challenge for the current authenticated account. A primary-login flow
// can call the application service directly with its server-verified account identity instead.
func (handler *Handler) CreateChallenge(writer http.ResponseWriter, request *http.Request) {
	principal, ok := handler.principal(writer, request)
	if !ok {
		return
	}
	var payload createChallengeRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.CreateChallenge(request.Context(), application.CreateChallengeInput{TenantID: principal.Tenant.ID, AccountID: principal.Account.ID, FactorID: payload.FactorID})
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "MFA 挑战创建成功", createChallengeResponse{ChallengeID: result.ChallengeID, Challenge: result.Challenge, ExpiresAt: result.ExpiresAt, MaxAttempts: result.MaxAttempts})
}

// VerifyChallenge verifies a challenge token without requiring a browser session. The opaque,
// high-entropy challenge token is the only lookup material; tenant and account IDs are never
// accepted from this unauthenticated request.
func (handler *Handler) VerifyChallenge(writer http.ResponseWriter, request *http.Request) {
	var payload verifyChallengeRequest
	if !decodeJSON(writer, request, &payload) {
		return
	}
	result, err := handler.service.VerifyChallenge(request.Context(), application.VerifyChallengeInput{Challenge: payload.Challenge, Code: payload.Code})
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	if !result.Verified {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_MFA_VERIFICATION_FAILED", "动态验证码或恢复码错误", map[string]any{"attempts_remaining": result.AttemptsRemaining}))
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "MFA 挑战验证成功", verifyChallengeResponse{ChallengeID: result.ChallengeID, Verified: result.Verified, VerificationMethod: result.VerificationMethod, AttemptsRemaining: result.AttemptsRemaining, VerifiedAt: result.VerifiedAt, Status: result.Status})
}

func (handler *Handler) principal(writer http.ResponseWriter, request *http.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.Account.ID) == "" {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
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

func (handler *Handler) writeApplicationError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidInput):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, domain.ErrFactorNotFound), errors.Is(err, domain.ErrChallengeNotFound):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
	case errors.Is(err, domain.ErrVersionConflict):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.VersionConflict)
	case errors.Is(err, domain.ErrEnrollmentExpired), errors.Is(err, domain.ErrChallengeExpired):
		httpresponse.WriteError(writer, request, http.StatusGone, httperror.New("AUTH_MFA_CHALLENGE_EXPIRED", "MFA 注册或挑战已过期", nil))
	case errors.Is(err, domain.ErrChallengeConsumed):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.New("AUTH_MFA_CHALLENGE_CONSUMED", "MFA 挑战已使用", nil))
	case errors.Is(err, domain.ErrChallengeAttemptsExceeded):
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_MFA_ATTEMPTS_EXCEEDED", "MFA 挑战尝试次数已用尽", nil))
	case errors.Is(err, domain.ErrInvalidVerificationCode), errors.Is(err, domain.ErrFactorUnavailable):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.Conflict)
	default:
		handler.logger.Error("MFA HTTP request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func accountLabel(principal authctx.Principal) string {
	if value := strings.TrimSpace(principal.Account.Name); value != "" {
		return value
	}
	return principal.Account.ID
}
