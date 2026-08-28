package oidchttp

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Logout implements a conservative RP-initiated logout edge: it revokes the current platform
// cookie session when the identity logout adapter is configured, then only redirects externally to
// an exact registered redirect URI. Any invalid logout parameter falls back to the platform's
// existing issuer + "/login.html" landing page rather than an unregistered external location.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxAuthorizeRequestBytes)
		if err := r.ParseForm(); err != nil {
			redirectToLogoutComplete(w, r, h.logoutCompleteLocation())
			return
		}
	}
	params := r.URL.Query()
	if r.Method == http.MethodPost {
		for key, values := range r.PostForm {
			params[key] = append(params[key], values...)
		}
	}
	location := h.validatedPostLogoutRedirect(r, params)
	if err := h.logoutBrowserSession(r); err != nil {
		// 已认证会话的统一撤销失败时不能继续清 Cookie 并伪装成成功退出；保留浏览器
		// 会话可让用户重试，同时避免残留 Realm 会话在下一次登录时切回旧账号。
		h.logger.Error("platform global logout failed", "error", err)
		setNoStoreHeaders(w)
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	// 所有业务系统共用当前浏览器 Origin，但 Cookie Path 各自隔离。Clear-Site-Data
	// 让支持该标准的浏览器同时清理各子系统 Cookie/Storage；服务端会话仍以本次
	// 已完成的平台和 Keycloak 撤销为权威，不能依赖浏览器清理替代服务端注销。
	w.Header().Set("Clear-Site-Data", `"cookies", "storage"`)
	h.clearSessionCookie(w)
	redirectToLogoutComplete(w, r, location)
}

func (h *Handler) logoutBrowserSession(r *http.Request) error {
	if h.sessionLogout == nil {
		return nil
	}
	principal, ok := h.authenticateBrowserSession(r)
	if !ok {
		return nil
	}
	if err := h.sessionLogout.Logout(r.Context(), principal); err != nil {
		return err
	}
	return nil
}

func (h *Handler) validatedPostLogoutRedirect(r *http.Request, values url.Values) string {
	fallback := h.logoutCompleteLocation()
	if err := requireAtMostOne(values, "post_logout_redirect_uri", "id_token_hint", "client_id", "state"); err != nil {
		return fallback
	}
	redirectURI := strings.TrimSpace(values.Get("post_logout_redirect_uri"))
	if redirectURI == "" || !validAbsoluteRedirectURI(redirectURI) || h.logoutRedirects == nil {
		return fallback
	}
	clientID := strings.TrimSpace(values.Get("client_id"))
	idTokenHint := strings.TrimSpace(values.Get("id_token_hint"))
	if idTokenHint != "" {
		hintClientID, ok := unverifiedJWTClientID(idTokenHint)
		if !ok {
			return fallback
		}
		claims, err := h.jwtManager.VerifyIDToken(idTokenHint, hintClientID, h.clock.Now().UTC())
		if err != nil || claims.ClientID != hintClientID {
			return fallback
		}
		if clientID != "" && clientID != claims.ClientID {
			return fallback
		}
		clientID = claims.ClientID
	}
	if !validProtocolParameter(clientID, 255) {
		return fallback
	}
	registered, err := h.logoutRedirects.IsRegisteredPostLogoutRedirectURI(r.Context(), clientID, redirectURI)
	if err != nil || !registered {
		return fallback
	}
	state := values.Get("state")
	if validateTextParameter(state, 2048) != nil {
		return fallback
	}
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return fallback
	}
	if state != "" {
		query := parsed.Query()
		query.Set("state", state)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

func (h *Handler) logoutCompleteLocation() string {
	return strings.TrimRight(h.jwtManager.Issuer(), "/") + "/login.html"
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: h.cookie.name, Value: "", Path: "/", HttpOnly: true, Secure: h.cookie.secure, SameSite: h.cookie.sameSite,
		Expires: h.clock.Now().UTC().Add(-time.Second), MaxAge: -1,
	})
}

func redirectToLogoutComplete(w http.ResponseWriter, r *http.Request, location string) {
	setNoStoreHeaders(w)
	status := http.StatusFound
	if r.Method == http.MethodPost {
		status = http.StatusSeeOther
	}
	http.Redirect(w, r, location, status)
}
