// Package loginhttp adapts external OIDC browser login use cases to net/http handlers.
package loginhttp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
)

const (
	maxCallbackQueryBytes      = 16 << 10
	browserBindingEntropyBytes = 32
)

// CallbackLifecycle contains only trusted local identifiers that may be used for lifecycle audit.
// It deliberately excludes authorization codes, state, ID/access tokens and the upstream subject.
//
// The external-login application adapter must return the tenant and provider context whenever it is
// safely known (for example, after consuming an encrypted server-side state record). User, account
// and binding identifiers are populated only after a verified subject has resolved to an active local
// binding.
type CallbackLifecycle struct {
	TenantID     string
	ProviderCode string
	ProviderID   string
	BindingID    string
	UserID       string
	AccountID    string
}

// ApplicationService is the browser-safe subset used by the external-login HTTP adapter.
//
// CompleteCallbackWithLifecycle is intentionally an adapter-level contract rather than a domain
// object. It permits the HTTP boundary to produce accountable lifecycle events without receiving an
// upstream subject or any protocol secret. A zero CallbackLifecycle is valid only when the callback
// cannot be safely associated with a tenant, such as an unknown or malformed state.
type ApplicationService interface {
	Begin(context.Context, application.BeginInput) (application.BeginResult, error)
	CompleteCallbackWithLifecycle(context.Context, application.CallbackInput) (application.CallbackResult, CallbackLifecycle, error)
}

// CallbackCompletion is the HTTP-safe callback outcome.
type CallbackCompletion struct {
	Session    domain.BrowserSession
	RedirectTo string
}

// lifecycleAuditRecorder persists server-generated identity lifecycle events. Recording is best
// effort: a completed login must not turn into a browser-visible failure merely because audit storage
// is temporarily unavailable.
type lifecycleAuditRecorder interface {
	Ingest(context.Context, string, auditapplication.EventInput) (auditdomain.Receipt, error)
}

// CookieConfig controls the platform session cookie and external-login browser-binding cookie.
type CookieConfig struct {
	Name     string
	Path     string
	Domain   string
	Secure   bool
	SameSite http.SameSite
}

// Handler exposes two routes that the main router may mount, for example:
//
//	GET /auth/external/login?tenant_id=...&provider_code=...&return_to=/
//	GET /auth/external/callback?state=...&code=...
//
// The callback intentionally emits only a redirect and an HttpOnly session cookie; no token,
// upstream subject or authorization code is written to the browser response body.
type Handler struct {
	service       ApplicationService
	cookie        CookieConfig
	logger        *slog.Logger
	auditRecorder lifecycleAuditRecorder
	auditConfig   config.AuditConfig
}

// NewHandler validates the externally supplied browser-cookie and lifecycle-audit policy.
func NewHandler(service ApplicationService, cookie CookieConfig, logger *slog.Logger, auditRecorder lifecycleAuditRecorder, auditConfig config.AuditConfig) (*Handler, error) {
	if service == nil || logger == nil || auditRecorder == nil {
		return nil, errors.New("external login HTTP handler dependencies must not be nil")
	}
	if strings.TrimSpace(cookie.Name) == "" {
		return nil, errors.New("external login session cookie name must not be blank")
	}
	if cookie.Path == "" {
		cookie.Path = "/"
	}
	if !strings.HasPrefix(cookie.Path, "/") {
		return nil, errors.New("external login cookie path must be absolute")
	}
	if !supportedSameSite(cookie.SameSite) {
		return nil, errors.New("external login session cookie must set a supported SameSite policy")
	}
	if cookie.SameSite == http.SameSiteNoneMode && !cookie.Secure {
		return nil, errors.New("SameSite=None external login cookie must be secure")
	}
	if strings.TrimSpace(auditConfig.ApplicationCode) == "" || strings.TrimSpace(auditConfig.EnvironmentCode) == "" {
		return nil, errors.New("external login lifecycle audit configuration must not be empty")
	}
	return &Handler{
		service:       service,
		cookie:        cookie,
		logger:        logger,
		auditRecorder: auditRecorder,
		auditConfig:   auditConfig,
	}, nil
}

