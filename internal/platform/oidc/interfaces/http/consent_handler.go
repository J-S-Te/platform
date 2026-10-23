package oidchttp

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
)

// Consent 暴露已认证浏览器会话的持久用户同意决定。bootstrap 可将其发布在
// GET /oauth2/consent 供同意页使用。它绝不把既有授权当作签发凭据返回；
// 授权码仍由授权流程负责签发。
func (h *Handler) Consent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	principal, ok := h.authenticateBrowserSession(r)
	if !ok {
		writeOAuthError(w, http.StatusUnauthorized, "login_required", "")
		return
	}
	input, err := consentInputFromValues(r.URL.Query(), principal.Tenant.ID, principal.User.ID)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	decision, err := h.service.DecideConsent(r.Context(), input)
	if err != nil {
		h.writeConsentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"required": decision.Required,
		"granted":  decision.Granted,
		"scope":    strings.Join(decision.Scopes, " "),
	}, true)
}

// GrantConsent 记录用户对请求的已登记 scope 集合的精确授权。bootstrap 可在其
// CSRF 保护的同意 UI 确认用户操作后，将本端点发布于 POST /oauth2/consent/grant。
func (h *Handler) GrantConsent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	principal, ok := h.authenticateBrowserSession(r)
	if !ok {
		writeOAuthError(w, http.StatusUnauthorized, "login_required", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthorizeRequestBytes)
	if err := r.ParseForm(); err != nil || requireAtMostOne(r.PostForm, "client_id", "scope") != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	input, err := consentInputFromValues(r.PostForm, principal.Tenant.ID, principal.User.ID)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if err = h.service.GrantConsent(r.Context(), input); err != nil {
		h.writeConsentError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil, true)
}

// RevokeConsent 移除一个客户端与已认证用户之间的全部已存储授权。发布在
// DELETE /oauth2/consent 使撤回独立于普通授权重定向。
func (h *Handler) RevokeConsent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	principal, ok := h.authenticateBrowserSession(r)
	if !ok {
		writeOAuthError(w, http.StatusUnauthorized, "login_required", "")
		return
	}
	if err := requireAtMostOne(r.URL.Query(), "client_id"); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	clientID := strings.TrimSpace(r.URL.Query().Get("client_id"))
	if !validProtocolParameter(clientID, 255) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if err := h.service.RevokeConsent(r.Context(), principal.Tenant.ID, principal.User.ID, clientID); err != nil {
		h.writeConsentError(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil, true)
}

func consentInputFromValues(values url.Values, tenantID, userID string) (application.ConsentInput, error) {
	clientID := strings.TrimSpace(values.Get("client_id"))
	scope := values.Get("scope")
	if !validProtocolParameter(clientID, 255) || validateScopeParameter(scope) != nil || len(strings.Fields(scope)) == 0 {
		return application.ConsentInput{}, errors.New("invalid consent parameters")
	}
	return application.ConsentInput{TenantID: tenantID, UserID: userID, ClientID: clientID, Scopes: strings.Fields(scope)}, nil
}

func (h *Handler) writeConsentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidRequest), errors.Is(err, application.ErrInvalidClient):
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
	case errors.Is(err, application.ErrInvalidScope):
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "")
	default:
		h.logger.Error("OIDC consent request failed", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
	}
}
