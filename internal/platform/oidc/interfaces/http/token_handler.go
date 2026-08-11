package oidchttp

import (
	"errors"
	"net/http"
	"strings"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
)

// Token 端点只接受表单 POST，拒绝查询串、重复敏感参数和未知授权类型；授权码与刷新令牌
// 走 OIDC 服务，机器凭据暂由既有签发器处理，以保持应用令牌格式和权限策略兼容。
func (h *Handler) Token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if r.URL != nil && r.URL.RawQuery != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if err := requireAtMostOne(r.Form, "grant_type", "client_id", "client_secret", "client_assertion", "client_assertion_type", "client_assertion_audience", "code", "redirect_uri", "code_verifier", "refresh_token", "scope"); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	grantType := strings.TrimSpace(r.Form.Get("grant_type"))
	switch grantType {
	case "authorization_code":
		h.exchangeAuthorizationCode(w, r)
	case "refresh_token":
		h.refreshToken(w, r)
	case "client_credentials":
		h.issueClientCredentials(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

func (h *Handler) exchangeAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	// 客户端认证通过后，应用层仍会绑定授权码的 client_id、redirect_uri 和 PKCE verifier；
	// 传输层只负责收集参数，不能把认证成功等同于授权码可兑换。
	authentication, ok := h.tokenClientAuthentication(w, r, true, true, true)
	if !ok {
		return
	}
	result, err := h.service.ExchangeAuthorizationCode(r.Context(), application.AuthorizationCodeExchangeInput{
		ClientAuthentication: authentication,
		Code:                 r.Form.Get("code"), RedirectURI: r.Form.Get("redirect_uri"), CodeVerifier: r.Form.Get("code_verifier"),
	})
	if err != nil {
		h.writeOIDCTokenError(w, err)
		return
	}
	writeTokenResult(w, result)
}

func (h *Handler) refreshToken(w http.ResponseWriter, r *http.Request) {
	// 刷新令牌同样绑定原客户端；重放和令牌族撤销由应用层统一处理，HTTP 层只映射为
	// 标准 invalid_grant，避免向调用方暴露令牌是否存在等内部状态。
	authentication, ok := h.tokenClientAuthentication(w, r, true, true, true)
	if !ok {
		return
	}
	result, err := h.service.Refresh(r.Context(), application.RefreshTokenInput{
		ClientAuthentication: authentication, RefreshToken: r.Form.Get("refresh_token"),
	})
	if err != nil {
		h.writeOIDCTokenError(w, err)
		return
	}
	writeTokenResult(w, result)
}

func (h *Handler) issueClientCredentials(w http.ResponseWriter, r *http.Request) {
	// client_credentials 不允许公共客户端表单身份或 private_key_jwt；当前兼容签发器要求
	// HTTP Basic，并在服务端校验该机器客户端可用的最小 scope。
	if h.legacyIssuer == nil {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "")
		return
	}
	authentication, ok := h.tokenClientAuthentication(w, r, false, false, false)
	if !ok {
		return
	}
	result, err := h.legacyIssuer.IssueClientCredentials(r.Context(), authentication.ClientID, authentication.ClientSecret, strings.Fields(r.Form.Get("scope")))
	if err != nil {
		switch {
		case errors.Is(err, applicationregistryapplication.ErrUnauthenticated):
			writeInvalidClient(w)
		case errors.Is(err, applicationregistryapplication.ErrInvalidGrant):
			writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "")
		case errors.Is(err, applicationregistryapplication.ErrInvalidScope):
			writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "")
		default:
			h.logger.Error("legacy client credentials token issuance failed", "error", err)
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		}
		return
	}
	writeLegacyTokenResult(w, result)
}

