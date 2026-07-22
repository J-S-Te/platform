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
	h.logoutBrowserSession(r)
	h.clearSessionCookie(w)
	redirectToLogoutComplete(w, r, location)
}

func (h *Handler) logoutBrowserSession(r *http.Request) {
	if h.sessionLogout == nil {
		return
	}
	principal, ok := h.authenticateBrowserSession(r)
	if !ok {
		return
	}
	if err := h.sessionLogout.Logout(r.Context(), principal); err != nil {
		// Logout completion must be safe even after an already-expired cookie. Do not disclose
		// session state or turn it into a redirect to an unvalidated URI.
		h.logger.Error("platform session logout during OIDC logout failed", "error", err)
	}
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
