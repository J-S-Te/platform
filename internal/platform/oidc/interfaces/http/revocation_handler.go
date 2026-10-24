package oidchttp

import (
	"errors"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
)

// Revoke 实现 RFC 7009。客户端认证成功后，对未知、过期或已撤销的令牌一律返回 200，
// 刻意避免泄露令牌是否存在。
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
		// RFC 7009 使 hint 可选。依次尝试两种受支持的表示，使未提示的 refresh token
		// 被撤销，而 access token 仍保留在黑名单中。
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
