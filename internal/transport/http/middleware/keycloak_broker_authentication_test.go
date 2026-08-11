package middleware

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
	"github.com/gin-gonic/gin"
)

func TestKeycloakBrokerJWTVerifierAcceptsOnlyBoundCurrentRealmToken(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/realms/basic-platform/protocol/openid-connect/certs" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []map[string]string{{"kid": "current", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes())}}})
	}))
	defer server.Close()
	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	token := signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": issuer, "sub": "keycloak-user", "aud": []string{"contract-prod-web"}, "exp": now.Add(time.Minute).Unix(), "iat": now.Add(-time.Minute).Unix(), "sid": "kc-session", "tenant_id": "tenant-1", "identity_id": "user-1"})

	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if claims.IdentityID != "user-1" || claims.TenantID != "tenant-1" || claims.SessionID != "kc-session" || len(claims.Audience) != 1 || claims.Audience[0] != "contract-prod-web" {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestKeycloakBrokerAuthenticationFailsClosed(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []map[string]string{{"kid": "current", "kty": "RSA", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes())}}})
	}))
	defer server.Close()
	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	valid := signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": issuer, "sub": "subject", "aud": "client", "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "sid": "sid", "tenant_id": "tenant", "identity_id": "identity"})
	for name, token := range map[string]string{"missing": "", "expired": signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": issuer, "sub": "subject", "aud": "client", "exp": now.Add(-time.Second).Unix(), "iat": now.Add(-time.Minute).Unix(), "sid": "sid", "tenant_id": "tenant", "identity_id": "identity"}), "wrong issuer": signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": "https://evil.example/realms/basic-platform", "sub": "subject", "aud": "client", "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "sid": "sid", "tenant_id": "tenant", "identity_id": "identity"}), "valid": valid} {
		t.Run(name, func(t *testing.T) {
			router := gin.New()
			router.POST("/", KeycloakBrokerAuthentication(verifier), func(c *gin.Context) {
				if _, ok := keycloakctx.BrokerClaimsFromContext(c.Request.Context()); !ok {
					t.Error("verified claims missing")
				}
				c.Status(http.StatusNoContent)
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/", nil)
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			router.ServeHTTP(recorder, request)
			want := http.StatusUnauthorized
			if name == "valid" {
				want = http.StatusNoContent
			}
			if recorder.Code != want {
				t.Fatalf("status = %d, want %d", recorder.Code, want)
			}
		})
	}
}

func signedKeycloakBrokerJWT(t *testing.T, privateKey *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encodedHeader, encodedPayload := base64.RawURLEncoding.EncodeToString(header), base64.RawURLEncoding.EncodeToString(payload)
	signingInput := encodedHeader + "." + encodedPayload
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}
