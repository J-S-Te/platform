package security

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const reauthenticationType = "OIDC-REAUTH"

type reauthenticationClaims struct {
	Issuer          string `json:"iss"`
	Audience        string `json:"aud"`
	RequestHash     string `json:"request_hash"`
	PreviousSession string `json:"previous_session"`
	IssuedAt        int64  `json:"iat"`
	ExpiresAt       int64  `json:"exp"`
}

// IssueReauthenticationProof 签发仅用于同一浏览器原授权请求的短效重新认证凭据。
// 参数：requestHash为完整原请求摘要，previousSession为原会话，now为服务端认证要求时间。
// 返回值：用途隔离的签名凭据；缺少请求、会话或签名材料时返回错误。
func (m *OIDCJWTManager) IssueReauthenticationProof(requestHash, previousSession string, now time.Time) (string, error) {
	if requestHash == "" || previousSession == "" || now.IsZero() || len(m.privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("invalid reauthentication proof input")
	}
	header, err := json.Marshal(jwtHeader{Algorithm: "EdDSA", Type: reauthenticationType})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(reauthenticationClaims{Issuer: m.issuer, Audience: m.issuer + "/authorize", RequestHash: requestHash, PreviousSession: previousSession, IssuedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix()})
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(m.privateKey, []byte(input))), nil
}

// VerifyReauthenticationProof 验证专用签名、请求绑定和五分钟期限，不接受任何业务JWT替代。
// 参数：proof为HttpOnly Cookie材料，requestHash为当前原授权请求摘要，now为服务端时钟。
// 返回值：原会话和要求重新认证的时间；任何用途、签名、绑定或时间错误均拒绝。
func (m *OIDCJWTManager) VerifyReauthenticationProof(proof, requestHash string, now time.Time) (string, time.Time, error) {
	invalid := errors.New("invalid reauthentication proof")
	parts := strings.Split(proof, ".")
	if len(parts) != 3 || len(proof) > 4096 || len(m.publicKey) != ed25519.PublicKeySize {
		return "", time.Time{}, invalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(m.publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return "", time.Time{}, invalid
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", time.Time{}, invalid
	}
	var header jwtHeader
	if json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "EdDSA" || header.Type != reauthenticationType {
		return "", time.Time{}, invalid
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", time.Time{}, invalid
	}
	var claims reauthenticationClaims
	if json.Unmarshal(body, &claims) != nil || claims.Issuer != m.issuer || claims.Audience != m.issuer+"/authorize" || claims.RequestHash != requestHash || claims.PreviousSession == "" || claims.IssuedAt > now.Unix() || claims.ExpiresAt <= now.Unix() || claims.ExpiresAt-claims.IssuedAt != 300 {
		return "", time.Time{}, invalid
	}
	return claims.PreviousSession, time.Unix(claims.IssuedAt, 0).UTC(), nil
}
