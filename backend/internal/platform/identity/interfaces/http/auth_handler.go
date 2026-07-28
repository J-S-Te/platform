// Package identityhttp adapts identity authentication use cases to the public HTTP API.
package identityhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	applicationregistry "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	auditapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/ulid"
)

const maxLoginRequestBytes = 8 * 1024

// Handler exposes the OpenAPI /auth endpoints.
type Handler struct {
	service       applicationService
	logger        *slog.Logger
	cookie        cookieConfig
	auditRecorder lifecycleAuditRecorder
	auditConfig   config.AuditConfig
	loginTargets  applicationregistry.LoginTargetResolver
}

type applicationService interface {
	Login(ctx context.Context, input application.LoginInput) (application.SessionResult, error)
	Authenticate(ctx context.Context, token string) (authctx.Principal, error)
	RecordInteraction(ctx context.Context, principal authctx.Principal) error
	Refresh(ctx context.Context, principal authctx.Principal) (application.SessionResult, error)
	Logout(ctx context.Context, principal authctx.Principal) error
	SessionIdleTimeout(ctx context.Context, tenantID string) (time.Duration, error)
}

// lifecycleAuditRecorder persists server-generated identity lifecycle events. Recording is best
// effort because a completed authentication response must not be turned into a failure if audit
// persistence is temporarily unavailable.
type lifecycleAuditRecorder interface {
	Ingest(ctx context.Context, tenantID string, input auditapplication.EventInput) (auditdomain.Receipt, error)
}

// cookieConfig keeps only cookie properties that must remain consistent when setting or clearing.
type cookieConfig struct {
	name     string
	secure   bool
	sameSite http.SameSite
}

// NewHandler creates the identity HTTP adapter. The caller validates the service during bootstrap.
func NewHandler(service applicationService, logger *slog.Logger, authConfig config.AuthConfig, auditRecorder lifecycleAuditRecorder, auditConfig config.AuditConfig, loginTargetResolvers ...applicationregistry.LoginTargetResolver) (*Handler, error) {
	if service == nil || logger == nil || auditRecorder == nil {
		return nil, errors.New("identity HTTP handler dependencies must not be nil")
	}
	if len(loginTargetResolvers) > 1 || (len(loginTargetResolvers) == 1 && loginTargetResolvers[0] == nil) {
		return nil, errors.New("identity HTTP handler accepts at most one non-nil login target resolver")
	}
	if strings.TrimSpace(auditConfig.ApplicationCode) == "" || strings.TrimSpace(auditConfig.EnvironmentCode) == "" {
		return nil, errors.New("identity lifecycle audit configuration must not be empty")
	}
	cookieName := strings.TrimSpace(authConfig.SessionCookieName)
	if cookieName == "" {
		return nil, errors.New("identity session cookie name must not be blank")
	}
	sameSite, err := parseSameSite(authConfig.SessionCookieSameSite)
	if err != nil {
		return nil, err
	}
	if sameSite == http.SameSiteNoneMode && !authConfig.SessionCookieSecure {
		return nil, errors.New("SameSite=None identity session cookie must be secure")
	}
	handler := &Handler{
		service:       service,
		logger:        logger,
		cookie:        cookieConfig{name: cookieName, secure: authConfig.SessionCookieSecure, sameSite: sameSite},
		auditRecorder: auditRecorder,
		auditConfig:   auditConfig,
	}
	if len(loginTargetResolvers) == 1 {
		handler.loginTargets = loginTargetResolvers[0]
	}
	return handler, nil
}

type loginRequest struct {
	Account   string `json:"account"`
	Password  string `json:"password"`
	LoginType string `json:"login_type"`

	ApplicationID          string `json:"application_id"`
	EnvironmentID          string `json:"environment_id"`
	LoginTargetCode        string `json:"login_target_code"`
	ReplaceExistingSession bool   `json:"replace_existing_session"`
}

type sessionResponse struct {
	ExpiresAt   time.Time `json:"expires_at"`
	RedirectURL string    `json:"redirect_url"`
}

type principalResponse struct {
	authctx.Principal
	IdleTimeoutSeconds uint `json:"idle_timeout_seconds"`
}