// Start starts an authorization-code + PKCE browser redirect.
func (handler *Handler) Start(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeFailure(writer, http.StatusMethodNotAllowed)
		return
	}

	browserBinding, err := newBrowserBinding()
	if err != nil {
		writeApplicationFailure(writer, application.ErrProviderUnavailable)
		return
	}
	input := application.BeginInput{
		TenantID: request.URL.Query().Get("tenant_id"), ProviderCode: request.URL.Query().Get("provider_code"),
		ReturnTo: request.URL.Query().Get("return_to"), BrowserBinding: browserBinding,
	}
	result, err := handler.service.Begin(request.Context(), input)
	if err != nil {
		writeApplicationFailure(writer, err)
		return
	}

	// A successful Begin call has validated the tenant-scoped provider. The raw authorization URL is
	// intentionally not used by the audit event because it contains the opaque state value.
	handler.recordLoginStart(request, input.TenantID, input.ProviderCode)
	handler.setBrowserBindingCookie(writer, browserBinding, result.ExpiresAt)
	writeNoStoreHeaders(writer)
	http.Redirect(writer, request, result.AuthorizationURL, http.StatusFound)
}

// Callback validates the upstream callback and writes a local browser session cookie.
func (handler *Handler) Callback(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeFailure(writer, http.StatusMethodNotAllowed)
		return
	}
	if request.URL == nil || len(request.URL.RawQuery) > maxCallbackQueryBytes {
		writeFailure(writer, http.StatusBadRequest)
		return
	}
	browserBinding := handler.browserBindingFromRequest(request)
	handler.clearBrowserBindingCookie(writer)

	input := application.CallbackInput{
		State: request.URL.Query().Get("state"), AuthorizationCode: request.URL.Query().Get("code"), ProviderError: request.URL.Query().Get("error"),
		BrowserBinding: browserBinding, IPAddress: remoteIP(request.RemoteAddr), UserAgent: request.UserAgent(),
	}
	result, lifecycle, err := handler.completeCallback(request.Context(), input)
	if err != nil {
		handler.recordCallbackFailure(request, lifecycle, err)
		writeApplicationFailure(writer, err)
		return
	}
	if !validCallbackCompletion(result, time.Now().UTC()) {
		handler.recordCallbackFailure(request, lifecycle, application.ErrSessionIssue)
		writeApplicationFailure(writer, application.ErrSessionIssue)
		return
	}

	// Audit after the application has completed cryptographic validation and local account binding,
	// but before the response is committed. Audit persistence remains best effort.
	handler.recordCallbackSuccess(request, lifecycle)
	handler.recordBindingSuccess(request, lifecycle)

	handler.setSessionCookie(writer, result.Session)
	writeNoStoreHeaders(writer)
	http.Redirect(writer, request, result.RedirectTo, http.StatusFound)
}

func (handler *Handler) recordLoginStart(request *http.Request, tenantID, providerCode string) {
	tenantID = strings.TrimSpace(tenantID)
	providerCode = strings.TrimSpace(providerCode)
	if tenantID == "" || providerCode == "" {
		return
	}
	handler.recordLifecycleEvent(request, tenantID, auditapplication.EventInput{
		ActorType:      "SYSTEM",
		Action:         "auth.external_login.start",
		ResourceType:   "external_identity_provider",
		ResourceID:     providerCode,
		Result:         "SUCCESS",
		RiskLevel:      "LOW",
		Classification: "INTERNAL",
		Summary:        "外部身份登录授权跳转已开始",
	})
}

func (handler *Handler) recordCallbackSuccess(request *http.Request, lifecycle CallbackLifecycle) {
	if !lifecycle.hasTenantAndProvider() {
		handler.recordMissingLifecycleContext("auth.external_login.callback")
		return
	}
	handler.recordLifecycleEvent(request, lifecycle.TenantID, auditapplication.EventInput{
		ActorType:      "SYSTEM",
		Action:         "auth.external_login.callback",
		ResourceType:   "external_identity_provider",
		ResourceID:     lifecycle.providerResourceID(),
		Result:         "SUCCESS",
		RiskLevel:      "MEDIUM",
		Classification: "INTERNAL",
		Summary:        "外部身份登录回调验证成功",
	})
}

