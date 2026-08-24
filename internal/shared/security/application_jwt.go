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

// ApplicationTokenClaims 是 OAuth 客户端的短期机器身份。Token 携带租户、应用、环境和 scope
// 完整绑定，但认证器仍会回查当前客户端登记；禁用客户端或收回 scope 不能仅依赖 Token 到期。
type ApplicationTokenClaims struct {
	OAuthClientID   string
	ClientID        string
	TenantID        string
	ApplicationID   string
	ApplicationCode string
	EnvironmentID   string
	EnvironmentCode string
	Scopes          []string
	IssuedAt        time.Time
	NotBefore       time.Time
	ExpiresAt       time.Time
}

type applicationJWTPayload struct {
	Issuer          string   `json:"iss"`
	Audience        string   `json:"aud"`
	TokenUse        string   `json:"token_use"`
	Subject         string   `json:"sub"`
	OAuthClientID   string   `json:"oauth_client_id"`
	TenantID        string   `json:"tenant_id"`
	ApplicationID   string   `json:"application_id"`
	ApplicationCode string   `json:"application_code"`
	EnvironmentID   string   `json:"environment_id"`
	EnvironmentCode string   `json:"environment_code"`
	Scopes          []string `json:"scope"`
	IssuedAt        int64    `json:"iat"`
	NotBefore       int64    `json:"nbf"`
	ExpiresAt       int64    `json:"exp"`
}

// ApplicationJWTManager 与浏览器会话复用 Ed25519 密钥，但使用独立 audience 和 token_use；
// 密钥复用不代表信任域可互换，验证方必须同时检查这两个用途隔离字段。
type ApplicationJWTManager struct {
	issuer     string
	audience   string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// LoadApplicationJWTManager 加载应用令牌使用的 Ed25519 签名密钥。
func LoadApplicationJWTManager(issuer, audience, privateKeyPath, publicKeyPath string) (*ApplicationJWTManager, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return nil, errors.New("application JWT issuer and audience must not be empty")
	}
	privateKey, err := loadEd25519PrivateKey(privateKeyPath)
	if err != nil {
		return nil, err
	}
	publicKey, err := loadEd25519PublicKey(publicKeyPath)
	if err != nil {
		return nil, err
	}
	return &ApplicationJWTManager{issuer: issuer, audience: audience, privateKey: privateKey, publicKey: publicKey}, nil
}