// Login handles the sole P0 login type: a local account with an Argon2id password credential.
func (handler *Handler) Login(writer http.ResponseWriter, request *http.Request) {
	payload, err := decodeLoginRequest(writer, request)
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnprocessableEntity, httperror.Validation)
		return
	}

	result, err := handler.service.Login(request.Context(), application.LoginInput{
		Account: payload.Account, Password: payload.Password, IPAddress: remoteIP(request), UserAgent: request.UserAgent(),
		ReplaceExistingSession: payload.ReplaceExistingSession,
	})
	if err != nil {
		handler.recordLoginFailure(request, err)
		if errors.Is(err, application.ErrUnauthenticated) {
			httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_UNAUTHENTICATED", "账号或密码错误", nil))
			return
		}
		handler.writeApplicationError(writer, request, err)
		return
	}
	result.RedirectURL = handler.resolveLoginRedirect(request.Context(), result.TenantID, payload)
	handler.setSessionCookie(writer, result.Token, result.ExpiresAt)
	handler.recordLogin(request, result)
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "登录成功", toSessionResponse(result))
}

// resolveLoginRedirect converts a complete target tuple into a pre-registered HTTPS address. The
// browser never supplies a URI. Missing, inactive or temporarily unavailable targets fail closed
// to the platform root without exposing stored addresses in application logs.
func (handler *Handler) resolveLoginRedirect(ctx context.Context, tenantID string, payload loginRequest) string {
	if payload.ApplicationID == "" {
		return "/"
	}
	if handler.loginTargets == nil {
		handler.logger.Warn("login target resolver is not configured", "application_id", payload.ApplicationID, "environment_id", payload.EnvironmentID, "target_code", payload.LoginTargetCode)
		return "/"
	}

	redirectURI, err := handler.loginTargets.ResolveActiveTargetURI(ctx, applicationregistry.LoginTargetResolveInput{
		TenantID: tenantID, ApplicationID: payload.ApplicationID,
		EnvironmentID: payload.EnvironmentID, TargetCode: payload.LoginTargetCode,
	})
	if err != nil {
		handler.logger.Warn("login target resolution failed", "error", err, "tenant_id", tenantID, "application_id", payload.ApplicationID, "environment_id", payload.EnvironmentID, "target_code", payload.LoginTargetCode)
		return "/"
	}
	return redirectURI
}

// Activity records an interaction explicitly reported by the browser. It is intentionally a
// separate endpoint so regular authenticated API traffic cannot extend the idle timeout.
func (handler *Handler) Activity(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	if err := handler.service.RecordInteraction(request.Context(), principal); err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	idleTimeout, err := handler.service.SessionIdleTimeout(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "用户活动已记录", principalResponse{Principal: principal, IdleTimeoutSeconds: uint(idleTimeout / time.Second)})
}

// Refresh renews the already authenticated current browser session and replaces its cookie.
func (handler *Handler) Refresh(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	result, err := handler.service.Refresh(request.Context(), principal)
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	handler.setSessionCookie(writer, result.Token, result.ExpiresAt)
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "会话已刷新", toSessionResponse(result))
}

// Logout revokes the already authenticated session and always writes a matching expired cookie on
// a successful server-side revocation.
func (handler *Handler) Logout(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	if err := handler.service.Logout(request.Context(), principal); err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	handler.clearSessionCookie(writer)
	handler.recordLogout(request, principal)
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "已退出所有应用系统", map[string]any{})
}

// Me returns only the principal that middleware obtained by verifying the JWT and persisted state.
func (handler *Handler) Me(writer http.ResponseWriter, request *http.Request) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
		return
	}
	idleTimeout, err := handler.service.SessionIdleTimeout(request.Context(), principal.Tenant.ID)
	if err != nil {
		handler.writeApplicationError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, http.StatusOK, "操作成功", principalResponse{Principal: principal, IdleTimeoutSeconds: uint(idleTimeout / time.Second)})
}

// Authenticate delegates token verification to the application service for HTTP middleware.
func (handler *Handler) Authenticate(ctx context.Context, token string) (authctx.Principal, error) {
	return handler.service.Authenticate(ctx, token)
}

// CookieName exposes the configured cookie name to the HTTP authentication middleware.
func (handler *Handler) CookieName() string {
	return handler.cookie.name
}