func (handler *Handler) recordBindingSuccess(request *http.Request, lifecycle CallbackLifecycle) {
	if !lifecycle.hasResolvedBinding() {
		handler.recordMissingLifecycleContext("auth.external_login.binding")
		return
	}
	handler.recordLifecycleEvent(request, lifecycle.TenantID, auditapplication.EventInput{
		ActorType:      "USER",
		ActorID:        lifecycle.UserID,
		Action:         "auth.external_login.binding",
		ResourceType:   "external_identity_binding",
		ResourceID:     lifecycle.BindingID,
		Result:         "SUCCESS",
		RiskLevel:      "MEDIUM",
		Classification: "INTERNAL",
		Summary:        "已验证的外部身份已关联本地账号并完成登录",
	})
}

func (handler *Handler) recordCallbackFailure(request *http.Request, lifecycle CallbackLifecycle, callbackErr error) {
	if !lifecycle.hasTenantAndProvider() {
		// Do not infer a tenant from state, code, provider error or other browser-controlled values.
		// A lifecycle-aware application adapter may supply safe state-derived context when available.
		return
	}

	result := "FAILURE"
	if errors.Is(callbackErr, application.ErrAccountNotBound) {
		result = "DENIED"
	}
	handler.recordLifecycleEvent(request, lifecycle.TenantID, auditapplication.EventInput{
		ActorType:      "SYSTEM",
		Action:         "auth.external_login.callback",
		ResourceType:   "external_identity_provider",
		ResourceID:     lifecycle.providerResourceID(),
		Result:         result,
		RiskLevel:      "HIGH",
		Classification: "INTERNAL",
		Summary:        "外部身份登录回调未完成",
	})
	if errors.Is(callbackErr, application.ErrAccountNotBound) {
		handler.recordLifecycleEvent(request, lifecycle.TenantID, auditapplication.EventInput{
			ActorType:      "SYSTEM",
			Action:         "auth.external_login.binding",
			ResourceType:   "external_identity_provider",
			ResourceID:     lifecycle.providerResourceID(),
			Result:         "DENIED",
			RiskLevel:      "MEDIUM",
			Classification: "INTERNAL",
			Summary:        "已验证的外部身份未关联可用本地账号",
		})
	}
}

// recordLifecycleEvent mirrors the local password handler's best-effort lifecycle-audit convention.
// It deliberately uses only method and URL path metadata; query values can contain authorization
// codes, state and provider error descriptions and therefore must never be recorded.
func (handler *Handler) recordLifecycleEvent(request *http.Request, tenantID string, input auditapplication.EventInput) {
	if strings.TrimSpace(tenantID) == "" {
		return
	}
	eventID, err := ulid.New(time.Now().UTC())
	if err != nil {
		handler.logger.Error("generate external login lifecycle audit event ID", "error", err, "action", input.Action)
		return
	}

	input.EventID = eventID
	input.ApplicationCode = handler.auditConfig.ApplicationCode
	input.EnvironmentCode = handler.auditConfig.EnvironmentCode
	input.OccurredAt = time.Now().UTC()
	input.SourceIP = remoteIP(request.RemoteAddr).String()
	input.UserAgent = request.UserAgent()
	input.EventCategory = "SECURITY"
	input.EventType = input.Action
	input.Metadata = map[string]any{"method": request.Method, "path": request.URL.Path}
	if _, err := handler.auditRecorder.Ingest(request.Context(), tenantID, input); err != nil {
		handler.logger.Error("record external login lifecycle audit event", "error", err, "action", input.Action)
	}
}

func (handler *Handler) recordMissingLifecycleContext(action string) {
	handler.logger.Error("external login lifecycle audit skipped because safe context is missing", "action", action)
}

func (lifecycle CallbackLifecycle) hasTenantAndProvider() bool {
	return strings.TrimSpace(lifecycle.TenantID) != "" && strings.TrimSpace(lifecycle.providerResourceID()) != ""
}

