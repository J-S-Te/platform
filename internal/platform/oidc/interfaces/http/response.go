package oidchttp

import (
	"encoding/json"
	"net/http"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
)

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	setNoStoreHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	payload := map[string]string{"error": code}
	if description != "" {
		payload["error_description"] = description
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func writeInvalidClient(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="oauth2/token"`)
	writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
}

func writeTokenResult(w http.ResponseWriter, result application.TokenResult) {
	payload := map[string]any{
		"access_token": result.AccessToken,
		"token_type":   result.TokenType,
		"expires_in":   result.ExpiresIn,
		"scope":        result.Scope,
	}
	if result.IDToken != "" {
		payload["id_token"] = result.IDToken
	}
	if result.RefreshToken != "" {
		payload["refresh_token"] = result.RefreshToken
	}
	writeJSON(w, http.StatusOK, payload, true)
}

func writeLegacyTokenResult(w http.ResponseWriter, result applicationregistryapplication.TokenResult) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": result.AccessToken,
		"token_type":   "Bearer",
		"expires_in":   result.ExpiresIn,
		"scope":        result.Scope,
	}, true)
}

func writeJSON(w http.ResponseWriter, status int, value any, noStore bool) {
	if noStore {
		setNoStoreHeaders(w)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func setNoStoreHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}
