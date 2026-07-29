package oidchttp

import (
	"errors"
	"net/http"
	"strings"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
)

// Token dispatches the supported OAuth token grants. Client credentials remain delegated to the
// pre-existing issuer so clients keep their current access-token format and authorization policy.
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
	authentication, ok := h.tokenClientAuthentication(w, r, true, true)
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
	authentication, ok := h.tokenClientAuthentication(w, r, true, true)
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
	if h.legacyIssuer == nil {
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", "")
		return
	}
	authentication, ok := h.tokenClientAuthentication(w, r, false, false)
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

// tokenClientAuthentication only accepts HTTP Basic for confidential clients. Public end-user
// grants (authorization_code and refresh_token) may instead provide client_id in the form without
// a client_secret; client_credentials always requires HTTP Basic.
// tokenClientAuthentication supports client_secret_basic, public client_id form authentication, and
// private_key_jwt for OIDC user grants. The assertion audience is derived from trusted server
// configuration; the non-standard client_assertion_audience form value is rejected.
func (h *Handler) tokenClientAuthentication(w http.ResponseWriter, r *http.Request, allowPublicClientForm, allowClientAssertion bool) (application.ClientAuthentication, bool) {
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