func (lifecycle CallbackLifecycle) hasResolvedBinding() bool {
	return lifecycle.hasTenantAndProvider() && strings.TrimSpace(lifecycle.BindingID) != "" &&
		strings.TrimSpace(lifecycle.UserID) != "" && strings.TrimSpace(lifecycle.AccountID) != ""
}

func (lifecycle CallbackLifecycle) providerResourceID() string {
	if providerID := strings.TrimSpace(lifecycle.ProviderID); providerID != "" {
		return providerID
	}
	return strings.TrimSpace(lifecycle.ProviderCode)
}

// completeCallback adapts the established application result to the HTTP completion model.
func (handler *Handler) completeCallback(ctx context.Context, input application.CallbackInput) (CallbackCompletion, CallbackLifecycle, error) {
	result, lifecycle, err := handler.service.CompleteCallbackWithLifecycle(ctx, input)
	if err != nil {
		return CallbackCompletion{}, lifecycle, err
	}
	return CallbackCompletion{Session: result.Session, RedirectTo: result.RedirectTo}, lifecycle, nil
}

func (handler *Handler) setSessionCookie(writer http.ResponseWriter, session domain.BrowserSession) {
	handler.setCookie(writer, handler.cookie.Name, session.CookieValue, session.ExpiresAt)
}

func (handler *Handler) setBrowserBindingCookie(writer http.ResponseWriter, credential string, expiresAt time.Time) {
	handler.setCookie(writer, browserBindingCookieName(handler.cookie.Name), credential, expiresAt)
}

func (handler *Handler) browserBindingFromRequest(request *http.Request) string {
	cookie, err := request.Cookie(browserBindingCookieName(handler.cookie.Name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

func (handler *Handler) clearBrowserBindingCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: browserBindingCookieName(handler.cookie.Name), Path: handler.cookie.Path, Domain: handler.cookie.Domain,
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1, HttpOnly: true, Secure: handler.cookie.Secure, SameSite: handler.cookie.SameSite,
	})
}

func (handler *Handler) setCookie(writer http.ResponseWriter, name, value string, expiresAt time.Time) {
	expiresAt = expiresAt.UTC()
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(writer, &http.Cookie{
		Name: name, Value: value, Path: handler.cookie.Path, Domain: handler.cookie.Domain,
		Expires: expiresAt, MaxAge: maxAge, HttpOnly: true, Secure: handler.cookie.Secure, SameSite: handler.cookie.SameSite,
	})
}

// browserBindingCookieName is independent from the authenticated session.
// Its value exists only to bind one external authorization response to the browser that initiated it.
func browserBindingCookieName(sessionCookieName string) string {
	return sessionCookieName + "_external_login"
}

func newBrowserBinding() (string, error) {
	buffer := make([]byte, browserBindingEntropyBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func validCallbackCompletion(result CallbackCompletion, now time.Time) bool {
	return strings.TrimSpace(result.RedirectTo) != "" &&
		strings.TrimSpace(result.Session.CookieValue) != "" && result.Session.ExpiresAt.After(now)
}

func supportedSameSite(value http.SameSite) bool {
	switch value {
	case http.SameSiteLaxMode, http.SameSiteStrictMode, http.SameSiteNoneMode:
		return true
	default:
		return false
	}
}

func writeApplicationFailure(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidRequest), errors.Is(err, application.ErrInvalidState), errors.Is(err, application.ErrAuthorizationDenied):
		writeFailure(writer, http.StatusBadRequest)
	case errors.Is(err, application.ErrTokenValidation):
		writeFailure(writer, http.StatusUnauthorized)
	case errors.Is(err, application.ErrAccountNotBound):
		writeFailure(writer, http.StatusForbidden)
	case errors.Is(err, application.ErrProviderUnavailable):
		writeFailure(writer, http.StatusServiceUnavailable)
	default:
		writeFailure(writer, http.StatusInternalServerError)
	}
}

// writeFailure intentionally has a generic body: protocol values and upstream errors must not be
// exposed to the browser, proxy logs, audit logs or observability collectors.
func writeFailure(writer http.ResponseWriter, status int) {
	writeNoStoreHeaders(writer)
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte("external login failed"))
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
