package oidchttp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
)

// Revoke implements RFC 7009. After successful client authentication it returns 200 for unknown,
// expired, or already-revoked tokens, deliberately preventing token-existence disclosure.
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
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
	if err := requireAtMostOne(r.Form, "token", "token_type_hint", "client_id", "client_secret"); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	authentication, ok := h.tokenClientAuthentication(w, r, false, false, false)
	if !ok {
		return
	}
	token := strings.TrimSpace(r.Form.Get("token"))
	if token == "" || len(token) > 8192 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	hint := strings.TrimSpace(r.Form.Get("token_type_hint"))
	types := []string{}
	switch hint {
	case domain.TokenTypeAccess, domain.TokenTypeRefresh:
		types = append(types, hint)
	default:
		// RFC 7009 makes the hint optional. Try both supported representations so an unhinted
		// refresh token is revoked while an access token remains blacklisted.
		types = append(types, domain.TokenTypeAccess, domain.TokenTypeRefresh)
	}
	for _, tokenType := range types {
		err := h.service.Revoke(r.Context(), application.RevokeTokenInput{
			ClientAuthentication: authentication, Token: token, TokenType: tokenType, Reason: "client_revocation",
		})
		if err == nil {
			continue
		}
		if errors.Is(err, application.ErrInvalidClient) {
			writeInvalidClient(w)
			return
		}
		if errors.Is(err, application.ErrInvalidRequest) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
			return
		}
		h.logger.Error("OIDC token revocation failed", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	setNoStoreHeaders(w)
	w.WriteHeader(http.StatusOK)
}
