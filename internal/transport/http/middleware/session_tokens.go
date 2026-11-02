package middleware

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/keycloakctx"
)

// KeycloakSessionClaims 仅在完整验签及成对核验成功后暴露原始认证起点。
type KeycloakSessionClaims struct {
	keycloakctx.BrokerClaims
	AuthenticatedAt time.Time
}

// VerifySessionTokens 验证同一客户端同一次登录的访问和身份令牌。
// 参数：ctx为请求，access/id为令牌原文，clientID为票据客户端，nonceHash为票据摘要。
// 返回值：受信原始周期；签名、身份、sid、nonce或auth_time不符时返回通用错误。
func (v *KeycloakBrokerJWTVerifier) VerifySessionTokens(ctx context.Context, access, id, clientID, nonceHash string) (KeycloakSessionClaims, error) {
	invalid := errors.New("invalid session token binding")
	a, err := v.VerifyAuthorizationAccessToken(ctx, access)
	if err != nil {
		return KeycloakSessionClaims{}, err
	}
	if a.AuthorizedParty != clientID {
		return KeycloakSessionClaims{}, invalid
	}
	// 先验签，再读取nonce；摘要在服务端匹配票据，不能以报文中的期望值替代。
	verified, err := v.Verify(ctx, id)
	if err != nil {
		return KeycloakSessionClaims{}, err
	}
	parts := strings.Split(id, ".")
	var raw struct {
		Nonce    string          `json:"nonce"`
		AuthTime json.RawMessage `json:"auth_time"`
	}
	if err = decodeKeycloakJSONObject(parts[1], &raw); err != nil {
		return KeycloakSessionClaims{}, invalid
	}
	sum := sha256.Sum256([]byte(raw.Nonce))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if raw.Nonce == "" || subtle.ConstantTimeCompare([]byte(challenge), []byte(nonceHash)) != 1 {
		return KeycloakSessionClaims{}, invalid
	}
	if _, err = v.VerifyIDToken(ctx, id, raw.Nonce, clientID); err != nil {
		return KeycloakSessionClaims{}, err
	}
	if verified.Issuer != a.Issuer || verified.SessionID != a.SessionID || verified.Subject != a.Subject ||
		verified.TenantID != a.TenantID || verified.IdentityID != a.IdentityID {
		return KeycloakSessionClaims{}, invalid
	}
	authTime, ok := keycloakNumericDate(raw.AuthTime)
	if !ok || authTime.IsZero() || authTime.After(v.now().Add(5*time.Second)) {
		return KeycloakSessionClaims{}, invalid
	}
	return KeycloakSessionClaims{BrokerClaims: verified, AuthenticatedAt: authTime}, nil
}
