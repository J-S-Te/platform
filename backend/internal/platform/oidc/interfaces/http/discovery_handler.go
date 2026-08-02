package oidchttp

import (
	"net/http"
	"strings"
)

// Discovery serves the provider metadata required by OpenID Connect relying parties.
func (h *Handler) Discovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	issuer := strings.TrimRight(h.jwtManager.Issuer(), "/")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                      issuer,
		"authorization_endpoint":                      issuer + "/authorize",
		"token_endpoint":                              issuer + "/oauth2/token",
		"pushed_authorization_request_endpoint":       issuer + "/oauth2/par",
		"userinfo_endpoint":                           issuer + "/oauth2/userinfo",
		"jwks_uri":                                    issuer + "/oauth2/jwks",
		"revocation_endpoint":                         issuer + "/oauth2/revoke",
		"end_session_endpoint":                        issuer + "/oauth2/logout",
		"response_types_supported":                    []string{"code"},
		"grant_types_supported":                       []string{"authorization_code", "refresh_token", "client_credentials"},
		"subject_types_supported":                     []string{"public"},
		"id_token_signing_alg_values_supported":       []string{"EdDSA"},
		"token_endpoint_auth_methods_supported":       []string{"client_secret_basic", "private_key_jwt", "none"},
		"request_parameter_supported":                 true,
		"request_uri_parameter_supported":             true,
		"require_pushed_authorization_requests":       false,
		"request_object_signing_alg_values_supported": []string{"EdDSA", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"},
		"code_challenge_methods_supported":            []string{"S256"},
		"scopes_supported":                            []string{"openid", "profile", "email"},
		"claims_supported": []string{
			"sub", "name", "preferred_username", "email",
			"tenant_id", "person_id", "primary_org_id", "organization_ids", "roles", "permissions",
			"role_config_hash", "authz_revision",
		},
	}, false)
}

// JWKS serves only the OIDC manager's public signing material.
func (h *Handler) JWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, h.jwtManager.JWKS(), false)
}
