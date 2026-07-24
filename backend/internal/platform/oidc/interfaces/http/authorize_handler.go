package oidchttp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
)

type parsedAuthorizationRequest struct {
	input         application.AuthorizationInput
	requestURI    string
	requestObject string
	responseType  string
}

// Authorize implements the browser-facing authorization-code endpoint. It accepts normal
// parameters, a one-time PAR request_uri, or a signed JAR request object. It only redirects to a
// client URI after Service.Authorize has validated that URI against the client registration.
func (h *Handler) Authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAuthorizationError(w, http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	if r.URL == nil || len(r.URL.RawQuery) > maxAuthorizeRequestBytes {
		writeAuthorizationError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	parsed, err := parseAuthorizationRequest(r)
	if err != nil {
		writeAuthorizationError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	principal, ok := h.authenticateBrowserSession(r)
	if !ok {
		redirectToPlatformLogin(w, r)
		return
	}

	var input application.AuthorizationInput
	switch {
	case parsed.requestURI != "":
		input, err = h.service.ConsumePushedAuthorizationRequest(r.Context(), application.ConsumePushedAuthorizationRequestInput{
			ClientID: parsed.input.ClientID, RequestURI: parsed.requestURI, SessionID: principal.SessionID,
		})
	case parsed.requestObject != "":
		input, err = h.service.ResolveRequestObject(r.Context(), application.RequestObjectAuthorizationInput{
			AuthorizationInput: parsed.input, ResponseType: parsed.responseType, RequestObject: parsed.requestObject,
			RequestObjectAudience: h.authorizationEndpointAudience(),
		})
		input.SessionID = principal.SessionID
	default:
		input = parsed.input
		input.SessionID = principal.SessionID
	}
	if err != nil {
		writeAuthorizeServiceError(w, h.logger, err)
		return
	}
	result, err := h.service.Authorize(r.Context(), input)
	if err != nil {
		writeAuthorizeServiceError(w, h.logger, err)
		return
	}
	location, err := authorizationSuccessLocation(result)
	if err != nil {
		h.logger.Error("OIDC authorization service returned an unsafe redirect URI", "error", err)
		writeAuthorizationError(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	http.Redirect(w, r, location, http.StatusFound)
}

func (h *Handler) authorizationEndpointAudience() string {
	return strings.TrimRight(h.jwtManager.Issuer(), "/") + "/authorize"
}

func (h *Handler) authenticateBrowserSession(r *http.Request) (authctx.Principal, bool) {
	if h.sessionAuth == nil {
		return authctx.Principal{}, false
	}
	cookie, err := r.Cookie(h.cookie.name)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return authctx.Principal{}, false
	}
	principal, err := h.sessionAuth.Authenticate(r.Context(), cookie.Value)
	if err != nil || strings.TrimSpace(principal.SessionID) == "" {
		return authctx.Principal{}, false
	}
	return principal, true
}

func parseAuthorizationRequest(r *http.Request) (parsedAuthorizationRequest, error) {
	query := r.URL.Query()
	if err := requireAtMostOne(query, "response_type", "client_id", "redirect_uri", "scope", "state", "nonce", "prompt", "code_challenge", "code_challenge_method", "request", "request_uri"); err != nil {
		return parsedAuthorizationRequest{}, err
	}
	clientID := strings.TrimSpace(query.Get("client_id"))
	if !validProtocolParameter(clientID, 255) {
		return parsedAuthorizationRequest{}, errors.New("client_id is invalid")
	}
	requestURI, requestObject := strings.TrimSpace(query.Get("request_uri")), strings.TrimSpace(query.Get("request"))
	if requestURI != "" {
		if requestObject != "" || hasAnyParameter(query, "response_type", "redirect_uri", "scope", "state", "nonce", "prompt", "code_challenge", "code_challenge_method") {
			return parsedAuthorizationRequest{}, errors.New("request_uri cannot be combined with authorization parameters")
		}
		if len(requestURI) > 1024 {
			return parsedAuthorizationRequest{}, errors.New("request_uri is too long")
		}
		return parsedAuthorizationRequest{input: application.AuthorizationInput{ClientID: clientID}, requestURI: requestURI}, nil
	}

	responseType := strings.TrimSpace(query.Get("response_type"))
	if requestObject == "" && responseType != "code" {
		return parsedAuthorizationRequest{}, errors.New("response_type is invalid")
	}
	if requestObject != "" && responseType != "" && responseType != "code" {
		return parsedAuthorizationRequest{}, errors.New("response_type is invalid")
	}
	if len(requestObject) > maxAuthorizeRequestBytes {
		return parsedAuthorizationRequest{}, errors.New("request object is too long")
	}
	input, err := parseDirectAuthorizationInput(query, clientID)
	if err != nil {
		return parsedAuthorizationRequest{}, err
	}
	if requestObject != "" {
		return parsedAuthorizationRequest{input: input, requestObject: requestObject, responseType: "code"}, nil
	}
	if input.RedirectURI == "" {
		return parsedAuthorizationRequest{}, errors.New("redirect_uri is required")
	}
	return parsedAuthorizationRequest{input: input, responseType: "code"}, nil
}

func parseDirectAuthorizationInput(query url.Values, clientID string) (application.AuthorizationInput, error) {
	redirectURI := strings.TrimSpace(query.Get("redirect_uri"))
	if redirectURI != "" && !validAbsoluteRedirectURI(redirectURI) {
		return application.AuthorizationInput{}, errors.New("redirect_uri is invalid")
	}
	if err := validateScopeParameter(query.Get("scope")); err != nil {
		return application.AuthorizationInput{}, err
	}
	if err := validateTextParameter(query.Get("state"), 2048); err != nil {
		return application.AuthorizationInput{}, err
	}
	if err := validateTextParameter(query.Get("nonce"), 255); err != nil {
		return application.AuthorizationInput{}, err
	}
	if err := validatePrompt(query.Get("prompt")); err != nil {
		return application.AuthorizationInput{}, err
	}
	challenge, method := strings.TrimSpace(query.Get("code_challenge")), strings.TrimSpace(query.Get("code_challenge_method"))
	if (challenge == "") != (method == "") || (method != "" && method != "S256" && method != "plain") || !validPKCEChallenge(challenge) {
		return application.AuthorizationInput{}, errors.New("PKCE parameters are invalid")
	}
	return application.AuthorizationInput{
		ClientID: clientID, RedirectURI: redirectURI, Scopes: strings.Fields(query.Get("scope")), State: query.Get("state"),
		Nonce: query.Get("nonce"), CodeChallenge: challenge, CodeChallengeMethod: method,
	}, nil
}

func hasAnyParameter(values url.Values, names ...string) bool {
	for _, name := range names {
		if _, present := values[name]; present {
			return true
		}
	}
	return false
}

func authorizationSuccessLocation(result application.AuthorizationResult) (string, error) {
	redirectURI, err := url.Parse(result.RedirectURI)
	if err != nil || redirectURI.Scheme == "" || redirectURI.Host == "" || redirectURI.User != nil || redirectURI.Fragment != "" {
		return "", errors.New("registered redirect URI is not a safe absolute URI")
	}
	if strings.TrimSpace(result.AuthorizationCode) == "" {
		return "", errors.New("authorization result did not contain a code")
	}
	query := redirectURI.Query()
	query.Set("code", result.AuthorizationCode)
	if result.State != "" {
		query.Set("state", result.State)
	}
	redirectURI.RawQuery = query.Encode()
	return redirectURI.String(), nil
}

func redirectToPlatformLogin(w http.ResponseWriter, r *http.Request) {
	returnTo := safeReturnTo(r)
	location := "/login.html?" + url.Values{"return_to": []string{returnTo}}.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	http.Redirect(w, r, location, http.StatusFound)
}

func safeReturnTo(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/authorize"
	}
	path := r.URL.EscapedPath()
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return "/authorize"
	}
	if r.URL.RawQuery == "" {
		return path
	}
	return path + "?" + r.URL.RawQuery
}

func writeAuthorizeServiceError(w http.ResponseWriter, logger interface{ Error(string, ...any) }, err error) {
	switch {
	case errors.Is(err, application.ErrInvalidRequest):
		writeAuthorizationError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, application.ErrInvalidScope):
		writeAuthorizationError(w, http.StatusBadRequest, "invalid_scope")
	case errors.Is(err, application.ErrInvalidClient), errors.Is(err, application.ErrUnauthorizedClient):
		writeAuthorizationError(w, http.StatusBadRequest, "unauthorized_client")
	case errors.Is(err, application.ErrInvalidGrant):
		// The redirect URI was not proven safe because authorization did not succeed.
		writeAuthorizationError(w, http.StatusBadRequest, "invalid_request")
	default:
		logger.Error("OIDC authorization failed", "error", err)
		writeAuthorizationError(w, http.StatusInternalServerError, "server_error")
	}
}

func writeAuthorizationError(w http.ResponseWriter, status int, code string) {
	writeOAuthError(w, status, code, "")
}

func validAbsoluteRedirectURI(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme != "" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func requireAtMostOne(values url.Values, names ...string) error {
	for _, name := range names {
		if len(values[name]) > 1 {
			return fmt.Errorf("multiple %s parameters", name)
		}
	}
	return nil
}
