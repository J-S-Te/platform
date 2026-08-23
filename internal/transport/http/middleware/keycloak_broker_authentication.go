package middleware

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
	"github.com/gin-gonic/gin"
)

const (
	maxKeycloakBrokerJWTLength = 16 * 1024

	// Keycloak JWKS normally changes only during signing-key rotation. Keep the
	// normal cache relatively short so rotations converge quickly, and permit a
	// much shorter stale window only for a previously validated keyset. The
	// stale window is deliberately not an authentication bypass: a token still
	// has to be signed by a known cached key and pass all regular claim checks.
	defaultKeycloakJWKSCacheTTL = 5 * time.Minute
	defaultKeycloakJWKSMaxStale = time.Minute
)

// KeycloakBrokerJWTVerifier verifies only end-user JWTs issued by the one
// configured Keycloak realm. It deliberately does not provide a reusable
// platform authentication mechanism.
type KeycloakBrokerJWTVerifier struct {
	issuer   string
	jwksURL  string
	client   *http.Client
	now      func() time.Time
	cacheTTL time.Duration
	maxStale time.Duration
	cache    keycloakJWKSCache
}

// KeycloakAuthorizationAccessTokenClaims is the strict, application-bound
// token view used by the authorization-context endpoint. Broker verification
// keeps its narrower compatibility contract, while authorization lookup must
// additionally prove that the token is an access token whose authorized party
// is present in the audience of the calling Client.
type KeycloakAuthorizationAccessTokenClaims struct {
	keycloakctx.BrokerClaims
	TokenUse string
}

// keycloakJWKSCache coalesces concurrent refreshes without holding its mutex
// over a network request. A completed fetch replaces the map instead of
// mutating it, so keys returned to concurrent verifications remain immutable.
type keycloakJWKSCache struct {
	mu         sync.Mutex
	keys       map[string]*rsa.PublicKey
	freshUntil time.Time
	staleUntil time.Time
	refreshing bool
	done       chan struct{}
}

func NewKeycloakBrokerJWTVerifier(issuer string, client *http.Client) (*KeycloakBrokerJWTVerifier, error) {
	return NewKeycloakBrokerJWTVerifierWithBackchannel(issuer, issuer, client)
}

