// Package dingtalkhttp adapts the dedicated DingTalk QR login flow to net/http handlers.
package dingtalkhttp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
)

const (
	maxCreateBodyBytes         = 16 << 10
	maxCallbackQueryBytes      = 16 << 10
	browserBindingBytes        = 32
	maxBrowserContextCookieLen = 2048
	browserContextPurpose      = "DINGTALK_QR_BROWSER_CONTEXT_V1"
)

var errBrowserContextExpired = errors.New("DingTalk browser context expired")

// applicationService is the browser-safe DingTalk use-case boundary.
type applicationService interface {
	CreateQRSession(context.Context, application.CreateQRSessionInput) (application.CreateQRSessionResult, error)
	CompleteCallback(context.Context, application.CallbackInput) (application.CallbackResult, error)
}

// lifecycleAuditRecorder persists best-effort security lifecycle events.
type lifecycleAuditRecorder interface {
	Ingest(context.Context, string, auditapplication.EventInput) (auditdomain.Receipt, error)
}

// browserContextProtector keeps the one-time browser binding and its audit attribution opaque to
// browser scripts and intermediaries. The configured protector must provide authenticated encryption.
type browserContextProtector interface {
	Encrypt(context.Context, []byte) ([]byte, error)
	Decrypt(context.Context, []byte) ([]byte, error)
}

