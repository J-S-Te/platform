// Package domain 定义与 HTTP 和 JWT 编码无关的 OIDC/OAuth 运行时状态。
package domain

import (
	"net"
	"time"
)

const (
	// ClientStatusActive is the only client status eligible for protocol processing.
	ClientStatusActive = "ACTIVE"
	// SessionStatusActive is the only local session status eligible for authorization.
	SessionStatusActive = "ACTIVE"
	// AuthorizationCodeStatusActive marks an authorization code that has not been exchanged.
	AuthorizationCodeStatusActive = "ACTIVE"
	// AuthorizationCodeStatusConsumed marks a code permanently used by the token endpoint.
	AuthorizationCodeStatusConsumed = "CONSUMED"
	// AuthorizationCodeStatusRevoked marks an unconsumed code invalidated with its browser session.
	AuthorizationCodeStatusRevoked = "REVOKED"
	// TokenFamilyStatusActive marks a refresh-token family that can still rotate.
	TokenFamilyStatusActive = "ACTIVE"
	// TokenFamilyStatusRevoked marks a family invalid after logout, revocation, or replay.
	TokenFamilyStatusRevoked = "REVOKED"
	// RefreshTokenStatusActive marks the single refresh token that can be exchanged next.
	RefreshTokenStatusActive = "ACTIVE"
	// RefreshTokenStatusConsumed marks a refresh token replaced during rotation.
	RefreshTokenStatusConsumed = "CONSUMED"
	// RefreshTokenStatusRevoked marks a refresh token invalidated with its family.
	RefreshTokenStatusRevoked = "REVOKED"
	// TokenTypeAccess identifies a bearer access-token revocation record.
	TokenTypeAccess = "access_token"
	// TokenTypeRefresh identifies a refresh-token revocation record.
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

// ClientCredential is a currently usable client-secret verifier. The plaintext secret is never
// copied into domain state, storage, logs, or returned values.
type ClientCredential struct {
	SecretHash []byte
	ValidUntil *time.Time
}

// SessionSubject is the local authenticated browser session to which an authorization code is
// bound. The repository only returns it when tenant, session, account and user remain active.
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

// TokenGrant is a validated subject/client binding from which an access token, optional ID token,
// and a rotated refresh token may safely be issued.
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

// UserInfoSubject is a current tenant-scoped user projection. It deliberately contains only
// claims that UserInfo may selectively expose according to granted scopes.
type UserInfoSubject struct {
	TenantID          string
	OAuthClientID     string
	SessionID         string
	UserID            string
	DisplayName       string
	PreferredUsername string
	Email             string
}

// ClientJWK is an active public signing key registered for private_key_jwt or signed request objects.
// The JWK is public-only; private JWK members are rejected by the application service.
type ClientJWK struct {
	PublicJWK  []byte
	KeyID      string
	ValidUntil *time.Time
}