// NewKeycloakBrokerJWTVerifierWithBackchannel keeps the browser-visible issuer
// as the token trust boundary while fetching signing keys from a separately
// configured container-reachable issuer URL. This is required when the public
// Keycloak hostname resolves only from the user's browser (for example a
// localhost port published by Docker).
func NewKeycloakBrokerJWTVerifierWithBackchannel(issuer, backchannelIssuer string, client *http.Client) (*KeycloakBrokerJWTVerifier, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if !validKeycloakIssuerURL(issuer) {
		return nil, errors.New("Keycloak broker issuer must be an absolute URL without credentials, query or fragment")
	}
	backchannelIssuer = strings.TrimRight(strings.TrimSpace(backchannelIssuer), "/")
	if !validKeycloakIssuerURL(backchannelIssuer) {
		return nil, errors.New("Keycloak broker backchannel issuer must be an absolute URL without credentials, query or fragment")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &KeycloakBrokerJWTVerifier{
		issuer: issuer, jwksURL: backchannelIssuer + "/protocol/openid-connect/certs", client: client, now: time.Now,
		cacheTTL: defaultKeycloakJWKSCacheTTL, maxStale: defaultKeycloakJWKSMaxStale,
	}, nil
}

func validKeycloakIssuerURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

// KeycloakBrokerAuthentication accepts a bearer token only for the dedicated
// broker-verification route. Verification uses a bounded cache of successfully
// parsed Keycloak JWKS documents. A Keycloak outage therefore does not make
// every in-flight broker verification fail at once, but a cache never accepts
// an unknown key and cannot outlive its short stale allowance.
func KeycloakBrokerAuthentication(verifier *KeycloakBrokerJWTVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		if verifier == nil {
			c.Abort()
			httpresponse.WriteError(c.Writer, c.Request, http.StatusInternalServerError, httperror.Internal)
			return
		}
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			c.Abort()
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		claims, err := verifier.Verify(c.Request.Context(), token)
		if err != nil {
			c.Abort()
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		c.Request = c.Request.WithContext(keycloakctx.WithBrokerClaims(c.Request.Context(), claims))
		c.Next()
	}
}

func (verifier *KeycloakBrokerJWTVerifier) Verify(ctx context.Context, raw string) (keycloakctx.BrokerClaims, error) {
	if verifier == nil || len(raw) == 0 || len(raw) > maxKeycloakBrokerJWTLength {
		return keycloakctx.BrokerClaims{}, errors.New("invalid Keycloak broker JWT")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return keycloakctx.BrokerClaims{}, errors.New("invalid Keycloak broker JWT serialization")
	}
	var header struct {
		Algorithm string          `json:"alg"`
		KeyID     string          `json:"kid"`
		Critical  json.RawMessage `json:"crit"`
	}
	if err := decodeKeycloakJSONObject(parts[0], &header); err != nil || header.Algorithm != "RS256" || strings.TrimSpace(header.KeyID) == "" || len(header.Critical) != 0 {
		return keycloakctx.BrokerClaims{}, errors.New("unsupported Keycloak broker JWT header")
	}
	key, err := verifier.keyFor(ctx, header.KeyID)
	if err != nil {
		return keycloakctx.BrokerClaims{}, fmt.Errorf("fetch Keycloak JWKS: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || rsa.VerifyPKCS1v15(key, crypto.SHA256, sha256Digest(parts[0]+"."+parts[1]), signature) != nil {
		return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT signature is invalid")
	}
	var payload struct {
		Issuer          string          `json:"iss"`
		Subject         string          `json:"sub"`
		Audience        json.RawMessage `json:"aud"`
		ExpiresAt       json.RawMessage `json:"exp"`
		IssuedAt        json.RawMessage `json:"iat"`
		NotBefore       json.RawMessage `json:"nbf"`
		SessionID       string          `json:"sid"`
		TenantID        string          `json:"tenant_id"`
		IdentityID      string          `json:"identity_id"`
		AuthorizedParty string          `json:"azp"`
	}
	if err := decodeKeycloakJSONObject(parts[1], &payload); err != nil {
		return keycloakctx.BrokerClaims{}, errors.New("invalid Keycloak broker JWT claims")
	}
	// Keycloak's reserved `sub` remains the issuer-native subject. The platform
	// identity_id mapper is a separate, required claim used for authorization.
	payload.Subject = strings.TrimSpace(payload.Subject)
	payload.IdentityID = strings.TrimSpace(payload.IdentityID)
	audience, ok := keycloakAudience(payload.Audience)
	expiresAt, okExp := keycloakNumericDate(payload.ExpiresAt)
	issuedAt, okIat := keycloakNumericDate(payload.IssuedAt)
	if !ok || !okExp || !okIat || payload.Issuer != verifier.issuer || payload.Subject == "" || strings.TrimSpace(payload.SessionID) == "" || strings.TrimSpace(payload.TenantID) == "" || payload.IdentityID == "" {
		return keycloakctx.BrokerClaims{}, errors.New("required Keycloak broker JWT claims are invalid")
	}
	now := verifier.now().UTC()
	if !expiresAt.After(now) || issuedAt.After(now.Add(time.Minute)) {
		return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT is outside its valid time window")
	}
	if len(payload.NotBefore) != 0 {
		notBefore, valid := keycloakNumericDate(payload.NotBefore)
		if !valid || notBefore.After(now.Add(time.Minute)) {
			return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT is not yet valid")
		}
	}
	return keycloakctx.BrokerClaims{Issuer: payload.Issuer, Subject: payload.Subject, SessionID: payload.SessionID, TenantID: payload.TenantID, IdentityID: payload.IdentityID, AuthorizedParty: strings.TrimSpace(payload.AuthorizedParty), Audience: audience}, nil
}

// KeycloakIDTokenClaims is the strict ID-token view used by the platform's own
// OIDC login callback. The broker verifier authenticates the signature first;
// this layer additionally proves the token is an ID token bound to the
// platform's OIDC client with the expected nonce.
type KeycloakIDTokenClaims struct {
	keycloakctx.BrokerClaims
	Nonce string
}

// VerifyIDToken applies the additional token-purpose, nonce and Client binding
// checks required before an ID token can complete the platform's OIDC login.
// The signed payload is decoded again only after Verify has authenticated it.
func (verifier *KeycloakBrokerJWTVerifier) VerifyIDToken(ctx context.Context, raw, expectedNonce, expectedClientID string) (KeycloakIDTokenClaims, error) {
	claims, err := verifier.Verify(ctx, raw)
	if err != nil {
		return KeycloakIDTokenClaims{}, err
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return KeycloakIDTokenClaims{}, errors.New("invalid Keycloak ID token serialization")
	}
	var payload struct {
		Nonce    string `json:"nonce"`
		TokenUse string `json:"token_use"`
	}
	if err := decodeKeycloakJSONObject(parts[1], &payload); err != nil {
		return KeycloakIDTokenClaims{}, errors.New("invalid Keycloak ID token claims")
	}
	nonce := strings.TrimSpace(payload.Nonce)
	tokenUse := strings.TrimSpace(payload.TokenUse)
	clientID := strings.TrimSpace(expectedClientID)
	if tokenUse != "id_token" || clientID == "" || !keycloakAudienceContains(claims.Audience, clientID) {
		return KeycloakIDTokenClaims{}, errors.New("Keycloak ID token is not bound to the platform OIDC client")
	}
	if expectedNonce == "" || nonce == "" || nonce != expectedNonce {
		return KeycloakIDTokenClaims{}, errors.New("Keycloak ID token nonce does not match")
	}
	if claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID {
		return KeycloakIDTokenClaims{}, errors.New("Keycloak ID token authorized party does not match")
	}
	return KeycloakIDTokenClaims{BrokerClaims: claims, Nonce: nonce}, nil
}

// VerifyAuthorizationAccessToken applies the additional token-purpose and
// Client audience checks required before a Keycloak token can select an
// application authorization context. The signed payload is decoded again only
// after Verify has authenticated it; no unverified value reaches the caller.
func (verifier *KeycloakBrokerJWTVerifier) VerifyAuthorizationAccessToken(ctx context.Context, raw string) (KeycloakAuthorizationAccessTokenClaims, error) {
	claims, err := verifier.Verify(ctx, raw)
	if err != nil {
		return KeycloakAuthorizationAccessTokenClaims{}, err
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return KeycloakAuthorizationAccessTokenClaims{}, errors.New("invalid Keycloak access token serialization")
	}
	var payload struct {
		TokenUse string `json:"token_use"`
	}
	if err := decodeKeycloakJSONObject(parts[1], &payload); err != nil {
		return KeycloakAuthorizationAccessTokenClaims{}, errors.New("invalid Keycloak access token claims")
	}
	tokenUse := strings.TrimSpace(payload.TokenUse)
	clientID := strings.TrimSpace(claims.AuthorizedParty)
	if tokenUse != "access_token" || clientID == "" || !keycloakAudienceContains(claims.Audience, clientID) {
		return KeycloakAuthorizationAccessTokenClaims{}, errors.New("Keycloak access token is not bound to its authorized Client")
	}
	return KeycloakAuthorizationAccessTokenClaims{BrokerClaims: claims, TokenUse: tokenUse}, nil
}

// keyFor returns a validated Keycloak signing key. A cache hit is used only
// while fresh. A missing kid always forces one refresh, even when the cached
// set is fresh, which makes Keycloak signing-key rotation converge immediately.
// If that refresh fails, only an already-known key may be used, and only within
// the bounded stale window established by the last successful fetch.
func (verifier *KeycloakBrokerJWTVerifier) keyFor(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	if verifier == nil {
		return nil, errors.New("Keycloak broker verifier is not initialized")
	}
	for {
		now := verifier.now().UTC()
		verifier.cache.mu.Lock()
		key := verifier.cache.keys[keyID]
		if key != nil && now.Before(verifier.cache.freshUntil) {
			verifier.cache.mu.Unlock()
			return key, nil
		}
		if verifier.cache.refreshing {
			done := verifier.cache.done
			verifier.cache.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// Capture a stale key before releasing the lock. The cache replacement
		// below never mutates this map, so it remains safe to use on fetch error.
		staleKey := key
		staleUntil := verifier.cache.staleUntil
		verifier.cache.refreshing = true
		verifier.cache.done = make(chan struct{})
		done := verifier.cache.done
		verifier.cache.mu.Unlock()

		keys, err := verifier.fetchKeys(ctx)
		completedAt := verifier.now().UTC()

		verifier.cache.mu.Lock()
		if err == nil {
			verifier.cache.keys = keys
			verifier.cache.freshUntil = completedAt.Add(verifier.cacheTTL)
			verifier.cache.staleUntil = verifier.cache.freshUntil.Add(verifier.maxStale)
		}
		verifier.cache.refreshing = false
		close(done)
		verifier.cache.done = nil
		verifier.cache.mu.Unlock()

		if err != nil {
			if staleKey != nil && completedAt.Before(staleUntil) {
				return staleKey, nil
			}
			return nil, err
		}
		key = keys[keyID]
		if key == nil {
			return nil, errors.New("Keycloak broker JWT signing key is unknown")
		}
		return key, nil
	}
}

func (verifier *KeycloakBrokerJWTVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, verifier.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := verifier.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected JWKS status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var document struct {
		Keys []struct {
			KeyID     string `json:"kid"`
			KeyType   string `json:"kty"`
			Algorithm string `json:"alg"`
			Use       string `json:"use"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, item := range document.Keys {
		if item.KeyType != "RSA" || item.Algorithm != "RS256" || (item.Use != "" && item.Use != "sig") || item.KeyID == "" {
			continue
		}
		n, nErr := base64.RawURLEncoding.DecodeString(item.Modulus)
		e, eErr := base64.RawURLEncoding.DecodeString(item.Exponent)
		if nErr != nil || eErr != nil || len(n) < 256 || len(e) == 0 {
			continue
		}
		exponent := new(big.Int).SetBytes(e)
		if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64()%2 == 0 {
			continue
		}
		publicKey := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent.Int64())}
		if publicKey.N.BitLen() < 2048 || publicKey.N.Bit(0) == 0 {
			continue
		}
		if _, duplicate := keys[item.KeyID]; duplicate {
			return nil, errors.New("duplicate Keycloak JWKS kid")
		}
		keys[item.KeyID] = publicKey
	}
	if len(keys) == 0 {
		return nil, errors.New("Keycloak JWKS contains no acceptable signing keys")
	}
	return keys, nil
}

func decodeKeycloakJSONObject(part string, destination any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("JSON object required")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("unexpected JSON data")
	}
	return json.Unmarshal(decoded, destination)
}

func sha256Digest(value string) []byte { digest := sha256.Sum256([]byte(value)); return digest[:] }

func keycloakAudience(raw json.RawMessage) ([]string, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil && strings.TrimSpace(one) != "" {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil || len(many) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(many))
	for _, value := range many {
		if strings.TrimSpace(value) == "" {
			return nil, false
		}
		if _, exists := seen[value]; exists {
			return nil, false
		}
		seen[value] = struct{}{}
	}
	return many, true
}

func keycloakAudienceContains(audience []string, expected string) bool {
	for _, value := range audience {
		if value == expected {
			return true
		}
	}
	return false
}

func keycloakNumericDate(raw json.RawMessage) (time.Time, bool) {
	var seconds json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&seconds) != nil {
		return time.Time{}, false
	}
	value, err := seconds.Int64()
	if err != nil || value <= 0 {
		return time.Time{}, false
	}
	return time.Unix(value, 0).UTC(), true
}