// Issue 签发 client_credentials Token。NotBefore 默认为签发时刻；调用方可推迟生效，
// 但不能构造早于 IssuedAt 的有效窗口。
func (manager *ApplicationJWTManager) Issue(claims ApplicationTokenClaims) (string, error) {
	if manager == nil {
		return "", errors.New("application JWT manager must not be nil")
	}
	claims = canonicalApplicationClaims(claims)
	if claims.NotBefore.IsZero() {
		claims.NotBefore = claims.IssuedAt
	}
	if err := validateApplicationClaims(claims); err != nil {
		return "", err
	}
	payload, err := json.Marshal(applicationJWTPayload{
		Issuer: manager.issuer, Audience: manager.audience, TokenUse: "application", Subject: claims.ClientID,
		OAuthClientID: claims.OAuthClientID, TenantID: claims.TenantID, ApplicationID: claims.ApplicationID,
		ApplicationCode: claims.ApplicationCode, EnvironmentID: claims.EnvironmentID,
		EnvironmentCode: claims.EnvironmentCode, Scopes: claims.Scopes,
		IssuedAt: claims.IssuedAt.Unix(), NotBefore: claims.NotBefore.Unix(), ExpiresAt: claims.ExpiresAt.Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("marshal application JWT payload: %w", err)
	}
	header, err := json.Marshal(jwtHeader{Algorithm: "EdDSA", Type: "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal application JWT header: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(manager.privateKey, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Verify 校验签名、用途、时间窗口和租户/应用/环境绑定。这里只证明 Token 自洽且由平台签发，
// 客户端当前是否启用、登记是否仍匹配以及 scope 是否撤销由应用注册认证器继续校验。
func (manager *ApplicationJWTManager) Verify(token string, now time.Time) (ApplicationTokenClaims, error) {
	if manager == nil {
		return ApplicationTokenClaims{}, errors.New("application JWT manager must not be nil")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || anyEmpty(parts) {
		return ApplicationTokenClaims{}, errors.New("JWT has an invalid compact serialization")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ApplicationTokenClaims{}, fmt.Errorf("decode JWT header: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil || header.Algorithm != "EdDSA" || header.Type != "JWT" {
		return ApplicationTokenClaims{}, errors.New("JWT uses an unsupported header")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(manager.publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return ApplicationTokenClaims{}, errors.New("JWT signature verification failed")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ApplicationTokenClaims{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var payload applicationJWTPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return ApplicationTokenClaims{}, fmt.Errorf("decode JWT payload JSON: %w", err)
	}
	if payload.Issuer != manager.issuer || payload.Audience != manager.audience || payload.TokenUse != "application" {
		return ApplicationTokenClaims{}, errors.New("JWT issuer, audience or token type does not match")
	}
	claims := canonicalApplicationClaims(ApplicationTokenClaims{
		OAuthClientID: payload.OAuthClientID, ClientID: payload.Subject, TenantID: payload.TenantID,
		ApplicationID: payload.ApplicationID, ApplicationCode: payload.ApplicationCode,
		EnvironmentID: payload.EnvironmentID, EnvironmentCode: payload.EnvironmentCode,
		Scopes: payload.Scopes, IssuedAt: time.Unix(payload.IssuedAt, 0).UTC(),
		NotBefore: time.Unix(payload.NotBefore, 0).UTC(), ExpiresAt: time.Unix(payload.ExpiresAt, 0).UTC(),
	})
	if err := validateApplicationClaims(claims); err != nil {
		return ApplicationTokenClaims{}, err
	}
	now = now.UTC()
	if !claims.ExpiresAt.After(now) || claims.IssuedAt.After(now.Add(allowedClockSkew)) || claims.NotBefore.After(now.Add(allowedClockSkew)) {
		return ApplicationTokenClaims{}, errors.New("application JWT is expired or not yet valid")
	}
	return claims, nil
}

func canonicalApplicationClaims(claims ApplicationTokenClaims) ApplicationTokenClaims {
	// JWT NumericDate 以秒为精度；签发和验证统一截断，避免数据库或测试中的亚秒值造成边界漂移。
	claims.IssuedAt = claims.IssuedAt.UTC().Truncate(time.Second)
	claims.NotBefore = claims.NotBefore.UTC().Truncate(time.Second)
	claims.ExpiresAt = claims.ExpiresAt.UTC().Truncate(time.Second)
	return claims
}

func validateApplicationClaims(claims ApplicationTokenClaims) error {
	for _, value := range []string{claims.OAuthClientID, claims.ClientID, claims.TenantID, claims.ApplicationID, claims.ApplicationCode, claims.EnvironmentID, claims.EnvironmentCode} {
		if strings.TrimSpace(value) == "" {
			return errors.New("application JWT contains an empty required claim")
		}
	}
	if len(claims.Scopes) == 0 || claims.IssuedAt.IsZero() || claims.NotBefore.IsZero() || claims.ExpiresAt.IsZero() ||
		claims.NotBefore.Before(claims.IssuedAt) || !claims.ExpiresAt.After(claims.NotBefore) {
		return errors.New("application JWT contains invalid scopes or timestamps")
	}
	seenScopes := make(map[string]struct{}, len(claims.Scopes))
	for _, scope := range claims.Scopes {
		if scope == "" || strings.TrimSpace(scope) != scope {
			return errors.New("application JWT contains an invalid scope")
		}
		if _, duplicated := seenScopes[scope]; duplicated {
			return errors.New("application JWT contains duplicated scopes")
		}
		seenScopes[scope] = struct{}{}
	}
	return nil
}
