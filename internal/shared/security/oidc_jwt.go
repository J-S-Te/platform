package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

// OIDCTokenUse 标识 OIDC JWT 的协议用途。验证方必须固定期望值，避免 ID Token 与
// Access Token 在签名密钥和部分声明相同的情况下被跨用途重放。
type OIDCTokenUse string

const (
	// OIDCTokenUseAccessToken identifies an OAuth 2.0 access token issued for an
	// authenticated end user.
	OIDCTokenUseAccessToken OIDCTokenUse = "access_token"
	// OIDCTokenUseIDToken identifies an OpenID Connect ID token.
	OIDCTokenUseIDToken OIDCTokenUse = "id_token"
)

// OIDCTokenClaims 是用户 Access Token 与 ID Token 共用的完整声明集。Issuer 由管理器固定，
// Audience 使用数组形态，Scope 按 OAuth 2.0 空格分隔格式编码。会话、jti、认证时间和授权版本
// 均作为必需数据，保证 UserInfo、注销、撤销和权限重验不依赖客户端补传状态。
type OIDCTokenClaims struct {
	Issuer             string
	Subject            string
	Audience           []string
	IssuedAt           time.Time
	ExpiresAt          time.Time
	JWTID              string
	SessionID          string
	AuthenticationTime time.Time
	Scope              []string
	ClientID           string
	Nonce              string
	TokenUse           OIDCTokenUse
	TenantID           string
	PersonID           string
	PrimaryOrgID       string
	OrganizationIDs    []string
	Roles              []string
	Permissions        []string
	RoleConfigHash     string
	AuthzRevision      uint64
}

