// Package domain 定义与 HTTP 和 JWT 编码无关的 OIDC/OAuth 运行时状态。
package domain

import (
	"net"
	"time"
)

const (
	// ClientStatusActive 是唯一可参与协议处理的客户端状态。
	ClientStatusActive = "ACTIVE"
	// SessionStatusActive 是唯一可用于授权的本地会话状态。
	SessionStatusActive = "ACTIVE"
	// AuthorizationCodeStatusActive 标记尚未兑换的授权码。
	AuthorizationCodeStatusActive = "ACTIVE"
	// AuthorizationCodeStatusConsumed 标记已被 token 端点永久使用的授权码。
	AuthorizationCodeStatusConsumed = "CONSUMED"
	// AuthorizationCodeStatusRevoked 标记未消费但已随浏览器会话一并失效的授权码。
	AuthorizationCodeStatusRevoked = "REVOKED"
	// TokenFamilyStatusActive 标记仍可轮换的 refresh token 族。
	TokenFamilyStatusActive = "ACTIVE"
	// TokenFamilyStatusRevoked 标记因登出、撤销或重放而整族失效的 refresh token 族。
	TokenFamilyStatusRevoked = "REVOKED"
	// RefreshTokenStatusActive 标记当前唯一可兑换的 refresh token。
	RefreshTokenStatusActive = "ACTIVE"
	// RefreshTokenStatusConsumed 标记在轮换中已被替换的 refresh token。
	RefreshTokenStatusConsumed = "CONSUMED"
	// RefreshTokenStatusRevoked 标记随其令牌族一并失效的 refresh token。
	RefreshTokenStatusRevoked = "REVOKED"
	// TokenTypeAccess 标识 bearer access token 撤销记录。
	TokenTypeAccess = "access_token"
	// TokenTypeRefresh 标识 refresh token 撤销记录。
	TokenTypeRefresh = "refresh_token"
)

// Client 是协议运行时读取的活跃客户端快照，不包含任何明文密钥；Credentials 仅携带
// 持久化的单向校验摘要，JWKs 也只允许公开验签材料。
type Client struct {
	ID                     string
	TenantID               string
	ClientID               string
	ClientType             string
	TokenAuthMethod        string
	AccessTokenTTLSeconds  uint
	RefreshTokenTTLSeconds uint
	RequirePKCE            bool
	GrantTypes             map[string]struct{}
	Scopes                 map[string]struct{}
	RedirectURIs           map[string]struct{}
	Credentials            []ClientCredential
	JWKs                   []ClientJWK
}

// ClientCredential 是当前可用的 client secret 校验器。明文密钥绝不进入领域状态、
// 存储、日志或返回值。
type ClientCredential struct {
	SecretHash []byte
	ValidUntil *time.Time
}

// SessionSubject 是授权码绑定的本地已认证浏览器会话；仓储只在租户、会话、
// 账号与用户全部保持活动时才返回它。
type SessionSubject struct {
	TenantID           string
	SessionID          string
	AccountID          string
	UserID             string
	LoginIP            net.IP
	ExpiresAt          time.Time
	MustChangePassword bool
}

// AuthorizationCode 是持久化的一次性凭据。CodeHash 固定为高熵随机授权码的 SHA-256 摘要，
// RedirectURI、PKCE 与会话主体一并冻结，兑换时不得从当前请求重新推断。
type AuthorizationCode struct {
	ID                  string
	TenantID            string
	OAuthClientID       string
	SessionID           string
	AccountID           string
	UserID              string
	CodeHash            [32]byte
	RedirectURI         string
	Scopes              []string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
	Status              string
}

// RefreshToken 是轮换链中的持久节点。一个令牌族最多允许一个 ACTIVE 节点；已消费节点重放时，
// 整族剩余令牌都会撤销，以把并发异常视为潜在泄露而非普通失败。
type RefreshToken struct {
	ID                   string
	TenantID             string
	OAuthClientID        string
	SessionID            string
	AccountID            string
	UserID               string
	Scopes               []string
	AuthorizedAt         time.Time
	TokenFamilyID        string
	ParentRefreshTokenID string
	TokenHash            [32]byte
	IssuedAt             time.Time
	ExpiresAt            time.Time
	UsedAt               *time.Time
	RevokedAt            *time.Time
	RevokeReason         string
	Status               string
}

// TokenGrant 是经验证的主体/客户端绑定，只有它才能安全签发 access token、
// 可选 ID token 及轮换后的 refresh token。
type TokenGrant struct {
	TenantID      string
	OAuthClientID string
	ClientID      string
	SessionID     string
	AccountID     string
	UserID        string
	Scopes        []string
	Nonce         string
	AuthorizedAt  time.Time
}

// UserInfoSubject 是租户内当前的用户投影。它刻意只包含 UserInfo 可按已授予
// scope 选择性暴露的声明。
type UserInfoSubject struct {
	TenantID          string
	OAuthClientID     string
	SessionID         string
	UserID            string
	DisplayName       string
	PreferredUsername string
	Email             string
}

// ClientJWK 是为 private_key_jwt 或签名请求对象注册的活动公开验签密钥。
// JWK 仅允许公开成员；含私钥成员的提交会被应用服务拒绝。
type ClientJWK struct {
	PublicJWK  []byte
	KeyID      string
	ValidUntil *time.Time
}
