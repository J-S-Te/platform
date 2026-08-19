// Package identityhttp adapts identity authentication use cases to the public HTTP API.
package identityhttp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	applicationregistry "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditdomain "github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

const maxLoginRequestBytes = 8 * 1024

// Handler exposes the OpenAPI /auth endpoints.
type Handler struct {
	service         applicationService
	logger          *slog.Logger
	cookie          cookieConfig
	auditRecorder   lifecycleAuditRecorder
	auditConfig     config.AuditConfig
	loginTargets    applicationregistry.LoginTargetResolver
	oidc            oidcLoginConfig
	idTokenVerifier oidcIDTokenVerifier
}

type oidcLoginConfig struct {
	enabled       bool
	issuer        string
	clientID      string
	clientSecret  string
	redirectPath  string
	publicBaseURL string
	stateCookie   string
	nonceCookie   string
}

// oidcIDTokenVerifier 校验平台自身 OIDC 登录回调收到的 ID Token（签名/issuer/aud/exp/
// nonce/azp），并返回与 subject 一致的平台 identity_id。P1-2：补齐平台作为 RP 的
// ID Token 校验与 nonce 绑定。
type oidcIDTokenVerifier interface {
	VerifyIDToken(ctx context.Context, rawToken, expectedNonce, expectedClientID string) (identityID string, err error)
}

// IDTokenVerifierOption 允许装配层注入平台 OIDC 登录的 ID Token 验证器；未注入时
// 回调退化为 userinfo 单一声明校验（本地开发兼容路径）。
type IDTokenVerifierOption func(*Handler)

// WithIDTokenVerifier 注入 ID Token 验证器（nil 被忽略）。
func WithIDTokenVerifier(verifier oidcIDTokenVerifier) IDTokenVerifierOption {
	return func(handler *Handler) {
		if verifier != nil {
			handler.idTokenVerifier = verifier
		}
	}
}

