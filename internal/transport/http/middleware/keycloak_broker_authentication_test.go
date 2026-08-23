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
	"sync"
	"sync/atomic"
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
	token := signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": issuer, "sub": "user-1", "aud": []string{"contract-prod-web"}, "exp": now.Add(time.Minute).Unix(), "iat": now.Add(-time.Minute).Unix(), "sid": "kc-session", "tenant_id": "tenant-1", "identity_id": "user-1"})

	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if claims.IdentityID != "user-1" || claims.TenantID != "tenant-1" || claims.SessionID != "kc-session" || len(claims.Audience) != 1 || claims.Audience[0] != "contract-prod-web" {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestKeycloakBrokerJWTVerifierFetchesJWKSFromBackchannelAndKeepsPublicIssuer(t *testing.T) {
	privateKey := newTestKeycloakRSAKey(t)
	var requestedPath string
	backchannel := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPath = request.URL.Path
		writeKeycloakJWKS(t, writer, map[string]*rsa.PrivateKey{"current": privateKey})
	}))
	defer backchannel.Close()

	issuer := "http://localhost:18090/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifierWithBackchannel(issuer, backchannel.URL+"/realms/basic-platform", backchannel.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	token := signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{
		"iss": issuer, "sub": "user-1", "aud": "customer_and_opportunity-dev-web",
		"exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "sid": "session-1",
		"tenant_id": "tenant-1", "identity_id": "user-1",
	})

	if _, err = verifier.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if requestedPath != "/realms/basic-platform/protocol/openid-connect/certs" {
		t.Fatalf("JWKS request path = %q", requestedPath)
	}
}

func TestKeycloakBrokerJWTVerifierRejectsInvalidBackchannelIssuer(t *testing.T) {
	if _, err := NewKeycloakBrokerJWTVerifierWithBackchannel("https://sso.example/realms/basic-platform", "http://keycloak:8080/realms/basic-platform?target=other", nil); err == nil {
		t.Fatal("invalid backchannel issuer was accepted")
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
	valid := signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": issuer, "sub": "subject", "aud": "client", "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "sid": "sid", "tenant_id": "tenant", "identity_id": "subject"})
	for name, token := range map[string]string{"missing": "", "expired": signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": issuer, "sub": "subject", "aud": "client", "exp": now.Add(-time.Second).Unix(), "iat": now.Add(-time.Minute).Unix(), "sid": "sid", "tenant_id": "tenant", "identity_id": "subject"}), "wrong issuer": signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": "https://evil.example/realms/basic-platform", "sub": "subject", "aud": "client", "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "sid": "sid", "tenant_id": "tenant", "identity_id": "subject"}), "identity mismatch": signedKeycloakBrokerJWT(t, privateKey, "current", map[string]any{"iss": issuer, "sub": "subject", "aud": "client", "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "sid": "sid", "tenant_id": "tenant", "identity_id": "other-subject"}), "valid": valid} {
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
			if name == "valid" || name == "identity mismatch" {
				want = http.StatusNoContent
			}
			if recorder.Code != want {
				t.Fatalf("status = %d, want %d", recorder.Code, want)
			}
		})
	}
}

