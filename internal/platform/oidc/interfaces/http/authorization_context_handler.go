package oidchttp

import (
	"net/http"
	"strings"
)

// AuthorizationContext returns the current, application-scoped authorization
// snapshot. It is intentionally online and short-cacheable: permissions and
// data scopes are not copied into OIDC tokens, so revocations do not wait for
// token expiry or require a large JWT.
func (h *Handler) AuthorizationContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	if h.authorizationContextResolver == nil || h.accessTokenSubjects == nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	rawToken, ok := bearerToken(r)
	if !ok {
		writeContextUnauthorized(w)
		return
	}
	expectedAudience, ok := unverifiedJWTClientID(rawToken)
	claims, err := h.jwtManager.VerifyAccessToken(rawToken, expectedAudience, h.clock.Now().UTC())
	clientID, subjectTenantID, subjectID := "", "", ""
	if ok && err == nil && claims.ClientID == expectedAudience && claims.Subject != "" && claims.SessionID != "" && hasScope(claims.Scope, "openid") {
		clientID, subjectID = claims.ClientID, claims.Subject
		subject, subjectErr := h.accessTokenSubjects.ResolveAccessTokenSubject(r.Context(), claims.ClientID, claims.SessionID, claims.Subject)
		if subjectErr != nil || strings.TrimSpace(subject.TenantID) == "" {
			writeContextUnauthorized(w)
			return
		}
		subjectTenantID = subject.TenantID
		revoked, revokeErr := h.service.IsAccessTokenRevoked(r.Context(), subject.TenantID, rawToken)
		if revokeErr != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
		if revoked {
			writeContextUnauthorized(w)
			return
		}
	} else if h.externalAuthorizationVerifier != nil {
		external, externalErr := h.externalAuthorizationVerifier.Verify(r.Context(), rawToken)
		if externalErr != nil || strings.TrimSpace(external.Subject) == "" || strings.TrimSpace(external.TenantID) == "" || len(external.Audience) == 0 {
			h.logger.Warn("external authorization context token rejected", "error", externalErr)
			writeContextUnauthorized(w)
			return
		}
		clientID = strings.TrimSpace(external.AuthorizedParty)
		if clientID == "" {
			clientID = external.Audience[0]
		}
		subjectID, subjectTenantID = external.Subject, external.TenantID
	} else {
		writeContextUnauthorized(w)
		return
	}
	context, err := h.authorizationContextResolver.ResolveOIDCAuthorizationContext(r.Context(), subjectTenantID, clientID, subjectID)
	if err != nil || context.TenantID != subjectTenantID {
		if err != nil {
			h.logger.Warn("OIDC authorization context resolution failed", "error", err, "client_id", clientID, "subject", subjectID)
		}
		writeContextUnauthorized(w)
		return
	}
	roles := append([]string(nil), context.Roles...)
	permissions := append([]string(nil), context.Permissions...)
	scopes := make([]map[string]string, 0, len(context.DataScopes))
	for _, scope := range context.DataScopes {
		scopes = append(scopes, map[string]string{"role_code": scope.RoleCode, "scope_type": scope.ScopeType, "scope_id": scope.ScopeID, "environment_code": scope.EnvironmentCode})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sub": subjectID, "identity_id": subjectID, "tenant_id": context.TenantID,
		"person_id": context.PersonID, "roles": roles, "permissions": permissions,
		"data_scopes": scopes, "authorization_revision": context.AuthorizationRevision,
	}, true)
}

func writeContextUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="oauth2/authorization-context", error="invalid_token"`)
	writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "")
}