type applicationService interface {
	Login(ctx context.Context, input application.LoginInput) (application.SessionResult, error)
	LoginOIDC(ctx context.Context, input application.OIDCLoginInput) (application.SessionResult, error)
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
func NewHandler(service applicationService, logger *slog.Logger, authConfig config.AuthConfig, auditRecorder lifecycleAuditRecorder, auditConfig config.AuditConfig, loginTargetResolver applicationregistry.LoginTargetResolver, options ...IDTokenVerifierOption) (*Handler, error) {
	if service == nil || logger == nil || auditRecorder == nil {
		return nil, errors.New("identity HTTP handler dependencies must not be nil")
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
		service: service,
		logger:  logger,
		cookie:  cookieConfig{name: cookieName, secure: authConfig.SessionCookieSecure, sameSite: sameSite},
		oidc: oidcLoginConfig{
			enabled:       authConfig.KeycloakOIDCEnabled,
			issuer:        strings.TrimRight(strings.TrimSpace(authConfig.KeycloakOIDCIssuer), "/"),
			clientID:      strings.TrimSpace(authConfig.KeycloakOIDCClientID),
			clientSecret:  authConfig.KeycloakOIDCClientSecret,
			redirectPath:  strings.TrimSpace(authConfig.KeycloakOIDCRedirectPath),
			publicBaseURL: strings.TrimRight(strings.TrimSpace(authConfig.PublicBaseURL), "/"),
			stateCookie:   "bp_oidc_state",
			nonceCookie:   "bp_oidc_nonce",
		},
		auditRecorder: auditRecorder,
		auditConfig:   auditConfig,
	}
	if loginTargetResolver != nil {
		handler.loginTargets = loginTargetResolver
	}
	for _, apply := range options {
		apply(handler)
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
	ExpiresAt          time.Time `json:"expires_at"`
	RedirectURL        string    `json:"redirect_url"`
	MustChangePassword bool      `json:"must_change_password"`
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
	if strings.EqualFold(strings.TrimSpace(payload.LoginType), "keycloak") ||
		(payload.Account == "" && payload.Password == "" && handler.oidc.enabled) {
		handler.BeginOIDCLogin(writer, request)
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

// BeginOIDCLogin starts the platform's normal browser login. It is an RP
// redirect only; no Broker Verification endpoint is called here.
func (handler *Handler) BeginOIDCLogin(writer http.ResponseWriter, request *http.Request) {
	if !handler.oidc.enabled || handler.oidc.issuer == "" || handler.oidc.clientID == "" || handler.oidc.redirectPath == "" {
		httpresponse.WriteError(writer, request, http.StatusServiceUnavailable, httperror.New("AUTH_OIDC_UNAVAILABLE", "统一认证暂不可用", nil))
		return
	}
	state, err := randomOIDCState()
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return
	}
	nonce, err := randomOIDCState()
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusInternalServerError, httperror.Internal)
		return
	}
	secure := handler.cookie.secure
	http.SetCookie(writer, &http.Cookie{Name: handler.oidc.stateCookie, Value: state, Path: "/", HttpOnly: true, Secure: secure, SameSite: handler.cookie.sameSite, MaxAge: 600})
	http.SetCookie(writer, &http.Cookie{Name: handler.oidc.nonceCookie, Value: nonce, Path: "/", HttpOnly: true, Secure: secure, SameSite: handler.cookie.sameSite, MaxAge: 600})
	query := url.Values{
		"client_id": {handler.oidc.clientID}, "response_type": {"code"},
		"scope": {"openid profile"}, "state": {state}, "nonce": {nonce},
		"redirect_uri":   {handler.oidc.redirectURI(request)},
		"code_challenge": {oidcCodeChallenge(state)}, "code_challenge_method": {"S256"},
	}
	http.Redirect(writer, request, handler.oidc.issuer+"/protocol/openid-connect/auth?"+query.Encode(), http.StatusFound)
}

// OIDCCallback exchanges the code with Keycloak and resolves identity through
// UserInfo. Keycloak has already validated the access token before UserInfo
// returns; this callback never performs Broker Verification or records a gate.
func (handler *Handler) OIDCCallback(writer http.ResponseWriter, request *http.Request) {
	if !handler.oidc.enabled {
		httpresponse.WriteError(writer, request, http.StatusNotFound, httperror.NotFound)
		return
	}
	stateCookie, err := request.Cookie(handler.oidc.stateCookie)
	if err != nil || stateCookie.Value == "" || request.URL.Query().Get("state") == "" || stateCookie.Value != request.URL.Query().Get("state") {
		httpresponse.WriteError(writer, request, http.StatusBadRequest, httperror.New("AUTH_OIDC_STATE_INVALID", "登录状态已失效，请重试", nil))
		return
	}
	nonceCookie, err := request.Cookie(handler.oidc.nonceCookie)
	if err != nil || nonceCookie.Value == "" {
		httpresponse.WriteError(writer, request, http.StatusBadRequest, httperror.New("AUTH_OIDC_STATE_INVALID", "登录状态已失效，请重试", nil))
		return
	}
	code := strings.TrimSpace(request.URL.Query().Get("code"))
	if code == "" {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_OIDC_FAILED", "统一认证失败", nil))
		return
	}
	http.SetCookie(writer, &http.Cookie{Name: handler.oidc.stateCookie, Value: "", Path: "/", HttpOnly: true, Secure: handler.cookie.secure, SameSite: handler.cookie.sameSite, MaxAge: -1})
	http.SetCookie(writer, &http.Cookie{Name: handler.oidc.nonceCookie, Value: "", Path: "/", HttpOnly: true, Secure: handler.cookie.secure, SameSite: handler.cookie.sameSite, MaxAge: -1})
	accessToken, rawIDToken, err := handler.exchangeOIDCCode(request.Context(), request, code, stateCookie.Value)
	if err != nil {
		handler.logger.Warn("Keycloak OIDC code exchange failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_OIDC_FAILED", "统一认证失败", nil))
		return
	}
	// P1-2：验签 ID Token 并校验 nonce/aud/azp，再用 userinfo 的 identity_id 与
	// 已验证的 sub 交叉确认；任何一步不一致都按统一认证失败处理。
	var identityID string
	if handler.idTokenVerifier != nil {
		verifiedID, verifyErr := handler.idTokenVerifier.VerifyIDToken(request.Context(), rawIDToken, nonceCookie.Value, handler.oidc.clientID)
		if verifyErr != nil {
			handler.logger.Warn("Keycloak OIDC ID token verification failed", "error", verifyErr)
			httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_OIDC_FAILED", "统一认证失败", nil))
			return
		}
		identityID = verifiedID
	}
	userInfoID, err := handler.fetchOIDCIdentity(request.Context(), accessToken)
	if err != nil {
		handler.logger.Warn("Keycloak OIDC identity lookup failed", "error", err)
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_OIDC_FAILED", "统一认证失败", nil))
		return
	}
	if identityID != "" && identityID != userInfoID {
		handler.logger.Warn("Keycloak OIDC identity mismatch", "id_token", identityID, "userinfo", userInfoID)
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_OIDC_FAILED", "统一认证失败", nil))
		return
	}
	if identityID == "" {
		identityID = userInfoID
	}
	result, err := handler.service.LoginOIDC(request.Context(), application.OIDCLoginInput{IdentityID: identityID, IPAddress: remoteIP(request), UserAgent: request.UserAgent()})
	if err != nil {
		httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.New("AUTH_OIDC_ACCOUNT_NOT_LINKED", "统一身份尚未绑定平台账号", nil))
		return
	}
	handler.setSessionCookie(writer, result.Token, result.ExpiresAt)
	handler.recordLogin(request, result)
	http.Redirect(writer, request, "/", http.StatusFound)
}