// OIDCPublicJWK 是 RFC 7517/RFC 8037 形式的 Ed25519 公钥；JWKS 只暴露验签材料，
// 不得从进程中的私钥结构派生或序列化任何私钥字段。
type OIDCPublicJWK struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	X         string `json:"x"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}

// OIDCJWKS is the JSON Web Key Set document served by an OIDC JWKS endpoint.
type OIDCJWKS struct {
	Keys []OIDCPublicJWK `json:"keys"`
}

// OIDCJWTManager 负责终端用户 OAuth/OIDC Token。虽然加载同一 Ed25519 PEM 密钥，
// 它不依赖浏览器 Cookie 或机器客户端 JWT 管理器，协议用途由 kid、issuer、audience 和 token_use 共同约束。
type OIDCJWTManager struct {
	issuer     string
	keyID      string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

// LoadOIDCJWTManager 加载并核对 PKCS#8 私钥与 PKIX 公钥。kid 由公钥摘要确定：同一密钥重启后
// 保持稳定，轮换密钥时自动变化，使子系统能通过 JWKS 选择正确验签公钥。
func LoadOIDCJWTManager(issuer, privateKeyPath, publicKeyPath string) (*OIDCJWTManager, error) {
	if !validOIDCString(issuer) {
		return nil, errors.New("OIDC JWT issuer must not be empty or contain surrounding whitespace")
	}

	privateKey, err := loadEd25519PrivateKey(privateKeyPath)
	if err != nil {
		return nil, err
	}
	publicKey, err := loadEd25519PublicKey(publicKeyPath)
	if err != nil {
		return nil, err
	}
	derivedPublicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok || !derivedPublicKey.Equal(publicKey) {
		return nil, errors.New("OIDC JWT private key does not match OIDC JWT public key")
	}

	return &OIDCJWTManager{
		issuer:     issuer,
		keyID:      oidcKeyID(publicKey),
		privateKey: privateKey,
		publicKey:  publicKey,
	}, nil
}

// Issuer returns the exact issuer claim configured for this manager.
func (manager *OIDCJWTManager) Issuer() string {
	if manager == nil {
		return ""
	}
	return manager.issuer
}

// KeyID returns the deterministic key ID included in JWT headers and public JWKs.
func (manager *OIDCJWTManager) KeyID() string {
	if manager == nil {
		return ""
	}
	return manager.keyID
}

// IssueAccessToken signs an OAuth 2.0 access token with token_use=access_token.
func (manager *OIDCJWTManager) IssueAccessToken(claims OIDCTokenClaims) (string, error) {
	return manager.issue(claims, OIDCTokenUseAccessToken)
}

// IssueIDToken signs an OpenID Connect ID token with token_use=id_token.
func (manager *OIDCJWTManager) IssueIDToken(claims OIDCTokenClaims) (string, error) {
	return manager.issue(claims, OIDCTokenUseIDToken)
}

// VerifyAccessToken validates an access token for one expected audience at now.
func (manager *OIDCJWTManager) VerifyAccessToken(token, expectedAudience string, now time.Time) (OIDCTokenClaims, error) {
	return manager.Verify(token, expectedAudience, OIDCTokenUseAccessToken, now)
}

// VerifyIDToken validates an ID token for one expected audience at now.
func (manager *OIDCJWTManager) VerifyIDToken(token, expectedAudience string, now time.Time) (OIDCTokenClaims, error) {
	return manager.Verify(token, expectedAudience, OIDCTokenUseIDToken, now)
}

// Verify 对紧凑 JWT 执行严格 JSON 解码，并校验 EdDSA、kid、issuer、audience、token_use、
// 必需声明和时间窗口。now 由调用方传入，使过期边界和测试可重复；OIDC Token 不采用浏览器
// 会话 JWT 的时钟宽限，签发节点时钟超前会直接失败。
func (manager *OIDCJWTManager) Verify(token, expectedAudience string, expectedTokenUse OIDCTokenUse, now time.Time) (OIDCTokenClaims, error) {
	if manager == nil {
		return OIDCTokenClaims{}, errors.New("OIDC JWT manager must not be nil")
	}
	if !validOIDCString(expectedAudience) {
		return OIDCTokenClaims{}, errors.New("OIDC JWT expected audience must not be empty or contain surrounding whitespace")
	}
	if !validOIDCTokenUse(expectedTokenUse) {
		return OIDCTokenClaims{}, errors.New("OIDC JWT expected token use is unsupported")
	}
	if now.IsZero() {
		return OIDCTokenClaims{}, errors.New("OIDC JWT validation time must not be zero")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 || anyEmpty(parts) {
		return OIDCTokenClaims{}, errors.New("OIDC JWT has an invalid compact serialization")
	}

	var header oidcJWTHeader
	if err := decodeOIDCJWTJSON(parts[0], &header); err != nil {
		return OIDCTokenClaims{}, fmt.Errorf("decode OIDC JWT header: %w", err)
	}
	if header.Algorithm != "EdDSA" || header.Type != "JWT" || header.KeyID != manager.keyID {
		return OIDCTokenClaims{}, errors.New("OIDC JWT uses an unsupported header")
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return OIDCTokenClaims{}, errors.New("OIDC JWT has an invalid signature encoding")
	}
	if !ed25519.Verify(manager.publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return OIDCTokenClaims{}, errors.New("OIDC JWT signature verification failed")
	}

	var payload oidcJWTPayload
	if err := decodeOIDCJWTJSON(parts[1], &payload); err != nil {
		return OIDCTokenClaims{}, fmt.Errorf("decode OIDC JWT payload: %w", err)
	}
	claims, err := oidcClaimsFromPayload(payload)
	if err != nil {
		return OIDCTokenClaims{}, err
	}
	if err := validateOIDCTokenClaims(claims, manager.issuer, expectedTokenUse); err != nil {
		return OIDCTokenClaims{}, err
	}
	if !containsOIDCAudience(claims.Audience, expectedAudience) {
		return OIDCTokenClaims{}, errors.New("OIDC JWT audience does not match")
	}

	now = now.UTC()
	if !claims.ExpiresAt.After(now) {
		return OIDCTokenClaims{}, errors.New("OIDC JWT is expired")
	}
	if claims.IssuedAt.After(now) {
		return OIDCTokenClaims{}, errors.New("OIDC JWT was issued in the future")
	}
	return claims, nil
}

// PublicJWK returns the manager's RFC 8037 OKP/Ed25519 public JWK.
func (manager *OIDCJWTManager) PublicJWK() OIDCPublicJWK {
	if manager == nil {
		return OIDCPublicJWK{}
	}
	return OIDCPublicJWK{
		KeyType:   "OKP",
		Curve:     "Ed25519",
		X:         base64.RawURLEncoding.EncodeToString(manager.publicKey),
		Use:       "sig",
		Algorithm: "EdDSA",
		KeyID:     manager.keyID,
	}
}

// JWKS returns a one-key JSON Web Key Set suitable for an OIDC JWKS endpoint.
func (manager *OIDCJWTManager) JWKS() OIDCJWKS {
	return OIDCJWKS{Keys: []OIDCPublicJWK{manager.PublicJWK()}}
}

func (manager *OIDCJWTManager) issue(claims OIDCTokenClaims, tokenUse OIDCTokenUse) (string, error) {
	if manager == nil {
		return "", errors.New("OIDC JWT manager must not be nil")
	}
	if !validOIDCTokenUse(tokenUse) {
		return "", errors.New("OIDC JWT token use is unsupported")
	}
	if claims.TokenUse != "" && claims.TokenUse != tokenUse {
		return "", errors.New("OIDC JWT claims token use does not match issue method")
	}
	if claims.Issuer != "" && claims.Issuer != manager.issuer {
		return "", errors.New("OIDC JWT claims issuer does not match manager issuer")
	}

	claims.Issuer = manager.issuer
	claims.TokenUse = tokenUse
	claims.IssuedAt = claims.IssuedAt.UTC().Truncate(time.Second)
	claims.ExpiresAt = claims.ExpiresAt.UTC().Truncate(time.Second)
	claims.AuthenticationTime = claims.AuthenticationTime.UTC().Truncate(time.Second)
	if err := validateOIDCTokenClaims(claims, manager.issuer, tokenUse); err != nil {
		return "", err
	}

	header, err := json.Marshal(oidcJWTHeader{Algorithm: "EdDSA", Type: "JWT", KeyID: manager.keyID})
	if err != nil {
		return "", fmt.Errorf("marshal OIDC JWT header: %w", err)
	}
	payload, err := json.Marshal(oidcJWTPayload{
		Issuer:             claims.Issuer,
		Subject:            claims.Subject,
		Audience:           oidcAudience(claims.Audience),
		IssuedAt:           claims.IssuedAt.Unix(),
		ExpiresAt:          claims.ExpiresAt.Unix(),
		JWTID:              claims.JWTID,
		SessionID:          claims.SessionID,
		AuthenticationTime: claims.AuthenticationTime.Unix(),
		Scope:              strings.Join(claims.Scope, " "),
		ClientID:           claims.ClientID,
		Nonce:              claims.Nonce,
		TokenUse:           claims.TokenUse,
		TenantID:           claims.TenantID,
		PersonID:           claims.PersonID,
		PrimaryOrgID:       claims.PrimaryOrgID,
		OrganizationIDs:    append([]string{}, claims.OrganizationIDs...),
		Roles:              append([]string(nil), claims.Roles...),
		Permissions:        append([]string(nil), claims.Permissions...),
		RoleConfigHash:     claims.RoleConfigHash,
		AuthzRevision:      claims.AuthzRevision,
	})
	if err != nil {
		return "", fmt.Errorf("marshal OIDC JWT payload: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(manager.privateKey, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type oidcJWTHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

type oidcJWTPayload struct {
	Issuer             string       `json:"iss"`
	Subject            string       `json:"sub"`
	Audience           oidcAudience `json:"aud"`
	IssuedAt           int64        `json:"iat"`
	ExpiresAt          int64        `json:"exp"`
	JWTID              string       `json:"jti"`
	SessionID          string       `json:"sid"`
	AuthenticationTime int64        `json:"auth_time"`
	Scope              string       `json:"scope"`
	ClientID           string       `json:"client_id"`
	Nonce              string       `json:"nonce"`
	TokenUse           OIDCTokenUse `json:"token_use"`
	TenantID           string       `json:"tenant_id"`
	PersonID           string       `json:"person_id,omitempty"`
	PrimaryOrgID       string       `json:"primary_org_id"`
	OrganizationIDs    []string     `json:"organization_ids"`
	Roles              []string     `json:"roles"`
	Permissions        []string     `json:"permissions"`
	RoleConfigHash     string       `json:"role_config_hash"`
	AuthzRevision      uint64       `json:"authz_revision"`
}

type oidcAudience []string

func (audience *oidcAudience) UnmarshalJSON(data []byte) error {
	// 兼容 JWT 规范允许的单字符串与字符串数组输入，进入领域声明后统一为切片表示。
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*audience = oidcAudience{single}
		return nil
	}

	var multiple []string
	if err := json.Unmarshal(data, &multiple); err != nil {
		return errors.New("audience must be a string or array of strings")
	}
	*audience = oidcAudience(multiple)
	return nil
}

func decodeOIDCJWTJSON(encoded string, destination any) error {
	// 拒绝未知字段和尾随第二个 JSON 值，避免签发方与验证方对同一载荷产生不同解释。
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("JSON contains multiple values")
		}
		return err
	}
	return nil
}

func oidcClaimsFromPayload(payload oidcJWTPayload) (OIDCTokenClaims, error) {
	scope := strings.Fields(payload.Scope)
	if strings.Join(scope, " ") != payload.Scope {
		return OIDCTokenClaims{}, errors.New("OIDC JWT scope claim is not canonical")
	}
	return OIDCTokenClaims{
		Issuer:             payload.Issuer,
		Subject:            payload.Subject,
		Audience:           []string(payload.Audience),
		IssuedAt:           time.Unix(payload.IssuedAt, 0).UTC(),
		ExpiresAt:          time.Unix(payload.ExpiresAt, 0).UTC(),
		JWTID:              payload.JWTID,
		SessionID:          payload.SessionID,
		AuthenticationTime: time.Unix(payload.AuthenticationTime, 0).UTC(),
		Scope:              scope,
		ClientID:           payload.ClientID,
		Nonce:              payload.Nonce,
		TokenUse:           payload.TokenUse,
		TenantID:           payload.TenantID,
		PersonID:           payload.PersonID,
		PrimaryOrgID:       payload.PrimaryOrgID,
		OrganizationIDs:    append([]string(nil), payload.OrganizationIDs...),
		Roles:              append([]string(nil), payload.Roles...),
		Permissions:        append([]string(nil), payload.Permissions...),
		RoleConfigHash:     payload.RoleConfigHash,
		AuthzRevision:      payload.AuthzRevision,
	}, nil
}

func validateOIDCTokenClaims(claims OIDCTokenClaims, expectedIssuer string, expectedTokenUse OIDCTokenUse) error {
	if claims.Issuer != expectedIssuer {
		return errors.New("OIDC JWT issuer does not match")
	}
	if claims.TokenUse != expectedTokenUse {
		return errors.New("OIDC JWT token use does not match")
	}
	for _, value := range []string{claims.Subject, claims.JWTID, claims.SessionID, claims.ClientID, claims.TenantID} {
		if !validOIDCString(value) {
			return errors.New("OIDC JWT contains an empty or whitespace-padded required claim")
		}
	}
	// nonce 在授权码流程中可选；一旦客户端提供，就必须保留其不透明原值供客户端关联 ID Token。
	if claims.Nonce != "" && !validOIDCString(claims.Nonce) {
		return errors.New("OIDC JWT contains an invalid nonce claim")
	}
	if len(claims.Audience) == 0 {
		return errors.New("OIDC JWT audience must not be empty")
	}
	seenAudience := make(map[string]struct{}, len(claims.Audience))
	for _, audience := range claims.Audience {
		if !validOIDCString(audience) {
			return errors.New("OIDC JWT contains an invalid audience")
		}
		if _, exists := seenAudience[audience]; exists {
			return errors.New("OIDC JWT audience contains a duplicate value")
		}
		seenAudience[audience] = struct{}{}
	}
	if err := validateOIDCScopes(claims.Scope); err != nil {
		return err
	}
	if err := validateOIDCOrganizations(claims.PrimaryOrgID, claims.OrganizationIDs); err != nil {
		return err
	}
	if claims.PersonID != "" && !validPMSPersonID(claims.PersonID) {
		return errors.New("OIDC JWT contains an invalid PMS person identifier")
	}
	if claims.IssuedAt.IsZero() || claims.ExpiresAt.IsZero() || claims.AuthenticationTime.IsZero() ||
		!claims.ExpiresAt.After(claims.IssuedAt) || claims.AuthenticationTime.After(claims.IssuedAt) {
		return errors.New("OIDC JWT has invalid iat, exp, or auth_time claims")
	}
	return nil
}

func validPMSPersonID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character > 0x7f || !(character == '-' || character == '_' || character == '.' || character == ':' ||
			character >= '0' && character <= '9' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z') {
			return false
		}
	}
	return true
}

const maxOIDCOrganizationIDs = 100

// validateOIDCOrganizations 将组织声明限制为有序唯一的直接任职集合，并要求主组织属于该集合。
// 这里不展开组织树后代，资源范围授权必须由服务端根据可信资源归属另行计算。
func validateOIDCOrganizations(primaryOrgID string, organizationIDs []string) error {
	if len(organizationIDs) > maxOIDCOrganizationIDs {
		return errors.New("OIDC JWT organization list exceeds the supported maximum")
	}
	primaryFound := primaryOrgID == ""
	previous := ""
	for index, organizationID := range organizationIDs {
		if !validOIDCString(organizationID) || len([]byte(organizationID)) > 64 {
			return errors.New("OIDC JWT contains an invalid organization identifier")
		}
		if index > 0 && organizationID <= previous {
			return errors.New("OIDC JWT organization identifiers are not a sorted unique set")
		}
		if organizationID == primaryOrgID {
			primaryFound = true
		}
		previous = organizationID
	}
	if primaryOrgID != "" && (!validOIDCString(primaryOrgID) || len([]byte(primaryOrgID)) > 64 || !primaryFound) {
		return errors.New("OIDC JWT primary organization is not an active direct membership")
	}
	return nil
}

func validateOIDCScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("OIDC JWT scope must not be empty")
	}
	seenScope := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if !validOIDCString(scope) || strings.IndexFunc(scope, unicode.IsSpace) >= 0 {
			return errors.New("OIDC JWT contains an invalid scope")
		}
		if _, exists := seenScope[scope]; exists {
			return errors.New("OIDC JWT scope contains a duplicate value")
		}
		seenScope[scope] = struct{}{}
	}
	return nil
}

func validOIDCTokenUse(tokenUse OIDCTokenUse) bool {
	return tokenUse == OIDCTokenUseAccessToken || tokenUse == OIDCTokenUseIDToken
}

func validOIDCString(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func containsOIDCAudience(audiences []string, expected string) bool {
	for _, audience := range audiences {
		if audience == expected {
			return true
		}
	}
	return false
}

func oidcKeyID(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
