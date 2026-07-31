package oidchttp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
)

// UserInfo returns claims for a verified, non-revoked end-user access token. It intentionally
// does not accept a bearer token in query or form parameters, which prevents accidental logging.
func (h *Handler) UserInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeUserInfoUnauthorized(w)
		return
	}
	rawToken, ok := bearerToken(r)
	if !ok {
		writeUserInfoUnauthorized(w)
		return
	}
	// The client_id below is decoded without trust only to select the expected audience. The JWT
	// manager verifies its signature and requires the signed client_id/audience before claims are used.
	expectedAudience, ok := unverifiedJWTClientID(rawToken)
	if !ok {
		writeUserInfoUnauthorized(w)
		return
	}
	claims, err := h.jwtManager.VerifyAccessToken(rawToken, expectedAudience, h.clock.Now().UTC())
	if err != nil {
		writeUserInfoUnauthorized(w)
		return
	}
	if claims.ClientID != expectedAudience || claims.Subject == "" || claims.SessionID == "" || !hasScope(claims.Scope, "openid") {
		writeUserInfoUnauthorized(w)
		return
	}
	if h.accessTokenSubjects == nil {
		h.logger.Error("OIDC UserInfo access-token subject resolver is not configured")
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	subject, err := h.accessTokenSubjects.ResolveAccessTokenSubject(r.Context(), claims.ClientID, claims.SessionID, claims.Subject)
	if err != nil || strings.TrimSpace(subject.TenantID) == "" || strings.TrimSpace(subject.OAuthClientID) == "" {
		if err != nil {
			h.logger.Error("OIDC access-token subject resolution failed", "error", err)
		}
		writeUserInfoUnauthorized(w)
		return
	}
	revoked, err := h.service.IsAccessTokenRevoked(r.Context(), subject.TenantID, rawToken)
	if err != nil {
		h.logger.Error("OIDC access-token revocation lookup failed", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if revoked {
		writeUserInfoUnauthorized(w)
		return
	}
	info, err := h.service.ResolveUserInfo(r.Context(), application.UserInfoInput{
		TenantID: subject.TenantID, OAuthClientID: subject.OAuthClientID, SessionID: claims.SessionID, UserID: claims.Subject, Scopes: claims.Scope,
	})
	if err != nil {
		if errors.Is(err, application.ErrInvalidToken) || errors.Is(err, application.ErrInvalidRequest) {
			writeUserInfoUnauthorized(w)
			return
		}
		h.logger.Error("OIDC UserInfo resolution failed", "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if h.authorizationResolver == nil {
		h.logger.Error("OIDC UserInfo authorization resolver is not configured")
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	authorization, err := h.authorizationResolver.ResolveOIDCAuthorization(r.Context(), subject.TenantID, claims.ClientID, claims.Subject)
	if err != nil || authorization.TenantID != subject.TenantID || authorization.AuthzRevision == 0 || strings.TrimSpace(authorization.RoleConfigHash) == "" {
		if err != nil {
			h.logger.Warn("OIDC UserInfo current authorization resolution failed", "error", err)
		}
		writeUserInfoUnauthorized(w)
		return
	}
	payload := map[string]any{
		"sub":              info.Subject,
		"tenant_id":        authorization.TenantID,
		"roles":            append([]string(nil), authorization.Roles...),
		"permissions":      append([]string(nil), authorization.Permissions...),
		"role_config_hash": authorization.RoleConfigHash,
		"authz_revision":   authorization.AuthzRevision,
	}
	if hasScope(claims.Scope, "profile") {
		if info.Name != "" {
			payload["name"] = info.Name
		}
		if info.PreferredUsername != "" {
			payload["preferred_username"] = info.PreferredUsername
		}
	}
	if hasScope(claims.Scope, "email") && info.Email != "" {
		payload["email"] = info.Email
	}
	writeJSON(w, http.StatusOK, payload, true)
}

func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return parts[1], true
}

func unverifiedJWTClientID(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var values struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(payload, &values); err != nil || !validProtocolParameter(values.ClientID, 255) {
		return "", false
	}
	return values.ClientID, true
}

func hasScope(scopes []string, wanted string) bool {
	for _, scope := range scopes {
		if scope == wanted {
			return true
		}
	}
	return false
}

func writeUserInfoUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="oauth2/userinfo", error="invalid_token"`)
	writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "")
}
