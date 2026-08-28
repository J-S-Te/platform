package oidchttp

import (
	"errors"
	"net/http"
	"strings"

	oidcapplication "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
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
	clientID, subjectTenantID, subjectID, tokenSubject, loginIP := "", "", "", "", ""
	if ok && err == nil && claims.ClientID == expectedAudience && claims.Subject != "" && claims.SessionID != "" && hasScope(claims.Scope, "openid") {
		if !h.allowLegacyPlatformAccessToken {
			writeContextUnauthorized(w)
			return
		}
		clientID, subjectID, tokenSubject = claims.ClientID, claims.Subject, claims.Subject
		subject, subjectErr := h.accessTokenSubjects.ResolveAccessTokenSubject(r.Context(), claims.ClientID, claims.SessionID, claims.Subject)
		if subjectErr != nil || strings.TrimSpace(subject.TenantID) == "" {
			writeContextUnauthorized(w)
			return
		}
		subjectTenantID = subject.TenantID
		loginIP = subject.LoginIP
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
		externalSubject := strings.TrimSpace(external.Subject)
		externalIdentityID := strings.TrimSpace(external.IdentityID)
		clientID = strings.TrimSpace(external.AuthorizedParty)
		if externalErr != nil || externalSubject == "" || externalIdentityID == "" || strings.TrimSpace(external.TenantID) == "" || external.TokenUse != "access_token" || clientID == "" || !containsExactAudience(external.Audience, clientID) {
			h.logger.Warn("external authorization context token rejected", "error", externalErr)
			writeContextUnauthorized(w)
			return
		}
		subjectID, subjectTenantID, tokenSubject = externalIdentityID, external.TenantID, externalSubject
	} else {
		writeContextUnauthorized(w)
		return
	}
	context, err := h.authorizationContextResolver.ResolveOIDCAuthorizationContext(r.Context(), subjectTenantID, clientID, subjectID)
	if err != nil {
		h.logger.Warn("OIDC authorization context resolution failed", "error", err, "client_id", clientID, "subject", subjectID)
		if errors.Is(err, oidcapplication.ErrAccessDenied) {
			writeOAuthError(w, http.StatusForbidden, "access_denied", "")
			return
		}
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if context.TenantID != subjectTenantID || strings.TrimSpace(context.ClientID) != clientID {
		writeContextUnauthorized(w)
		return
	}
	roles := append([]string(nil), context.Roles...)
	permissions := append([]string(nil), context.Permissions...)
	scopes := make([]map[string]string, 0, len(context.DataScopes))
	for _, scope := range context.DataScopes {
		scopes = append(scopes, map[string]string{"role_code": scope.RoleCode, "scope_type": scope.ScopeType, "scope_id": scope.ScopeID, "environment_code": scope.EnvironmentCode})
	}
	response := map[string]any{
		// sub 保留 Keycloak 签发方的原生主体；subject_id 是跨业务系统唯一允许
		// 持久化的平台注册主体。identity_id 在兼容期与 subject_id 双写。
		"sub": tokenSubject, "subject_id": subjectID, "identity_id": subjectID, "tenant_id": context.TenantID,
		"client_id": context.ClientID, "application_code": context.ApplicationCode, "environment_code": context.EnvironmentCode,
		"person_id": context.PersonID, "roles": roles, "permissions": permissions,
		"data_scopes": scopes, "authorization_revision": context.AuthorizationRevision,
		"catalog_version":               context.CatalogVersion,
		"compatible_catalog_versions":   append([]string(nil), context.CompatibleCatalogVersions...),
		"role_config_hash":              context.RoleConfigHash,
		"compatible_role_config_hashes": append([]string(nil), context.CompatibleRoleConfigHashes...),
	}
	if strings.TrimSpace(loginIP) != "" {
		response["user_login_ip"] = loginIP
	}
	// customer_ref 只在开关启用且解析到 ACTIVE 绑定时出现；解析失败一律省略声明并记日志，
	// 消费方按无绑定 fail closed，不因平台侧临时故障扩大为整个授权上下文 5xx。
	if h.emitCustomerRef {
		customerRef, refErr := h.customerBindingResolver.ResolveCustomerBinding(r.Context(), subjectTenantID, subjectID, context.ApplicationCode)
		if refErr == nil && customerRef != "" && customerRef == strings.TrimSpace(customerRef) && len(customerRef) <= 64 {
			response["customer_ref"] = customerRef
		} else if refErr != nil {
			h.logger.Warn("customer binding resolution failed; customer_ref claim omitted", "error", refErr, "client_id", clientID, "subject", subjectID)
		}
	}
	writeJSON(w, http.StatusOK, response, true)
}

func containsExactAudience(audience []string, expected string) bool {
	for _, value := range audience {
		if value == expected {
			return true
		}
	}
	return false
}

func writeContextUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="oauth2/authorization-context", error="invalid_token"`)
	writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "")
}
