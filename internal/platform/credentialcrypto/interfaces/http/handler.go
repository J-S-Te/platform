package credentialhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/credentialcrypto"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

const preAuthCookieName = "bp_credential_preauth"

type Authenticator interface {
	Authenticate(context.Context, string) (authctx.Principal, error)
	CookieName() string
}

type Handler struct {
	KeyRing          *credentialcrypto.KeyRing
	Challenges       *credentialcrypto.ChallengeService
	Mode             string
	Authenticator    Authenticator
	SessionCookie    string
	CookieSecure     bool
	Logger           *slog.Logger
	ResponseMaxBytes int64
	// AllowLoginAccount is applied after decryption and challenge consumption.
	AllowLoginAccount func(string) bool
}

type challengeRequest struct {
	Purpose    string `json:"purpose"`
	ResourceID string `json:"resource_id"`
}

func (handler *Handler) Enabled() bool { return handler != nil && handler.Mode != "disabled" }

func (handler *Handler) required() bool { return handler.Mode == "required" }

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	httpresponse.WriteError(w, r, status, httperror.New(code, message, nil))
}

func valueFromRequestCookie(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

func (handler *Handler) sessionCookieName() string {
	if strings.TrimSpace(handler.SessionCookie) != "" {
		return strings.TrimSpace(handler.SessionCookie)
	}
	if handler.Authenticator != nil {
		return handler.Authenticator.CookieName()
	}
	return ""
}

func (handler *Handler) bindingForIssue(w http.ResponseWriter, r *http.Request, purpose string) (tenantID, bindingValue string, err error) {
	principal, authenticated := authctx.PrincipalFromContext(r.Context())
	sessionCookie := valueFromRequestCookie(r, handler.sessionCookieName())
	if purpose != credentialcrypto.PurposeLogin && !authenticated && sessionCookie != "" && handler.Authenticator != nil {
		if resolved, authErr := handler.Authenticator.Authenticate(r.Context(), sessionCookie); authErr == nil {
			principal, authenticated = resolved, true
		}
	}
	if purpose != credentialcrypto.PurposeLogin && authenticated {
		if sessionCookie == "" {
			return "", "", errors.New("session cookie missing")
		}
		return principal.Tenant.ID, sessionCookie, nil
	}
	if purpose != credentialcrypto.PurposeLogin {
		return "", "", errors.New("authentication required")
	}
	cookie := valueFromRequestCookie(r, preAuthCookieName)
	if cookie == "" {
		raw, err := credentialcrypto.NewRawID()
		if err != nil {
			return "", "", err
		}
		cookie = raw
		http.SetCookie(w, &http.Cookie{
			Name: preAuthCookieName, Value: cookie, Path: "/api/v1/auth",
			HttpOnly: true, Secure: handler.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: 300,
		})
	}
	return "", cookie, nil
}

func (handler *Handler) bindingForConsume(r *http.Request, purpose string) (tenantID, bindingValue string, err error) {
	principal, authenticated := authctx.PrincipalFromContext(r.Context())
	if authenticated {
		cookie := valueFromRequestCookie(r, handler.sessionCookieName())
		if cookie == "" {
			return "", "", errors.New("session cookie missing")
		}
		return principal.Tenant.ID, cookie, nil
	}
	if purpose != credentialcrypto.PurposeLogin {
		return "", "", errors.New("authentication required")
	}
	cookie := valueFromRequestCookie(r, preAuthCookieName)
	if cookie == "" {
		return "", "", errors.New("pre-authentication cookie missing")
	}
	return "", cookie, nil
}

func (handler *Handler) IssueChallenge(w http.ResponseWriter, r *http.Request) {
	if !handler.Enabled() {
		writeError(w, r, http.StatusServiceUnavailable, "CREDENTIAL_CRYPTO_UNAVAILABLE", "凭据加密尚未启用。")
		return
	}
	defer r.Body.Close()
	var request challengeRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, r, http.StatusBadRequest, "CREDENTIAL_ENVELOPE_INVALID", "凭据参数请求格式不正确。")
		return
	}
	purpose, ok := credentialcrypto.Purposes[strings.TrimSpace(request.Purpose)]
	if !ok {
		writeError(w, r, http.StatusBadRequest, "CREDENTIAL_ENVELOPE_INVALID", "凭据用途无效。")
		return
	}
	tenantID, bindingValue, err := handler.bindingForIssue(w, r, purpose.Code)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "CREDENTIAL_ENVELOPE_INVALID", "当前会话无法申请凭据加密参数。")
		return
	}
	if purpose.ResourceBound != (strings.TrimSpace(request.ResourceID) != "") {
		writeError(w, r, http.StatusBadRequest, "CREDENTIAL_ENVELOPE_INVALID", "凭据资源参数与用途不匹配。")
		return
	}
	binding := credentialcrypto.HashBinding(bindingValue)
	issued, err := handler.Challenges.Issue(r.Context(), handler.KeyRing.ActiveKID(), purpose.Code, strings.TrimSpace(request.ResourceID), tenantID, binding)
	if err != nil {
		handler.Logger.Error("credential challenge issue failed", "purpose", purpose.Code, "error", err)
		writeError(w, r, http.StatusInternalServerError, "CREDENTIAL_CRYPTO_UNAVAILABLE", "无法签发凭据加密参数。")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpresponse.WriteSuccess(w, r, http.StatusOK, "凭据加密参数已签发", map[string]any{
		"protocol": credentialcrypto.Protocol, "kid": issued.KID,
		"public_key": handler.KeyRing.PublicKeyPEM(), "challenge_id": issued.Raw,
		"purpose": issued.Purpose, "resource_id": issued.ResourceID,
		"expires_at": issued.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

type bufferedWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	max      int64
	overflow bool
}

func (writer *bufferedWriter) Header() http.Header { return writer.header }
func (writer *bufferedWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}
func (writer *bufferedWriter) Write(value []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	if writer.max > 0 && int64(writer.body.Len()+len(value)) > writer.max {
		writer.overflow = true
		return 0, errors.New("credential response exceeds configured limit")
	}
	return writer.body.Write(value)
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

func (handler *Handler) wrap(w http.ResponseWriter, r *http.Request, purpose, routeKey, resourceID, contentType string, next http.HandlerFunc) {
	if !handler.Enabled() {
		next(w, r)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		if handler.required() {
			writeError(w, r, http.StatusUnprocessableEntity, "CREDENTIAL_ENCRYPTION_REQUIRED", "该接口必须使用加密凭据提交。")
			return
		}
		// Compat keeps the old multipart/JSON clients working; encryption is never
		// inferred from a missing envelope.
		next(w, r)
		return
	}
	var body struct {
		Envelope credentialcrypto.RequestEnvelope `json:"credential_envelope"`
	}
	rawBody, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	r.Body = io.NopCloser(bytes.NewReader(rawBody))
	if readErr != nil {
		writeError(w, r, http.StatusRequestEntityTooLarge, "CREDENTIAL_ENVELOPE_INVALID", "凭据请求体过大。")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&body)
	if decodeErr != nil || body.Envelope.Protocol == "" {
		if handler.required() {
			writeError(w, r, http.StatusUnprocessableEntity, "CREDENTIAL_ENCRYPTION_REQUIRED", "该接口必须使用加密凭据提交。")
			return
		}
		next(w, r)
		return
	}
	env := body.Envelope
	if env.Protocol != credentialcrypto.Protocol {
		writeError(w, r, http.StatusBadRequest, "CREDENTIAL_ENVELOPE_INVALID", "凭据协议版本不受支持。")
		return
	}
	expected := credentialcrypto.EnvelopeExpectation{Purpose: purpose, ResourceID: resourceID, RouteKey: routeKey, HTTPMethod: r.Method, ContentType: contentType}
	plain, responseKey, err := handler.KeyRing.OpenRequest(env, expected)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "CREDENTIAL_DECRYPT_FAILED", "凭据请求无法解密，请刷新页面后重试。")
		return
	}
	tenantID, bindingValue, err := handler.bindingForConsume(r, purpose)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "CREDENTIAL_ENVELOPE_INVALID", "当前会话无法使用凭据加密参数。")
		return
	}
	if err := handler.Challenges.Consume(r.Context(), env.ChallengeID, env.KID, env.Purpose, env.ResourceID, tenantID, credentialcrypto.HashBinding(bindingValue)); err != nil {
		switch {
		case errors.Is(err, credentialcrypto.ErrChallengeUsed):
			writeError(w, r, http.StatusConflict, "CREDENTIAL_CHALLENGE_USED", "该凭据请求已处理，请核对业务结果，勿重复提交。")
		case errors.Is(err, credentialcrypto.ErrChallengeExpired):
			writeError(w, r, http.StatusConflict, "CREDENTIAL_CHALLENGE_EXPIRED", "凭据加密参数已过期，请刷新页面后重新提交。")
		default:
			writeError(w, r, http.StatusConflict, "CREDENTIAL_ENVELOPE_INVALID", "凭据加密参数无效或与当前请求不匹配。")
		}
		return
	}
	if purpose == credentialcrypto.PurposeLogin && handler.AllowLoginAccount != nil {
		var loginBody struct {
			Account string `json:"account"`
		}
		if json.Unmarshal(plain, &loginBody) != nil || !handler.AllowLoginAccount(loginBody.Account) {
			writeError(w, r, http.StatusTooManyRequests, "PLATFORM_RATE_LIMITED", "请求过于频繁，请稍后再试")
			return
		}
	}
	// Restore the original body and content type before entering the unchanged
	// business handler.
	r.Body = io.NopCloser(bytes.NewReader(plain))
	r.ContentLength = int64(len(plain))
	r.Header.Set("Content-Type", env.ContentType)
	buffer := &bufferedWriter{header: make(http.Header), max: handler.ResponseMaxBytes}
	next(buffer, r)
	status := buffer.status
	if status == 0 {
		status = http.StatusOK
	}
	if buffer.body.Len() == 0 {
		copyHeaders(w.Header(), buffer.header)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	envelope, err := credentialcrypto.SealResponse(env.KID, env.ChallengeID, env.Purpose, env.ResourceID, responseKey, buffer.body.Bytes())
	if err != nil {
		copyHeaders(w.Header(), buffer.header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"CREDENTIAL_RESPONSE_ENCRYPTION_FAILED","message":"操作结果无法加密返回，请核对业务状态，勿自动重复提交。"}`))
		return
	}
	copyHeaders(w.Header(), buffer.header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"credential_envelope": envelope})
}

func accountIDFromPath(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for index, part := range parts {
		if part == "accounts" && index+1 < len(parts) {
			return strings.TrimSpace(parts[index+1])
		}
	}
	return ""
}

// WrapJSON wraps a registered JSON credential route.
func (handler *Handler) WrapJSON(purpose, routeKey string, next http.HandlerFunc) http.HandlerFunc {
	return handler.WrapJSONResource(purpose, routeKey, nil, next)
}

// WrapJSONResource wraps a resource-bound JSON route. The resource resolver is
// evaluated server-side from the URL, never from the client envelope alone.
func (handler *Handler) WrapJSONResource(purpose, routeKey string, resource func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resourceID := ""
		if purpose == credentialcrypto.PurposeInitialize || purpose == credentialcrypto.PurposeReset {
			resourceID = accountIDFromPath(r)
			if resource != nil {
				resourceID = strings.TrimSpace(resource(r))
			}
		}
		handler.wrap(w, r, purpose, routeKey, resourceID, "application/json", next)
	}
}

// WrapJSONOrMultipart wraps the batch import route, which supports both a JSON
// item list and raw multipart CSV. The envelope still binds its actual content type.
func (handler *Handler) WrapJSONOrMultipart(purpose, routeKey string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler.wrap(w, r, purpose, routeKey, "", "", next)
	}
}

// WrapMultipart wraps the CSV import route while preserving the original
// multipart bytes and Content-Type.
func (handler *Handler) WrapMultipart(routeKey string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler.wrap(w, r, credentialcrypto.PurposeCreateEmployees, routeKey, "", "multipart/form-data", next)
	}
}
