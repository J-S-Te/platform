package security

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const OIDCBackchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

// OIDCLogoutTokenClaims 是 Back-Channel Logout 专用声明，不复用 ID Token 声明模型。
type OIDCLogoutTokenClaims struct {
	Issuer    string
	Subject   string
	Audience  []string
	IssuedAt  time.Time
	ExpiresAt time.Time
	JWTID     string
	SessionID string
	Events    map[string]any
}

type oidcLogoutPayload struct {
	Issuer    string         `json:"iss"`
	Subject   string         `json:"sub,omitempty"`
	Audience  oidcAudience   `json:"aud"`
	IssuedAt  int64          `json:"iat"`
	ExpiresAt int64          `json:"exp"`
	JWTID     string         `json:"jti"`
	SessionID string         `json:"sid,omitempty"`
	Events    map[string]any `json:"events"`
}

// IssueLogoutToken 专门签发 logout_token；普通 ID Token 不得作为注销令牌使用。
func (manager *OIDCJWTManager) IssueLogoutToken(claims OIDCLogoutTokenClaims, maxTTL time.Duration) (string, error) {
	if manager == nil || len(manager.privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("OIDC JWT manager is not initialized")
	}
	if maxTTL <= 0 || claims.Issuer != "" && claims.Issuer != manager.issuer || len(claims.Audience) == 0 || claims.JWTID == "" || claims.Subject == "" && claims.SessionID == "" || claims.IssuedAt.IsZero() || claims.ExpiresAt.IsZero() || !claims.ExpiresAt.After(claims.IssuedAt) || claims.ExpiresAt.Sub(claims.IssuedAt) > maxTTL {
		return "", errors.New("invalid OIDC back-channel logout claims")
	}
	if _, ok := claims.Events[OIDCBackchannelLogoutEvent]; !ok {
		return "", errors.New("logout token events claim is required")
	}
	claims.Issuer = manager.issuer
	payload, err := json.Marshal(oidcLogoutPayload{Issuer: claims.Issuer, Subject: claims.Subject, Audience: oidcAudience(claims.Audience), IssuedAt: claims.IssuedAt.UTC().Unix(), ExpiresAt: claims.ExpiresAt.UTC().Unix(), JWTID: claims.JWTID, SessionID: claims.SessionID, Events: claims.Events})
	if err != nil {
		return "", fmt.Errorf("marshal logout token: %w", err)
	}
	header, _ := json.Marshal(oidcJWTHeader{Algorithm: "EdDSA", Type: "logout+jwt", KeyID: manager.keyID})
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(manager.privateKey, []byte(input))), nil
}

// VerifyLogoutToken 验证专用 logout+jwt 头、签名和协议声明，明确拒绝普通 ID Token。
func (manager *OIDCJWTManager) VerifyLogoutToken(token, expectedAudience string, now time.Time, maxTTL time.Duration) (OIDCLogoutTokenClaims, error) {
	if manager == nil || !validOIDCString(expectedAudience) || now.IsZero() || maxTTL <= 0 {
		return OIDCLogoutTokenClaims{}, errors.New("invalid logout token verifier")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return OIDCLogoutTokenClaims{}, errors.New("invalid logout token serialization")
	}
	var header oidcJWTHeader
	if err := decodeOIDCJWTJSON(parts[0], &header); err != nil || header.Algorithm != "EdDSA" || header.Type != "logout+jwt" || header.KeyID != manager.keyID {
		return OIDCLogoutTokenClaims{}, errors.New("logout token header is invalid")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(manager.publicKey, []byte(parts[0]+"."+parts[1]), sig) {
		return OIDCLogoutTokenClaims{}, errors.New("logout token signature is invalid")
	}
	var payload oidcLogoutPayload
	if err = decodeOIDCJWTJSON(parts[1], &payload); err != nil {
		return OIDCLogoutTokenClaims{}, err
	}
	if !containsOIDCAudience([]string(payload.Audience), expectedAudience) || payload.Issuer != manager.issuer || payload.JWTID == "" || payload.Subject == "" && payload.SessionID == "" || payload.ExpiresAt <= payload.IssuedAt || time.Duration(payload.ExpiresAt-payload.IssuedAt)*time.Second > maxTTL || !time.Unix(payload.ExpiresAt, 0).After(now.UTC()) || time.Unix(payload.IssuedAt, 0).After(now.UTC().Add(time.Minute)) {
		return OIDCLogoutTokenClaims{}, errors.New("logout token claims are invalid")
	}
	if _, ok := payload.Events[OIDCBackchannelLogoutEvent]; !ok {
		return OIDCLogoutTokenClaims{}, errors.New("logout token event is missing")
	}
	return OIDCLogoutTokenClaims{Issuer: payload.Issuer, Subject: payload.Subject, Audience: []string(payload.Audience), IssuedAt: time.Unix(payload.IssuedAt, 0).UTC(), ExpiresAt: time.Unix(payload.ExpiresAt, 0).UTC(), JWTID: payload.JWTID, SessionID: payload.SessionID, Events: payload.Events}, nil
}