// recordLogin writes a successful password-login event after the session has been persisted.
func (handler *Handler) recordLogin(request *http.Request, result application.SessionResult) {
	action := "auth.login"
	riskLevel := "LOW"
	summary := "本地账号密码登录成功"
	if result.ReplacedExistingSession {
		action = "auth.login.session_replaced"
		riskLevel = "MEDIUM"
		summary = "用户重新验证口令并退出原会话后登录"
	}
	handler.recordLifecycleEvent(request, result.TenantID, auditapplication.EventInput{
		ActorType: "USER", ActorID: result.UserID, ActorName: result.UserName, SessionID: result.SessionID,
		Action: action, ResourceType: "auth_session", ResourceID: result.SessionID, ResourceName: result.AccountName,
		Result: "SUCCESS", RiskLevel: riskLevel, Classification: "INTERNAL", Summary: summary,
	})
}

// recordLoginFailure records only trusted account metadata when the authentication service found
// an existing account. Unknown-account failures intentionally remain unlinked to prevent account
// enumeration and avoid creating misleading identity records.
func (handler *Handler) recordLoginFailure(request *http.Request, err error) {
	var concurrent application.ConcurrentSessionError
	if errors.As(err, &concurrent) {
		handler.recordLifecycleEvent(request, concurrent.TenantID, auditapplication.EventInput{
			ActorType: "USER", ActorID: concurrent.UserID, ActorName: concurrent.UserName,
			Action: "auth.login.concurrent_denied", ResourceType: "auth_account", ResourceID: concurrent.AccountID, ResourceName: concurrent.AccountName,
			Result: "FAILURE", RiskLevel: "MEDIUM", Classification: "INTERNAL", Summary: "账号已有有效会话，拒绝其他终端并发登录",
		})
		return
	}
	var failed application.LoginFailedError
	if errors.As(err, &failed) {
		handler.recordLifecycleEvent(request, failed.TenantID, auditapplication.EventInput{
			ActorType: "USER", ActorID: failed.UserID, ActorName: failed.UserName,
			Action: "auth.login.failed", ResourceType: "auth_account", ResourceID: failed.AccountID, ResourceName: failed.AccountName,
			Result: "FAILURE", RiskLevel: "MEDIUM", Classification: "INTERNAL", Summary: "本地账号密码登录失败",
		})
		return
	}
	var locked application.AccountLockedError
	if errors.As(err, &locked) {
		handler.recordLifecycleEvent(request, locked.TenantID, auditapplication.EventInput{
			ActorType: "USER", ActorID: locked.UserID, ActorName: locked.UserName,
			Action: "auth.login.locked", ResourceType: "auth_account", ResourceID: locked.AccountID, ResourceName: locked.AccountName,
			Result: "FAILURE", RiskLevel: "HIGH", Classification: "INTERNAL", Summary: "账号处于锁定状态，拒绝密码登录",
		})
	}
}

// recordLogout writes a successful logout event after the current server-side session is revoked.
func (handler *Handler) recordLogout(request *http.Request, principal authctx.Principal) {
	handler.recordLifecycleEvent(request, principal.Tenant.ID, auditapplication.EventInput{
		ActorType: "USER", ActorID: principal.User.ID, ActorName: principal.User.Name, SessionID: principal.SessionID,
		Action: "auth.logout", ResourceType: "auth_session", ResourceID: principal.SessionID, ResourceName: principal.Account.Name,
		Result: "SUCCESS", RiskLevel: "LOW", Classification: "INTERNAL",
		Summary: "已退出所有应用系统",
	})
}

// recordLifecycleEvent enriches a server-generated audit event with trusted HTTP context. It never
// records passwords, cookies, authorization headers, request IDs, or trace IDs.
func (handler *Handler) recordLifecycleEvent(request *http.Request, tenantID string, input auditapplication.EventInput) {
	if strings.TrimSpace(tenantID) == "" {
		handler.logger.Error("identity lifecycle audit skipped because tenant is missing", "action", input.Action)
		return
	}
	eventID, err := ulid.New(time.Now().UTC())
	if err != nil {
		handler.logger.Error("generate identity lifecycle audit event ID", "error", err, "action", input.Action)
		return
	}

	input.EventID = eventID
	input.ApplicationCode = handler.auditConfig.ApplicationCode
	input.EnvironmentCode = handler.auditConfig.EnvironmentCode
	input.OccurredAt = time.Now().UTC()
	input.SourceIP = remoteIP(request).String()
	input.UserAgent = request.UserAgent()
	input.EventCategory = "SECURITY"
	input.EventType = input.Action
	input.Metadata = map[string]any{"method": request.Method, "path": request.URL.Path}
	if _, err := handler.auditRecorder.Ingest(request.Context(), tenantID, input); err != nil {
		handler.logger.Error("record identity lifecycle audit event", "error", err, "action", input.Action, "tenant_id", tenantID)
	}
}

