// Package http exposes the OAuth Client Credentials token endpoint.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const maxTokenRequestBytes = 64 << 10

type Handler struct {
	service *application.Service
	logger  *slog.Logger
}

func NewHandler(service *application.Service, logger *slog.Logger) (*Handler, error) {
	if service == nil || logger == nil {
		return nil, errors.New("application token handler dependencies must not be nil")
	}
	return &Handler{service: service, logger: logger}, nil
}

// IssueToken implements OAuth 2.0 Client Credentials with client_secret_basic only. Client
// secrets are intentionally not accepted in form fields to prevent accidental body logging.
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
