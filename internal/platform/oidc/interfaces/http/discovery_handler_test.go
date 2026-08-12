package oidchttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

type discoveryJWTManager struct{}

func (discoveryJWTManager) Issuer() string          { return "https://identity.example.com/" }
func (discoveryJWTManager) JWKS() security.OIDCJWKS { return security.OIDCJWKS{} }
func (discoveryJWTManager) VerifyAccessToken(string, string, time.Time) (security.OIDCTokenClaims, error) {
	return security.OIDCTokenClaims{}, nil
}
func (discoveryJWTManager) VerifyIDToken(string, string, time.Time) (security.OIDCTokenClaims, error) {
	return security.OIDCTokenClaims{}, nil
}

func TestDiscoveryAdvertisesActuallyIssuedAuthorizationClaims(t *testing.T) {
	handler := &Handler{jwtManager: discoveryJWTManager{}}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)

	handler.Discovery(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var document struct {
		Issuer                       string   `json:"issuer"`
		AuthorizationContextEndpoint string   `json:"authorization_context_endpoint"`
		ClaimsSupported              []string `json:"claims_supported"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Issuer != "https://identity.example.com" {
		t.Fatalf("issuer=%q", document.Issuer)
	}
	if document.AuthorizationContextEndpoint != "https://identity.example.com/oauth2/authorization-context" {
		t.Fatalf("authorization_context_endpoint=%q", document.AuthorizationContextEndpoint)
	}
	claims := make(map[string]bool, len(document.ClaimsSupported))
	for _, claim := range document.ClaimsSupported {
		if claims[claim] {
			t.Fatalf("duplicate claim %q", claim)
		}
		claims[claim] = true
	}
	for _, claim := range []string{
		"sub", "identity_id", "name", "preferred_username", "email", "tenant_id", "person_id", "roles",
	} {
		if !claims[claim] {
			t.Errorf("claims_supported does not contain %q", claim)
		}
	}
	for _, claim := range []string{"personnel_directory"} {
		if claims[claim] {
			t.Errorf("claims_supported must not advertise %q", claim)
		}
	}
	for _, claim := range []string{"organization_ids", "permissions", "role_config_hash", "authz_revision"} {
		if claims[claim] {
			t.Errorf("claims_supported must not advertise detailed authorization claim %q", claim)
		}
	}
}

func TestDiscoveryRejectsNonGET(t *testing.T) {
	handler := &Handler{jwtManager: discoveryJWTManager{}}
	response := httptest.NewRecorder()
	handler.Discovery(response, httptest.NewRequest(http.MethodPost, "/.well-known/openid-configuration", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}