func (handler *Handler) setSessionCookie(writer http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(writer, &http.Cookie{
		Name: handler.cookie.name, Value: token, Path: "/", HttpOnly: true,
		Secure: handler.cookie.secure, SameSite: handler.cookie.sameSite, Expires: expiresAt.UTC(),
	})
}

// ClearSessionCookie removes the browser session cookie after a security-sensitive state change.
func (handler *Handler) ClearSessionCookie(writer http.ResponseWriter) {
	handler.clearSessionCookie(writer)
}

func (handler *Handler) clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: handler.cookie.name, Value: "", Path: "/", HttpOnly: true,
		Secure: handler.cookie.secure, SameSite: handler.cookie.sameSite,
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1,
	})
}

func (handler *Handler) writeApplicationError(writer http.ResponseWriter, request *http.Request, err error) {
	var locked application.AccountLockedError
	var concurrent application.ConcurrentSessionError
	switch {
	case errors.As(err, &locked):
		apiError := httperror.AccountLocked
		apiError.Details = map[string]time.Time{"locked_until": locked.LockedUntil.UTC()}
		httpresponse.WriteError(writer, request, http.StatusLocked, apiError)
	case errors.As(err, &concurrent), errors.Is(err, application.ErrConcurrentSession):
		httpresponse.WriteError(writer, request, http.StatusConflict, httperror.ConcurrentSession)
	case errors.Is(err, application.ErrUnauthenticated):
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
	default:
		handler.logger.Error("identity authentication request failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
	}
}

func decodeLoginRequest(writer http.ResponseWriter, request *http.Request) (loginRequest, error) {
	defer request.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxLoginRequestBytes))
	decoder.DisallowUnknownFields()

	var payload loginRequest
	if err := decoder.Decode(&payload); err != nil {
		return loginRequest{}, err
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return loginRequest{}, err
	}
	payload.Account = strings.TrimSpace(payload.Account)
	payload.ApplicationID = strings.TrimSpace(payload.ApplicationID)
	payload.EnvironmentID = strings.TrimSpace(payload.EnvironmentID)
	payload.LoginTargetCode = strings.TrimSpace(payload.LoginTargetCode)
	if payload.LoginType != "password" || payload.Account == "" || len(payload.Account) > 128 ||
		payload.Password == "" || len(payload.Password) > 256 || !validLoginTargetSelection(payload) {
		return loginRequest{}, errors.New("login request does not meet contract")
	}
	return payload, nil
}

func validLoginTargetSelection(payload loginRequest) bool {
	provided := 0
	for _, value := range []string{payload.ApplicationID, payload.EnvironmentID, payload.LoginTargetCode} {
		if value != "" {
			provided++
		}
	}
	if provided == 0 {
		return true
	}
	return provided == 3 && len(payload.ApplicationID) == 26 && len(payload.EnvironmentID) == 26 &&
		len(payload.LoginTargetCode) <= 64
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request has multiple JSON values")
		}
		return err
	}
	return nil
}

func parseSameSite(value string) (http.SameSite, error) {
	switch strings.ToLower(value) {
	case "lax":
		return http.SameSiteLaxMode, nil
	case "strict":
		return http.SameSiteStrictMode, nil
	case "none":
		return http.SameSiteNoneMode, nil
	default:
		return http.SameSiteDefaultMode, fmt.Errorf("unsupported session cookie SameSite value %q", value)
	}
}

func toSessionResponse(result application.SessionResult) sessionResponse {
	return sessionResponse{ExpiresAt: result.ExpiresAt.UTC(), RedirectURL: result.RedirectURL}
}

func remoteIP(request *http.Request) net.IP {
	remoteAddress := requestctx.ClientIP(request.Context())
	if remoteAddress == "" {
		remoteAddress = request.RemoteAddr
	}
	host, _, err := net.SplitHostPort(remoteAddress)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(remoteAddress)
}