func TestKeycloakBrokerJWTVerifierRequiresBoundAccessTokenForAuthorizationContext(t *testing.T) {
	privateKey := newTestKeycloakRSAKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeKeycloakJWKS(t, writer, map[string]*rsa.PrivateKey{"current": privateKey})
	}))
	defer server.Close()

	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	base := validKeycloakBrokerClaims(issuer, now)
	base["identity_id"] = "subject"

	tests := []struct {
		name     string
		tokenUse string
		azp      string
		audience any
		wantOK   bool
	}{
		{name: "bound access token", tokenUse: "access_token", azp: "contract-prod-web", audience: []string{"account", "contract-prod-web"}, wantOK: true},
		{name: "ID token", tokenUse: "id_token", azp: "contract-prod-web", audience: "contract-prod-web"},
		{name: "missing token use", azp: "contract-prod-web", audience: "contract-prod-web"},
		{name: "missing authorized party", tokenUse: "access_token", audience: "contract-prod-web"},
		{name: "audience does not contain authorized party", tokenUse: "access_token", azp: "contract-prod-web", audience: []string{"account"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := make(map[string]any, len(base)+3)
			for key, value := range base {
				claims[key] = value
			}
			claims["aud"] = test.audience
			if test.tokenUse != "" {
				claims["token_use"] = test.tokenUse
			}
			if test.azp != "" {
				claims["azp"] = test.azp
			}
			token := signedKeycloakBrokerJWT(t, privateKey, "current", claims)
			got, verifyErr := verifier.VerifyAuthorizationAccessToken(context.Background(), token)
			if test.wantOK {
				if verifyErr != nil {
					t.Fatalf("VerifyAuthorizationAccessToken() error = %v", verifyErr)
				}
				if got.TokenUse != "access_token" || got.AuthorizedParty != test.azp {
					t.Fatalf("claims = %#v", got)
				}
				return
			}
			if verifyErr == nil {
				t.Fatalf("VerifyAuthorizationAccessToken() claims = %#v, want error", got)
			}
		})
	}
}

func TestKeycloakBrokerJWTVerifierRequiresBoundIDTokenWithNonce(t *testing.T) {
	privateKey := newTestKeycloakRSAKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeKeycloakJWKS(t, writer, map[string]*rsa.PrivateKey{"current": privateKey})
	}))
	defer server.Close()

	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	base := validKeycloakBrokerClaims(issuer, now)
	base["identity_id"] = "subject"
	base["azp"] = "platform-console-web"

	tests := []struct {
		name     string
		tokenUse string
		nonce    string
		expected string
		audience any
		wantOK   bool
	}{
		{name: "bound id token with nonce", tokenUse: "id_token", nonce: "nonce-1", expected: "nonce-1", audience: "platform-console-web", wantOK: true},
		{name: "nonce mismatch", tokenUse: "id_token", nonce: "nonce-1", expected: "nonce-2", audience: "platform-console-web"},
		{name: "missing nonce", tokenUse: "id_token", expected: "nonce-1", audience: "platform-console-web"},
		{name: "wrong token use", tokenUse: "access_token", nonce: "nonce-1", expected: "nonce-1", audience: "platform-console-web"},
		{name: "audience missing client", tokenUse: "id_token", nonce: "nonce-1", expected: "nonce-1", audience: []string{"account"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := make(map[string]any, len(base)+3)
			for key, value := range base {
				claims[key] = value
			}
			claims["aud"] = test.audience
			claims["token_use"] = test.tokenUse
			if test.nonce != "" {
				claims["nonce"] = test.nonce
			}
			token := signedKeycloakBrokerJWT(t, privateKey, "current", claims)
			got, verifyErr := verifier.VerifyIDToken(context.Background(), token, test.expected, "platform-console-web")
			if test.wantOK {
				if verifyErr != nil {
					t.Fatalf("VerifyIDToken() error = %v", verifyErr)
				}
				if got.Nonce != "nonce-1" || got.IdentityID != "subject" {
					t.Fatalf("claims = %#v", got)
				}
				return
			}
			if verifyErr == nil {
				t.Fatalf("VerifyIDToken() claims = %#v, want error", got)
			}
		})
	}
}

func TestKeycloakBrokerJWTVerifierCachesFreshJWKS(t *testing.T) {
	privateKey := newTestKeycloakRSAKey(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writeKeycloakJWKS(t, writer, map[string]*rsa.PrivateKey{"current": privateKey})
	}))
	defer server.Close()

	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	token := signedKeycloakBrokerJWT(t, privateKey, "current", validKeycloakBrokerClaims(issuer, now))
	for range 2 {
		if _, err := verifier.Verify(context.Background(), token); err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("JWKS requests = %d, want 1 fresh-cache request", got)
	}
}

