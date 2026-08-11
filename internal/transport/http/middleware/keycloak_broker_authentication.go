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
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
	"github.com/gin-gonic/gin"
)

const maxKeycloakBrokerJWTLength = 16 * 1024

// KeycloakBrokerJWTVerifier verifies only end-user JWTs issued by the one
// configured Keycloak realm. It deliberately does not provide a reusable
// platform authentication mechanism.
type KeycloakBrokerJWTVerifier struct {
	issuer  string
	jwksURL string
	client  *http.Client
	now     func() time.Time
}

func NewKeycloakBrokerJWTVerifier(issuer string, client *http.Client) (*KeycloakBrokerJWTVerifier, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	parsed, err := url.ParseRequestURI(issuer)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Keycloak broker issuer must be an absolute URL without credentials, query or fragment")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &KeycloakBrokerJWTVerifier{
		issuer: issuer, jwksURL: issuer + "/protocol/openid-connect/certs", client: client, now: time.Now,
	}, nil
}

// KeycloakBrokerAuthentication accepts a bearer token only for the dedicated
// broker-verification route. A JWKS request is made for every verification so
// a Keycloak outage fails this broker evidence flow closed rather than turning
// an old local key cache into an implicit authentication source.
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
	keys, err := verifier.fetchKeys(ctx)
	if err != nil {
		return keycloakctx.BrokerClaims{}, fmt.Errorf("fetch Keycloak JWKS: %w", err)
	}
	key, ok := keys[header.KeyID]
	if !ok {
		return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT signing key is unknown")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || rsa.VerifyPKCS1v15(key, crypto.SHA256, sha256Digest(parts[0]+"."+parts[1]), signature) != nil {
		return keycloakctx.BrokerClaims{}, errors.New("Keycloak broker JWT signature is invalid")
	}
	var payload struct {
		Issuer     string          `json:"iss"`
		Subject    string          `json:"sub"`
		Audience   json.RawMessage `json:"aud"`
		ExpiresAt  json.RawMessage `json:"exp"`
		IssuedAt   json.RawMessage `json:"iat"`
		NotBefore  json.RawMessage `json:"nbf"`
		SessionID  string          `json:"sid"`
		TenantID   string          `json:"tenant_id"`
		IdentityID string          `json:"identity_id"`
	}
	if err := decodeKeycloakJSONObject(parts[1], &payload); err != nil {
		return keycloakctx.BrokerClaims{}, errors.New("invalid Keycloak broker JWT claims")
	}
	audience, ok := keycloakAudience(payload.Audience)
	expiresAt, okExp := keycloakNumericDate(payload.ExpiresAt)
	issuedAt, okIat := keycloakNumericDate(payload.IssuedAt)
	if !ok || !okExp || !okIat || payload.Issuer != verifier.issuer || strings.TrimSpace(payload.Subject) == "" || strings.TrimSpace(payload.SessionID) == "" || strings.TrimSpace(payload.TenantID) == "" || strings.TrimSpace(payload.IdentityID) == "" {
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
	return keycloakctx.BrokerClaims{Issuer: payload.Issuer, Subject: payload.Subject, SessionID: payload.SessionID, TenantID: payload.TenantID, IdentityID: payload.IdentityID, Audience: audience}, nil
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