type browserLoginContext struct {
	Purpose      string    `json:"purpose"`
	Binding      string    `json:"binding"`
	TenantID     string    `json:"tenant_id"`
	ProviderCode string    `json:"provider_code"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// CookieConfig controls the platform session cookie and its derived short-lived cookies.
type CookieConfig struct {
	Name     string
	Path     string
	Domain   string
	Secure   bool
	SameSite http.SameSite
}

// Handler exposes the anonymous QR-session creation and DingTalk callback endpoints.
type Handler struct {
	service          applicationService
	cookie           CookieConfig
	logger           *slog.Logger
	auditRecorder    lifecycleAuditRecorder
	auditConfig      config.AuditConfig
	contextProtector browserContextProtector
}

// NewHandler validates cookie, encryption, and audit policies before exposing the login flow.
func NewHandler(service applicationService, contextProtector browserContextProtector, cookie CookieConfig, logger *slog.Logger, auditRecorder lifecycleAuditRecorder, auditConfig config.AuditConfig) (*Handler, error) {
	if service == nil || contextProtector == nil || logger == nil || auditRecorder == nil {
		return nil, errors.New("DingTalk login HTTP handler dependencies must not be nil")
	}
	if strings.TrimSpace(cookie.Name) == "" {
		return nil, errors.New("DingTalk login session cookie name must not be blank")
	}
	if cookie.Path == "" {
		cookie.Path = "/"
	}
	if !strings.HasPrefix(cookie.Path, "/") {
		return nil, errors.New("DingTalk login cookie path must be absolute")
	}
	if !supportedSameSite(cookie.SameSite) {
		return nil, errors.New("DingTalk login cookie must set a supported SameSite policy")
	}
	if cookie.SameSite == http.SameSiteNoneMode && !cookie.Secure {
		return nil, errors.New("SameSite=None DingTalk login cookie must be secure")
	}
	if strings.TrimSpace(auditConfig.ApplicationCode) == "" || strings.TrimSpace(auditConfig.EnvironmentCode) == "" {
		return nil, errors.New("DingTalk login lifecycle audit configuration must not be empty")
	}
	return &Handler{service: service, contextProtector: contextProtector, cookie: cookie, logger: logger, auditRecorder: auditRecorder, auditConfig: auditConfig}, nil
}

type createQRSessionRequest struct {
	TenantID     string `json:"tenant_id"`
	ProviderCode string `json:"provider_code"`
	ReturnTo     string `json:"return_to"`
}

// CreateQRSession creates one MySQL-backed, browser-bound DingTalk authorization attempt.
func (handler *Handler) CreateQRSession(writer http.ResponseWriter, request *http.Request) {
	writeNoStoreHeaders(writer)
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		httpresponse.WriteError(writer, request, http.StatusMethodNotAllowed, httperror.MethodNotAllowed)
		return
	}

	var input createQRSessionRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxCreateBodyBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}
	browserBinding, err := newOpaqueCredential(browserBindingBytes)
	if err != nil {
		handler.logger.Error("generate DingTalk browser binding", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return
	}
	result, err := handler.service.CreateQRSession(request.Context(), application.CreateQRSessionInput{
		TenantID: input.TenantID, ProviderCode: input.ProviderCode, ReturnTo: input.ReturnTo, BrowserBinding: browserBinding,
	})
	if err != nil {
		handler.recordStart(request, input.TenantID, input.ProviderCode, "FAILURE")
		writeCreateFailure(writer, request, err)
		return
	}
	if !validQRSessionResult(result, time.Now().UTC()) {
		handler.logger.Error("DingTalk QR service returned invalid creation result")
		handler.recordStart(request, input.TenantID, input.ProviderCode, "FAILURE")
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return
	}

	if err := handler.setBrowserBindingCookie(request.Context(), writer, browserLoginContext{
		Purpose:      browserContextPurpose,
		Binding:      browserBinding,
		TenantID:     strings.TrimSpace(input.TenantID),
		ProviderCode: strings.TrimSpace(input.ProviderCode),
		ExpiresAt:    result.ExpiresAt.UTC(),
	}); err != nil {
		handler.logger.Error("protect DingTalk browser context", "error", err)
		handler.recordStart(request, input.TenantID, input.ProviderCode, "FAILURE")
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return
	}
	handler.recordStart(request, input.TenantID, input.ProviderCode, "SUCCESS")
	httpresponse.WriteSuccess(writer, request, http.StatusCreated, "钉钉扫码会话已创建", result)
}

// Callback consumes DingTalk's one-time authorization result. It completes only in the top-level
// browser: credentials remain HttpOnly cookies and the response is a same-origin relative redirect.
func (handler *Handler) Callback(writer http.ResponseWriter, request *http.Request) {
	writeNoStoreHeaders(writer)
	browserContext, browserContextErr := handler.browserContextFromRequest(request)
	auditLifecycle := lifecycleFromBrowserContext(browserContext)

	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		handler.recordEarlyCallbackFailure(request, auditLifecycle, "DINGTALK_CALLBACK_METHOD_NOT_ALLOWED", browserContextErr)
		handler.redirectToLoginError(writer, "PLATFORM_METHOD_NOT_ALLOWED")
		return
	}
	if request.URL == nil || len(request.URL.RawQuery) > maxCallbackQueryBytes {
		handler.recordEarlyCallbackFailure(request, auditLifecycle, "DINGTALK_CALLBACK_QUERY_INVALID", browserContextErr)
		handler.redirectToLoginError(writer, "AUTH_DINGTALK_CALLBACK_INVALID")
		return
	}

	query, err := parseCallbackQuery(request.URL.RawQuery)
	if err != nil {
		handler.recordEarlyCallbackFailure(request, auditLifecycle, "DINGTALK_CALLBACK_QUERY_INVALID", browserContextErr)
		handler.redirectToLoginError(writer, "AUTH_DINGTALK_CALLBACK_INVALID")
		return
	}
	authCode := strings.TrimSpace(query.Get("authCode"))
	legacyCode := strings.TrimSpace(query.Get("code"))
	if authCode != "" && legacyCode != "" && authCode != legacyCode {
		handler.recordEarlyCallbackFailure(request, auditLifecycle, "DINGTALK_CALLBACK_AUTH_CODE_CONFLICT", browserContextErr)
		handler.redirectToLoginError(writer, "AUTH_DINGTALK_CALLBACK_INVALID")
		return
	}
	if authCode == "" {
		authCode = legacyCode
	}

	browserBinding := ""
	if browserContextErr == nil {
		browserBinding = browserContext.Binding
	}
	handler.clearBrowserBindingCookie(writer)
	result, err := handler.service.CompleteCallback(request.Context(), application.CallbackInput{
		State: query.Get("state"), AuthorizationCode: authCode, ProviderError: query.Get("error"),
		BrowserBinding: browserBinding, IPAddress: remoteIP(request.RemoteAddr), UserAgent: request.UserAgent(),
	})
	result.Lifecycle = mergeCallbackLifecycle(result.Lifecycle, auditLifecycle)
	if err != nil {
		handler.recordCallback(request, result.Lifecycle, err, callbackReasonCode(err))
		handler.redirectToLoginError(writer, callbackFailureCode(err))
		return
	}
	if !validCallbackResult(result, time.Now().UTC()) {
		handler.logger.Error("DingTalk QR service returned invalid callback result")
		handler.recordCallback(request, result.Lifecycle, application.ErrSessionIssue, "DINGTALK_CALLBACK_SESSION_ISSUE")
		handler.redirectToLoginError(writer, "PLATFORM_INTERNAL_ERROR")
		return
	}

	if result.Session.MFARequired {
		handler.setCookie(writer, preAuthenticationCookieName(handler.cookie.Name), result.Session.PreAuthenticationCredential, result.Session.PreAuthenticationExpiresAt)
		handler.recordCallback(request, result.Lifecycle, nil, "")
		redirectSeeOther(writer, mfaLoginRedirect(result.RedirectTo))
		return
	}

	handler.setCookie(writer, handler.cookie.Name, result.Session.CookieValue, result.Session.ExpiresAt)
	handler.recordCallback(request, result.Lifecycle, nil, "")
	redirectSeeOther(writer, result.RedirectTo)
}

func writeCreateFailure(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidRequest):
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrProviderUnavailable):
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.New("AUTH_DINGTALK_PROVIDER_NOT_AVAILABLE", "钉钉扫码登录暂不可用", nil))
	default:
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func callbackFailureCode(err error) string {
	switch {
	case errors.Is(err, application.ErrInvalidRequest), errors.Is(err, application.ErrAuthorizationDenied):
		return "AUTH_DINGTALK_CALLBACK_INVALID"
	case errors.Is(err, application.ErrInvalidState):
		return "AUTH_DINGTALK_QR_SESSION_INVALID"
	case errors.Is(err, application.ErrProtocolValidation):
		return "AUTH_DINGTALK_IDENTITY_VERIFICATION_FAILED"
	case errors.Is(err, application.ErrAccountNotBound):
		return "AUTH_DINGTALK_EXTERNAL_IDENTITY_NOT_BOUND"
	case errors.Is(err, application.ErrProviderUnavailable):
		return "AUTH_DINGTALK_PROVIDER_DISABLED"
	default:
		return "PLATFORM_INTERNAL_ERROR"
	}
}

func validQRSessionResult(result application.CreateQRSessionResult, now time.Time) bool {
	config := result.SDKConfig
	redirectURI, err := url.ParseRequestURI(strings.TrimSpace(config.RedirectURI))
	if err != nil || redirectURI.Scheme != "https" || redirectURI.Host == "" || redirectURI.User != nil {
		return false
	}
	return strings.TrimSpace(result.SessionID) != "" && result.ExpiresAt.After(now) && result.RenderMode == "dingtalk_frame" &&
		strings.TrimSpace(config.ClientID) != "" && config.ResponseType == "code" && strings.TrimSpace(config.Scope) != "" && strings.TrimSpace(config.State) != ""
}

func validCallbackResult(result application.CallbackResult, now time.Time) bool {
	if strings.TrimSpace(result.SessionID) == "" || !validSameOriginRelativeURL(result.RedirectTo) {
		return false
	}
	if result.Session.MFARequired {
		return strings.TrimSpace(result.Session.CookieValue) == "" && result.Session.ExpiresAt.IsZero() &&
			strings.TrimSpace(result.Session.PreAuthenticationCredential) != "" && result.Session.PreAuthenticationExpiresAt.After(now) && result.Session.MFAMaxAttempts > 0
	}
	return strings.TrimSpace(result.Session.CookieValue) != "" && result.Session.ExpiresAt.After(now) &&
		strings.TrimSpace(result.Session.PreAuthenticationCredential) == "" && result.Session.PreAuthenticationExpiresAt.IsZero() && result.Session.MFAMaxAttempts == 0
}

// validSameOriginRelativeURL prevents the callback endpoint from becoming an open redirect.
// It validates both the raw and decoded forms so percent-encoded backslashes, control
// characters, and network-path references cannot bypass the same-origin boundary.
func validSameOriginRelativeURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.Contains(value, `\`) || containsControlCharacter(value) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Fragment != "" {
		return false
	}
	decodedPath, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || !strings.HasPrefix(decodedPath, "/") || strings.HasPrefix(decodedPath, "//") ||
		strings.Contains(decodedPath, `\`) || containsControlCharacter(decodedPath) {
		return false
	}
	decodedValue, err := url.QueryUnescape(value)
	return err == nil && !strings.Contains(decodedValue, `\`) && !containsControlCharacter(decodedValue)
}

func containsControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func (handler *Handler) redirectToLoginError(writer http.ResponseWriter, errorCode string) {
	loginURL := url.URL{Path: "/login"}
	query := loginURL.Query()
	query.Set("dingtalk_error", errorCode)
	loginURL.RawQuery = query.Encode()
	redirectSeeOther(writer, loginURL.String())
}

func parseCallbackQuery(rawQuery string) (url.Values, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, errors.New("DingTalk callback query is malformed")
	}
	for _, key := range []string{"state", "authCode", "code", "error"} {
		if len(values[key]) > 1 {
			return nil, errors.New("DingTalk callback security parameter is duplicated")
		}
	}
	return values, nil
}

func mfaLoginRedirect(returnTo string) string {
	loginURL := url.URL{Path: "/login"}
	query := loginURL.Query()
	query.Set("dingtalk_mfa", "1")
	query.Set("return_to", returnTo)
	loginURL.RawQuery = query.Encode()
	return loginURL.String()
}

func redirectSeeOther(writer http.ResponseWriter, location string) {
	writer.Header().Set("Location", location)
	writer.WriteHeader(http.StatusSeeOther)
}

func (handler *Handler) browserContextFromRequest(request *http.Request) (browserLoginContext, error) {
	var result browserLoginContext
	cookie, err := request.Cookie(browserBindingCookieName(handler.cookie.Name))
	if err != nil {
		return result, err
	}
	encoded := strings.TrimSpace(cookie.Value)
	if encoded == "" || len(encoded) > maxBrowserContextCookieLen {
		return result, errors.New("DingTalk browser context cookie is malformed")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return result, errors.New("DingTalk browser context cookie encoding is invalid")
	}
	plaintext, err := handler.contextProtector.Decrypt(request.Context(), ciphertext)
	if err != nil {
		return result, errors.New("DingTalk browser context cookie authentication failed")
	}
	if err := json.Unmarshal(plaintext, &result); err != nil {
		return browserLoginContext{}, errors.New("DingTalk browser context payload is invalid")
	}
	result.Purpose = strings.TrimSpace(result.Purpose)
	result.Binding = strings.TrimSpace(result.Binding)
	result.TenantID = strings.TrimSpace(result.TenantID)
	result.ProviderCode = strings.TrimSpace(result.ProviderCode)
	result.ExpiresAt = result.ExpiresAt.UTC()
	if result.Purpose != browserContextPurpose || result.Binding == "" || len(result.Binding) > 256 ||
		result.TenantID == "" || len(result.TenantID) > 128 || result.ProviderCode == "" || len(result.ProviderCode) > 128 || result.ExpiresAt.IsZero() {
		return browserLoginContext{}, errors.New("DingTalk browser context payload is invalid")
	}
	if !result.ExpiresAt.After(time.Now().UTC()) {
		return result, errBrowserContextExpired
	}
	return result, nil
}

func (handler *Handler) setBrowserBindingCookie(ctx context.Context, writer http.ResponseWriter, value browserLoginContext) error {
	value.Purpose = strings.TrimSpace(value.Purpose)
	value.Binding = strings.TrimSpace(value.Binding)
	value.TenantID = strings.TrimSpace(value.TenantID)
	value.ProviderCode = strings.TrimSpace(value.ProviderCode)
	value.ExpiresAt = value.ExpiresAt.UTC()
	if value.Purpose != browserContextPurpose || value.Binding == "" || len(value.Binding) > 256 ||
		value.TenantID == "" || len(value.TenantID) > 128 || value.ProviderCode == "" || len(value.ProviderCode) > 128 || !value.ExpiresAt.After(time.Now().UTC()) {
		return errors.New("DingTalk browser context is invalid")
	}
	plaintext, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal DingTalk browser context: %w", err)
	}
	ciphertext, err := handler.contextProtector.Encrypt(ctx, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt DingTalk browser context: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(ciphertext)
	if len(encoded) > maxBrowserContextCookieLen {
		return errors.New("encrypted DingTalk browser context exceeds cookie limit")
	}
	maxAge := int(time.Until(value.ExpiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(writer, &http.Cookie{
		Name: browserBindingCookieName(handler.cookie.Name), Value: encoded, Path: handler.cookie.Path, Domain: handler.cookie.Domain,
		Expires: value.ExpiresAt, MaxAge: maxAge, HttpOnly: true, Secure: handler.cookie.Secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (handler *Handler) clearBrowserBindingCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: browserBindingCookieName(handler.cookie.Name), Path: handler.cookie.Path, Domain: handler.cookie.Domain,
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1, HttpOnly: true, Secure: handler.cookie.Secure, SameSite: http.SameSiteLaxMode,
	})
}

func (handler *Handler) setCookie(writer http.ResponseWriter, name, value string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt.UTC()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(writer, &http.Cookie{
		Name: name, Value: value, Path: handler.cookie.Path, Domain: handler.cookie.Domain,
		Expires: expiresAt.UTC(), MaxAge: maxAge, HttpOnly: true, Secure: handler.cookie.Secure, SameSite: handler.cookie.SameSite,
	})
}

func (handler *Handler) recordStart(request *http.Request, tenantID, providerCode, result string) {
	tenantID, providerCode = strings.TrimSpace(tenantID), strings.TrimSpace(providerCode)
	if tenantID == "" || providerCode == "" {
		return
	}
	handler.record(request, tenantID, auditapplication.EventInput{
		ActorType: "SYSTEM", Action: "auth.dingtalk_qr_session.create", ResourceType: "external_identity_provider", ResourceID: providerCode,
		Result: result, RiskLevel: "LOW", Classification: "INTERNAL", Summary: "钉钉扫码登录会话创建",
	})
}

func (handler *Handler) recordEarlyCallbackFailure(request *http.Request, lifecycle application.CallbackLifecycle, reasonCode string, browserContextErr error) {
	if strings.TrimSpace(lifecycle.TenantID) == "" || strings.TrimSpace(lifecycle.ProviderCode) == "" {
		handler.logUnscopedCallbackFailure(request, reasonCode, browserContextErr)
		return
	}
	handler.recordCallback(request, lifecycle, application.ErrProtocolValidation, reasonCode)
}

func (handler *Handler) logUnscopedCallbackFailure(request *http.Request, reasonCode string, browserContextErr error) {
	handler.logger.Warn("reject unscoped DingTalk login callback",
		"reason_code", strings.TrimSpace(reasonCode),
		"browser_context_status", browserContextStatus(browserContextErr),
		"request_id", requestctx.RequestID(request.Context()),
		"trace_id", requestctx.TraceID(request.Context()),
		"correlation_id", requestctx.CorrelationID(request.Context()),
		"source_ip", sourceIP(request.RemoteAddr),
	)
}

func (handler *Handler) recordCallback(request *http.Request, lifecycle application.CallbackLifecycle, callbackErr error, reasonCode string) {
	if strings.TrimSpace(lifecycle.TenantID) == "" || strings.TrimSpace(lifecycle.ProviderCode) == "" {
		if callbackErr != nil {
			handler.logUnscopedCallbackFailure(request, reasonCode, nil)
		}
		return
	}
	result, summary := "SUCCESS", "钉钉扫码登录回调验证成功"
	if callbackErr != nil {
		result, summary = "FAILURE", "钉钉扫码登录回调未完成"
		if errors.Is(callbackErr, application.ErrAccountNotBound) || errors.Is(callbackErr, application.ErrAuthorizationDenied) {
			result = "DENIED"
		}
	}
	resourceID := lifecycle.ProviderID
	if strings.TrimSpace(resourceID) == "" {
		resourceID = lifecycle.ProviderCode
	}
	handler.record(request, lifecycle.TenantID, auditapplication.EventInput{
		ActorType: "SYSTEM", Action: "auth.dingtalk_qr_callback", ResourceType: "external_identity_provider", ResourceID: resourceID,
		Result: result, ReasonCode: strings.TrimSpace(reasonCode), RiskLevel: "HIGH", Classification: "INTERNAL", Summary: summary,
	})
	if callbackErr == nil && strings.TrimSpace(lifecycle.BindingID) != "" {
		handler.record(request, lifecycle.TenantID, auditapplication.EventInput{
			ActorType: "USER", ActorID: lifecycle.UserID, Action: "auth.dingtalk_qr_binding", ResourceType: "external_identity_binding", ResourceID: lifecycle.BindingID,
			Result: "SUCCESS", RiskLevel: "MEDIUM", Classification: "INTERNAL", Summary: "钉钉身份预绑定校验成功",
		})
	}
}

func lifecycleFromBrowserContext(value browserLoginContext) application.CallbackLifecycle {
	return application.CallbackLifecycle{TenantID: strings.TrimSpace(value.TenantID), ProviderCode: strings.TrimSpace(value.ProviderCode)}
}

func mergeCallbackLifecycle(primary, fallback application.CallbackLifecycle) application.CallbackLifecycle {
	if strings.TrimSpace(primary.TenantID) == "" {
		primary.TenantID = strings.TrimSpace(fallback.TenantID)
	}
	if strings.TrimSpace(primary.ProviderCode) == "" {
		primary.ProviderCode = strings.TrimSpace(fallback.ProviderCode)
	}
	return primary
}

func callbackReasonCode(err error) string {
	switch {
	case errors.Is(err, application.ErrInvalidRequest):
		return "DINGTALK_CALLBACK_INVALID_REQUEST"
	case errors.Is(err, application.ErrAuthorizationDenied):
		return "DINGTALK_CALLBACK_AUTHORIZATION_DENIED"
	case errors.Is(err, application.ErrInvalidState):
		return "DINGTALK_CALLBACK_STATE_INVALID"
	case errors.Is(err, application.ErrProtocolValidation):
		return "DINGTALK_CALLBACK_IDENTITY_VERIFICATION_FAILED"
	case errors.Is(err, application.ErrAccountNotBound):
		return "DINGTALK_CALLBACK_ACCOUNT_NOT_BOUND"
	case errors.Is(err, application.ErrProviderUnavailable):
		return "DINGTALK_CALLBACK_PROVIDER_UNAVAILABLE"
	default:
		return "DINGTALK_CALLBACK_INTERNAL_ERROR"
	}
}

func browserContextStatus(err error) string {
	switch {
	case err == nil:
		return "valid"
	case errors.Is(err, http.ErrNoCookie):
		return "missing"
	case errors.Is(err, errBrowserContextExpired):
		return "expired"
	default:
		return "invalid"
	}
}

func (handler *Handler) record(request *http.Request, tenantID string, input auditapplication.EventInput) {
	eventID, err := ulid.New(time.Now().UTC())
	if err != nil {
		handler.logger.Error("generate DingTalk login audit event ID", "error", err, "action", input.Action)
		return
	}
	input.EventID = eventID
	input.ApplicationCode = handler.auditConfig.ApplicationCode
	input.EnvironmentCode = handler.auditConfig.EnvironmentCode
	input.OccurredAt = time.Now().UTC()
	input.SourceIP = sourceIP(request.RemoteAddr)
	input.UserAgent = request.UserAgent()
	input.EventCategory = "SECURITY"
	input.EventType = input.Action
	if _, err := handler.auditRecorder.Ingest(request.Context(), tenantID, input); err != nil {
		handler.logger.Error("record DingTalk login lifecycle audit event", "error", err, "action", input.Action)
	}
}

func browserBindingCookieName(sessionCookieName string) string {
	return sessionCookieName + "_dingtalk_qr"
}
func preAuthenticationCookieName(sessionCookieName string) string { return sessionCookieName + "_mfa" }

func newOpaqueCredential(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func supportedSameSite(value http.SameSite) bool {
	switch value {
	case http.SameSiteLaxMode, http.SameSiteStrictMode, http.SameSiteNoneMode:
		return true
	default:
		return false
	}
}

func writeNoStoreHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Referrer-Policy", "no-referrer")
}

func remoteIP(remoteAddress string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddress))
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func sourceIP(remoteAddress string) string {
	ip := remoteIP(remoteAddress)
	if ip == nil {
		return ""
	}
	return ip.String()
}
