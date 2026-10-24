// Package http 暴露 OAuth Client Credentials 令牌端点。
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// maxTokenRequestBytes 限制令牌请求体上限：本端点只需 grant_type/scope 几个字段，
// 64 KiB 足够且能防止恶意超大 body 被解析与记账。
const maxTokenRequestBytes = 64 << 10

// Handler 是应用令牌端点的 HTTP 适配层：只做协议校验与错误映射，
// 授权判定全部委托给 application.Service，自身不持有任何客户端凭据。
type Handler struct {
	service *application.Service
	logger  *slog.Logger
}

// NewHandler 装配令牌端点 Handler；service 或 logger 缺失时返回错误，
// 拒绝构造半可用的认证端点。
func NewHandler(service *application.Service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("application token handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

// IssueToken 实现 OAuth 2.0 Client Credentials，且仅接受 client_secret_basic。
// 刻意不接受表单字段里的 client_secret，防止 secret 随请求体被日志意外记录；
// 表单中出现 client_id/client_secret 一律按 invalid_request 拒绝。
func (h *Handler) IssueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenRequestBytes)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if strings.TrimSpace(r.Form.Get("grant_type")) != "client_credentials" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	if r.Form.Get("client_id") != "" || r.Form.Get("client_secret") != "" {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	clientID, clientSecret, ok := r.BasicAuth()
	if !ok || strings.TrimSpace(clientID) == "" || clientSecret == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth2/token"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}

	result, err := h.service.IssueClientCredentials(r.Context(), clientID, clientSecret, strings.Fields(r.Form.Get("scope")))
	if err != nil {
		switch {
		case errors.Is(err, application.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", `Basic realm="oauth2/token"`)
			oauthError(w, http.StatusUnauthorized, "invalid_client")
		case errors.Is(err, application.ErrInvalidGrant):
			oauthError(w, http.StatusBadRequest, "unauthorized_client")
		case errors.Is(err, application.ErrInvalidScope):
			oauthError(w, http.StatusBadRequest, "invalid_scope")
		default:
			h.logger.Error("application token issuance failed", "error", err)
			oauthError(w, http.StatusInternalServerError, "server_error")
		}
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": result.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   result.ExpiresIn,
		"scope":        result.Scope,
	})
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
