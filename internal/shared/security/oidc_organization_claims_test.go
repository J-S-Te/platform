package security

import (
	"testing"
	"time"
)

func TestValidateOIDCTokenClaimsUsesStableIdentityWithoutDataScopes(t *testing.T) {
	now := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	claims := OIDCTokenClaims{
		Issuer: "https://identity.example", Subject: "identity-1", Audience: []string{"crm-client"},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), JWTID: "token-1", SessionID: "session-1",
		AuthenticationTime: now, Scope: []string{"openid"}, ClientID: "crm-client",
		TokenUse: OIDCTokenUseIDToken, TenantID: "tenant-1", Roles: []string{"crm-user"},
	}
	if err := validateOIDCTokenClaims(claims, claims.Issuer, OIDCTokenUseIDToken); err != nil {
		t.Fatalf("valid compact claims rejected: %v", err)
	}
}