func TestKeycloakBrokerJWTVerifierRefreshesImmediatelyOnUnknownKid(t *testing.T) {
	firstKey, secondKey := newTestKeycloakRSAKey(t), newTestKeycloakRSAKey(t)
	var keys atomic.Value
	keys.Store(map[string]*rsa.PrivateKey{"first": firstKey})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writeKeycloakJWKS(t, writer, keys.Load().(map[string]*rsa.PrivateKey))
	}))
	defer server.Close()

	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	if _, err := verifier.Verify(context.Background(), signedKeycloakBrokerJWT(t, firstKey, "first", validKeycloakBrokerClaims(issuer, now))); err != nil {
		t.Fatalf("first Verify() error = %v", err)
	}
	keys.Store(map[string]*rsa.PrivateKey{"first": firstKey, "second": secondKey})
	if _, err := verifier.Verify(context.Background(), signedKeycloakBrokerJWT(t, secondKey, "second", validKeycloakBrokerClaims(issuer, now))); err != nil {
		t.Fatalf("rotated-key Verify() error = %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("JWKS requests = %d, want 2 after kid miss", got)
	}
}

func TestKeycloakBrokerJWTVerifierUsesOnlyBoundedStaleVerifiedKeyset(t *testing.T) {
	privateKey := newTestKeycloakRSAKey(t)
	var serveKeys atomic.Bool
	serveKeys.Store(true)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if !serveKeys.Load() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeKeycloakJWKS(t, writer, map[string]*rsa.PrivateKey{"current": privateKey})
	}))
	defer server.Close()

	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	now := base
	verifier.now = func() time.Time { return now }
	verifier.cacheTTL = time.Minute
	verifier.maxStale = 30 * time.Second
	claims := validKeycloakBrokerClaims(issuer, base)
	claims["exp"] = base.Add(10 * time.Minute).Unix()
	token := signedKeycloakBrokerJWT(t, privateKey, "current", claims)
	if _, err := verifier.Verify(context.Background(), token); err != nil {
		t.Fatalf("initial Verify() error = %v", err)
	}

	serveKeys.Store(false)
	now = base.Add(time.Minute + time.Second)
	if _, err := verifier.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify() should use bounded stale keyset, got %v", err)
	}
	now = base.Add(time.Minute + 31*time.Second)
	if _, err := verifier.Verify(context.Background(), token); err == nil {
		t.Fatal("Verify() succeeded after stale JWKS allowance elapsed")
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("JWKS requests = %d, want 3", got)
	}
}

func TestKeycloakBrokerJWTVerifierCoalescesConcurrentJWKSRefresh(t *testing.T) {
	privateKey := newTestKeycloakRSAKey(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		once.Do(func() { close(started) })
		<-release
		writeKeycloakJWKS(t, writer, map[string]*rsa.PrivateKey{"current": privateKey})
	}))
	defer server.Close()

	issuer := server.URL + "/realms/basic-platform"
	verifier, err := NewKeycloakBrokerJWTVerifier(issuer, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	verifier.now = func() time.Time { return now }
	token := signedKeycloakBrokerJWT(t, privateKey, "current", validKeycloakBrokerClaims(issuer, now))

	const callers = 8
	errorsByCaller := make(chan error, callers)
	for range callers {
		go func() { _, err := verifier.Verify(context.Background(), token); errorsByCaller <- err }()
	}
	<-started
	close(release)
	for range callers {
		if err := <-errorsByCaller; err != nil {
			t.Fatalf("concurrent Verify() error = %v", err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("JWKS requests = %d, want 1 coalesced refresh", got)
	}
}

func newTestKeycloakRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func validKeycloakBrokerClaims(issuer string, now time.Time) map[string]any {
	return map[string]any{
		"iss": issuer, "sub": "subject", "aud": "client", "exp": now.Add(time.Minute).Unix(), "iat": now.Add(-time.Second).Unix(),
		"sid": "sid", "tenant_id": "tenant", "identity_id": "subject",
	}
}

func writeKeycloakJWKS(t *testing.T, writer http.ResponseWriter, keys map[string]*rsa.PrivateKey) {
	t.Helper()
	document := make([]map[string]string, 0, len(keys))
	for keyID, privateKey := range keys {
		document = append(document, map[string]string{
			"kid": keyID, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
		})
	}
	if err := json.NewEncoder(writer).Encode(map[string]any{"keys": document}); err != nil {
		t.Errorf("write JWKS: %v", err)
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
