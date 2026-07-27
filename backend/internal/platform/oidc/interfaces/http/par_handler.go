package oidchttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
)

const defaultPARTTL = 90 * time.Second

// PAR accepts an RFC 9126 pushed authorization request. Bootstrap should publish this handler at
// POST /oauth2/par; it intentionally has no router dependency so deployments can wire it alongside
// their existing OIDC endpoints.
func (h *Handler) PAR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	if r.URL != nil && r.URL.RawQuery != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthorizeRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if err := requireAtMostOne(r.Form,
		"client_id", "client_secret", "client_assertion", "client_assertion_type", "client_assertion_audience",
		"response_type", "redirect_uri", "scope", "state", "nonce", "code_challenge", "code_challenge_method", "request",
	); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if err := validateScopeParameter(r.Form.Get("scope")); err != nil ||
		validateTextParameter(r.Form.Get("state"), 2048) != nil ||
		validateTextParameter(r.Form.Get("nonce"), 255) != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	responseType := strings.TrimSpace(r.Form.Get("response_type"))
	if responseType != "code" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	redirectURI := strings.TrimSpace(r.Form.Get("redirect_uri"))
	if redirectURI != "" && !validAbsoluteRedirectURI(redirectURI) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	challenge, method := strings.TrimSpace(r.Form.Get("code_challenge")), strings.TrimSpace(r.Form.Get("code_challenge_method"))
	if (challenge == "") != (method == "") || (method != "" && method != "S256") || !validPKCEChallenge(challenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	authentication, ok := h.tokenClientAuthentication(w, r, true, true)
	if !ok {
		return
	}
	result, err := h.service.PushAuthorizationRequest(r.Context(), application.PushAuthorizationRequestInput{
		ClientAuthentication:  authentication,
		ResponseType:          responseType,
		RedirectURI:           redirectURI,
		Scopes:                strings.Fields(r.Form.Get("scope")),
		State:                 r.Form.Get("state"),
		Nonce:                 r.Form.Get("nonce"),
		CodeChallenge:         challenge,
		CodeChallengeMethod:   method,
		RequestObject:         strings.TrimSpace(r.Form.Get("request")),
		RequestObjectAudience: h.authorizationEndpointAudience(),
		TTL:                   defaultPARTTL,
	})
	if err != nil {
		h.writePARError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"request_uri": result.RequestURI,
		"expires_in":  result.ExpiresIn,
	}, true)
}

func (h *Handler) writePARError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidClient):
		writeInvalidClient(w)
	case errors.Is(err, application.ErrUnauthorizedClient):
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "")
	case errors.Is(err, application.ErrInvalidScope):
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "")
	case errors.Is(err, application.ErrInvalidRequest):
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
	default:
		h.logger.Error("OIDC PAR request failed", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
	}
}
