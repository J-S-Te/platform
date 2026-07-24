package security

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const allowedClockSkew = time.Minute

// TokenClaims is the server-side JWT claim set used for browser session cookies.
type TokenClaims struct {
	SessionID string
	UserID    string
	TenantID  string
	AccountID string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type jwtPayload struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	SessionID string `json:"sid"`
	Subject   string `json:"sub"`
	TenantID  string `json:"tid"`
	AccountID string `json:"aid"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// JWTManager signs and verifies Ed25519 JWTs. It accepts only the EdDSA algorithm and validates
// issuer, audience and required session identity claims.
type JWTManager struct {
	issuer     string
	audience   string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// LoadJWTManager loads matching PKCS#8 Ed25519 private and PKIX Ed25519 public keys from files.
func LoadJWTManager(issuer, audience, privateKeyPath, publicKeyPath string) (*JWTManager, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return nil, errors.New("JWT issuer and audience must not be empty")
	}
	privateKey, err := loadEd25519PrivateKey(privateKeyPath)
	if err != nil {
		return nil, err
	}
	publicKey, err := loadEd25519PublicKey(publicKeyPath)
	if err != nil {
		return nil, err
	}
	if !privateKey.Public().(ed25519.PublicKey).Equal(publicKey) {
		return nil, errors.New("JWT private key does not match JWT public key")
	}
	return &JWTManager{issuer: issuer, audience: audience, privateKey: privateKey, publicKey: publicKey}, nil
}

// Issue creates a signed JWT from the supplied session claims.
func (manager *JWTManager) Issue(claims TokenClaims) (string, error) {
	if err := validateClaims(claims); err != nil {
		return "", err
	}

	header, err := json.Marshal(jwtHeader{Algorithm: "EdDSA", Type: "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal JWT header: %w", err)
	}
	payload, err := json.Marshal(jwtPayload{
		Issuer: manager.issuer, Audience: manager.audience, SessionID: claims.SessionID,
		Subject: claims.UserID, TenantID: claims.TenantID, AccountID: claims.AccountID,
		IssuedAt: claims.IssuedAt.UTC().Unix(), ExpiresAt: claims.ExpiresAt.UTC().Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal JWT payload: %w", err)
	}

	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := encodedHeader + "." + encodedPayload
	signature := ed25519.Sign(manager.privateKey, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verify validates and returns a token's required session claims at the supplied time.
func (manager *JWTManager) Verify(token string, now time.Time) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || anyEmpty(parts) {
		return TokenClaims{}, errors.New("JWT has an invalid compact serialization")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return TokenClaims{}, fmt.Errorf("decode JWT header: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return TokenClaims{}, fmt.Errorf("decode JWT header JSON: %w", err)
	}
	if header.Algorithm != "EdDSA" || header.Type != "JWT" {
		return TokenClaims{}, errors.New("JWT uses an unsupported header")
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return TokenClaims{}, errors.New("JWT has an invalid signature encoding")
	}
	if !ed25519.Verify(manager.publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return TokenClaims{}, errors.New("JWT signature verification failed")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var payload jwtPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return TokenClaims{}, fmt.Errorf("decode JWT payload JSON: %w", err)
	}
	if payload.Issuer != manager.issuer || payload.Audience != manager.audience {
		return TokenClaims{}, errors.New("JWT issuer or audience does not match")
	}

	claims := TokenClaims{
		SessionID: payload.SessionID, UserID: payload.Subject, TenantID: payload.TenantID,
		AccountID: payload.AccountID, IssuedAt: time.Unix(payload.IssuedAt, 0).UTC(),
		ExpiresAt: time.Unix(payload.ExpiresAt, 0).UTC(),
	}
	if err := validateClaims(claims); err != nil {
		return TokenClaims{}, err
	}
	now = now.UTC()
	if !claims.ExpiresAt.After(now) {
		return TokenClaims{}, errors.New("JWT has expired")
	}
	if claims.IssuedAt.After(now.Add(allowedClockSkew)) {
		return TokenClaims{}, errors.New("JWT issued_at is in the future")
	}
	return claims, nil
}

func loadEd25519PrivateKey(path string) (ed25519.PrivateKey, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("AUTH_JWT_PRIVATE_KEY_PATH must not be empty")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read JWT private key: %w", err)
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return nil, errors.New("JWT private key is not PEM encoded")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse JWT private key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("JWT private key must be an Ed25519 PKCS#8 key")
	}
	return key, nil
}

func loadEd25519PublicKey(path string) (ed25519.PublicKey, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("AUTH_JWT_PUBLIC_KEY_PATH must not be empty")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read JWT public key: %w", err)
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return nil, errors.New("JWT public key is not PEM encoded")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse JWT public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("JWT public key must be an Ed25519 PKIX key")
	}
	return key, nil
}

func validateClaims(claims TokenClaims) error {
	if strings.TrimSpace(claims.SessionID) == "" || strings.TrimSpace(claims.UserID) == "" ||
		strings.TrimSpace(claims.TenantID) == "" || strings.TrimSpace(claims.AccountID) == "" {
		return errors.New("JWT contains an empty required claim")
	}
	if claims.IssuedAt.IsZero() || claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(claims.IssuedAt) {
		return errors.New("JWT has invalid issued_at or expires_at claims")
	}
	return nil
}

func anyEmpty(parts []string) bool {
	for _, part := range parts {
		if part == "" {
			return true
		}
	}
	return false
}