// 用户授权类型支持三种互斥身份：机密客户端 HTTP Basic、公共客户端表单 client_id，或
// private_key_jwt。断言 audience 始终由服务端 issuer 推导，拒绝客户端自报 audience；
// 同时出现多种认证材料或多个 Authorization 头时失败关闭，避免解析差异造成身份混淆。
func (h *Handler) tokenClientAuthentication(w http.ResponseWriter, r *http.Request, allowPublicClientForm, allowClientAssertion, allowBrokerClientSecretPost bool) (application.ClientAuthentication, bool) {
	basicClientID, basicClientSecret, hasBasic := r.BasicAuth()
	authorizationValues := r.Header.Values("Authorization")
	formClientID := strings.TrimSpace(r.Form.Get("client_id"))
	hasAssertion := formParameterPresent(r.Form, "client_assertion")
	hasAssertionType := formParameterPresent(r.Form, "client_assertion_type")
	if len(authorizationValues) > 1 || (len(authorizationValues) == 1 && !hasBasic) || formParameterPresent(r.Form, "client_assertion_audience") {
		writeInvalidClient(w)
		return application.ClientAuthentication{}, false
	}
	if hasBasic {
		if strings.TrimSpace(basicClientID) == "" || basicClientSecret == "" || formParameterPresent(r.Form, "client_id") || formParameterPresent(r.Form, "client_secret") || hasAssertion || hasAssertionType {
			writeInvalidClient(w)
			return application.ClientAuthentication{}, false
		}
		return application.ClientAuthentication{ClientID: basicClientID, ClientSecret: basicClientSecret}, true
	}
	// Keycloak's OIDC broker uses client_secret_post for the upstream token
	// exchange in the local Keycloak distribution. Keep this compatibility
	// exception limited to the dedicated broker client; all business clients
	// continue to require HTTP Basic authentication.
	if allowBrokerClientSecretPost && formClientID == "keycloak-broker" && formParameterPresent(r.Form, "client_secret") && !hasAssertion && !hasAssertionType {
		return application.ClientAuthentication{ClientID: formClientID, ClientSecret: r.Form.Get("client_secret")}, true
	}
	if hasAssertion || hasAssertionType {
		if !allowClientAssertion || formClientID == "" || formParameterPresent(r.Form, "client_secret") || !hasAssertion || !hasAssertionType ||
			strings.TrimSpace(r.Form.Get("client_assertion")) == "" || strings.TrimSpace(r.Form.Get("client_assertion_type")) == "" {
			writeInvalidClient(w)
			return application.ClientAuthentication{}, false
		}
		return application.ClientAuthentication{
			ClientID: formClientID, ClientAssertion: r.Form.Get("client_assertion"), ClientAssertionType: r.Form.Get("client_assertion_type"),
			ClientAssertionAudience: h.tokenEndpointAudience(),
		}, true
	}
	if allowPublicClientForm && formClientID != "" && !formParameterPresent(r.Form, "client_secret") {
		return application.ClientAuthentication{ClientID: formClientID}, true
	}
	writeInvalidClient(w)
	return application.ClientAuthentication{}, false
}

func (h *Handler) tokenEndpointAudience() string {
	return strings.TrimRight(h.jwtManager.Issuer(), "/") + "/oauth2/token"
}

func (h *Handler) writeOIDCTokenError(w http.ResponseWriter, err error) {
	// 对外只返回 OAuth 标准错误类别；内部数据库、验签和策略错误写服务端日志，不能把
	// 令牌存在性、客户端配置或调用栈暴露给调用方。
	switch {
	case errors.Is(err, application.ErrInvalidClient):
		writeInvalidClient(w)
	case errors.Is(err, application.ErrUnauthorizedClient):
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "")
	case errors.Is(err, application.ErrAccessDenied):
		writeOAuthError(w, http.StatusForbidden, "access_denied", "")
	case errors.Is(err, application.ErrInvalidScope):
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "")
	case errors.Is(err, application.ErrInvalidGrant), errors.Is(err, application.ErrRefreshTokenReplay):
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "")
	case errors.Is(err, application.ErrInvalidRequest):
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
	default:
		h.logger.Error("OIDC token issuance failed", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
	}
}

func formParameterPresent(values map[string][]string, name string) bool {
	_, exists := values[name]
	return exists
}
