package bootstrap

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

	platformmiddleware "github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// 平台按 identity_id 对齐 Keycloak 用户，Keycloak 的 sub 是 Keycloak 内部用户 ID，
// 只作为外部资源句柄。二者在常规部署下本来就不同（Keycloak UUID vs 平台 ULID）。
// 登录回调曾经要求 identity_id == sub，于是正常投影用户全部被拒绝登录。
func TestOIDCIDTokenVerifierAdapterAcceptsDistinctKeycloakSubjectAndPlatformIdentity(t *testing.T) {
	privateKey := newBootstrapKeycloakRSAKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/realms/basic-platform/protocol/openid-connect/certs" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "current", "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
		}}})
	}))
	defer server.Close()
	issuer := server.URL + "/realms/basic-platform"
	verifier, err := platformmiddleware.NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakBrokerJWTVerifier: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	keycloakUserID := "8f14e45f-ceea-467a-9f0a-1b2c3d4e5f60"
	platformIdentityID := "01M22KQ73BYCNKVHTXZBM8APBM"
	token := signedBootstrapKeycloakIDToken(t, privateKey, "current", map[string]any{
		"iss": issuer, "sub": keycloakUserID, "aud": "basic-platform-console-web",
		"exp": now.Add(time.Minute).Unix(), "iat": now.Add(-time.Minute).Unix(),
		"sid": "keycloak-session", "tenant_id": "tenant-1", "identity_id": platformIdentityID,
		"nonce": "expected-nonce", "token_use": "id_token",
	})

	adapter := oidcIDTokenVerifierAdapter{verifier: verifier}
	identityID, err := adapter.VerifyIDToken(context.Background(), token, "expected-nonce", "basic-platform-console-web")
	if err != nil {
		t.Fatalf("VerifyIDToken() error = %v; 不同标识的正常投影用户被拒绝登录", err)
	}
	if identityID != platformIdentityID {
		t.Fatalf("identityID = %q, want %q", identityID, platformIdentityID)
	}
}

func TestOIDCIDTokenVerifierAdapterStillRejectsMissingIdentityClaims(t *testing.T) {
	privateKey := newBootstrapKeycloakRSAKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "current", "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
		}}})
	}))
	defer server.Close()
	issuer := server.URL + "/realms/basic-platform"
	verifier, err := platformmiddleware.NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatalf("NewKeycloakBrokerJWTVerifier: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)

	for name, identity := range map[string]string{"缺少 identity_id": "", "缺少 sub": ""} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{
				"iss": issuer, "sub": "keycloak-user", "aud": "basic-platform-console-web",
				"exp": now.Add(time.Minute).Unix(), "iat": now.Add(-time.Minute).Unix(),
				"sid": "keycloak-session", "tenant_id": "tenant-1", "identity_id": "01M22KQ73BYCNKVHTXZBM8APBM",
				"nonce": "expected-nonce", "token_use": "id_token",
			}
			if identity == "" {
				if name == "缺少 sub" {
					claims["sub"] = ""
				} else {
					claims["identity_id"] = ""
				}
			}
			token := signedBootstrapKeycloakIDToken(t, privateKey, "current", claims)
			adapter := oidcIDTokenVerifierAdapter{verifier: verifier}
			if _, err := adapter.VerifyIDToken(context.Background(), token, "expected-nonce", "basic-platform-console-web"); err == nil {
				t.Fatalf("%s 的 ID Token 被接受", name)
			}
		})
	}
}

func newBootstrapKeycloakRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return privateKey
}

func signedBootstrapKeycloakIDToken(t *testing.T, privateKey *rsa.PrivateKey, keyID string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": keyID, "typ": "JWT"})
	if err != nil {
		t.Fatalf("encode header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("encode claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}