func randomOIDCState() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func oidcCodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (config oidcLoginConfig) redirectURI(request *http.Request) string {
	if strings.HasPrefix(config.redirectPath, "http://") || strings.HasPrefix(config.redirectPath, "https://") {
		return config.redirectPath
	}
	if config.publicBaseURL != "" {
		return config.publicBaseURL + "/" + strings.TrimLeft(config.redirectPath, "/")
	}
	return "http://" + request.Host + "/" + strings.TrimLeft(config.redirectPath, "/")
}

func (handler *Handler) exchangeOIDCCode(ctx context.Context, request *http.Request, code, verifier string) (string, string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {handler.oidc.clientID}, "redirect_uri": {handler.oidc.redirectURI(request)}, "code_verifier": {verifier}}
	if handler.oidc.clientSecret != "" {
		form.Set("client_secret", handler.oidc.clientSecret)
	}
	tokenRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, handler.oidc.issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(tokenRequest)
	if err != nil {
		return "", "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", fmt.Errorf("OIDC token endpoint returned %s", response.Status)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil || payload.AccessToken == "" {
		return "", "", errors.New("OIDC token response has no access_token")
	}
	if strings.TrimSpace(payload.IDToken) == "" {
		return "", "", errors.New("OIDC token response has no id_token")
	}
	return payload.AccessToken, payload.IDToken, nil
}

func (handler *Handler) fetchOIDCIdentity(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, handler.oidc.issuer+"/protocol/openid-connect/userinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("OIDC userinfo endpoint returned %s", response.Status)
	}
	var payload struct {
		Subject    string `json:"sub"`
		IdentityID string `json:"identity_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	identityID := strings.TrimSpace(payload.IdentityID)
	if identityID == "" || strings.TrimSpace(payload.Subject) == "" || identityID != strings.TrimSpace(payload.Subject) {
		return "", errors.New("OIDC identity_id does not match sub")
	}
	return identityID, nil
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
	// The authenticated principal contains a live server-side authorization snapshot.
	// It must never be stored by browsers or shared proxies, otherwise a role change could
	// leave an active page rendering a stale set of permissions.
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Vary", "Cookie")
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
		ActorType: "USER", ActorID: result.UserID, ActorName: operatorDisplayName(result.UserName, result.AccountName), SessionID: result.SessionID,
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
			ActorType: "USER", ActorID: concurrent.UserID, ActorName: operatorDisplayName(concurrent.UserName, concurrent.AccountName),
			Action: "auth.login.concurrent_denied", ResourceType: "auth_account", ResourceID: concurrent.AccountID, ResourceName: concurrent.AccountName,
			Result: "FAILURE", RiskLevel: "MEDIUM", Classification: "INTERNAL", Summary: "账号已有有效会话，拒绝其他终端并发登录",
		})
		return
	}
	var failed application.LoginFailedError
	if errors.As(err, &failed) {
		handler.recordLifecycleEvent(request, failed.TenantID, auditapplication.EventInput{
			ActorType: "USER", ActorID: failed.UserID, ActorName: operatorDisplayName(failed.UserName, failed.AccountName),
			Action: "auth.login.failed", ResourceType: "auth_account", ResourceID: failed.AccountID, ResourceName: failed.AccountName,
			Result: "FAILURE", RiskLevel: "MEDIUM", Classification: "INTERNAL", Summary: "本地账号密码登录失败",
		})
		return
	}
	// 锁定窗口内的密码失败走统一 auth.login.failed 审计（防枚举），不再有独立的
	// locked 事件分支；锁定状态由安全模块的 sec_login_attempt 与解锁接口承载。
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
	var concurrent application.ConcurrentSessionError
	switch {
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
	return sessionResponse{ExpiresAt: result.ExpiresAt.UTC(), RedirectURL: result.RedirectURL, MustChangePassword: result.MustChangePassword}
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

// operatorDisplayName 优先返回用户显示名，为空时回退到账号名，确保审计操作人不为空。
func operatorDisplayName(displayName, accountName string) string {
	if name := strings.TrimSpace(displayName); name != "" {
		return name
	}
	return strings.TrimSpace(accountName)
}
