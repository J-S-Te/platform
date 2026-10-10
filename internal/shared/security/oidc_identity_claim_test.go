package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOIDCUserTokensEmitCanonicalIdentityAlias(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager := &OIDCJWTManager{issuer: "https://platform.example", keyID: "test-only", publicKey: public, privateKey: private}
	now := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	claims := OIDCTokenClaims{Subject: "identity-1", Audience: []string{"contract-client"},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), JWTID: "token-1", SessionID: "session-1",
		AuthenticationTime: now, Scope: []string{"openid"}, ClientID: "contract-client",
		Nonce: "nonce-1", TenantID: "tenant-1", Roles: []string{"admin"}}
	for _, use := range []OIDCTokenUse{OIDCTokenUseAccessToken, OIDCTokenUseIDToken} {
		t.Run(string(use), func(t *testing.T) {
			token, err := manager.issue(claims, use)
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(token, ".")
			raw, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err = json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["identity_id"] != "identity-1" || payload["sub"] != payload["identity_id"] {
				t.Fatal("signed identity alias must equal canonical platform subject")
			}
			if _, err = manager.Verify(token, "contract-client", use, now); err != nil {
				t.Fatal(err)
			}
			for _, alias := range []string{"", "other-identity", " identity-1"} {
				if alias == "" {
					delete(payload, "identity_id")
				} else {
					payload["identity_id"] = alias
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				body := parts[0] + "." + base64.RawURLEncoding.EncodeToString(encoded)
				signed := body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(body)))
				_, err = manager.Verify(signed, "contract-client", use, now)
				if (alias == "") != (err == nil) {
					t.Fatalf("alias compatibility/mismatch policy violated for %q", alias)
				}
			}
		})
	}
}
