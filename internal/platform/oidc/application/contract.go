// Package application 在不依赖 HTTP 的情况下协调 OIDC/OAuth 协议状态。
package application

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
)

var (
	// ErrInvalidRequest 标识畸形的协议输入。
	ErrInvalidRequest = errors.New("invalid OIDC/OAuth request")
	// ErrInvalidClient 刻意覆盖未知、停用与认证不当的客户端，不向调用方区分具体原因。
	ErrInvalidClient = errors.New("invalid OIDC/OAuth client")
	// ErrUnauthorizedClient 表示客户端有效但未注册使用所请求的 grant。
	ErrUnauthorizedClient = errors.New("OIDC/OAuth client is not authorized for this grant")
	// ErrAccessDenied 表示已认证的资源属主没有应用级访问权。传输适配器只能暴露
	// 标准 OAuth 错误码，绝不能泄露角色数据。
	ErrAccessDenied = errors.New("OIDC/OAuth resource owner access is denied")
	// ErrInvalidScope 标识未注册或畸形的请求 scope。
	ErrInvalidScope = errors.New("invalid OIDC/OAuth scope")
	// ErrInvalidGrant 刻意覆盖过期、已消费、不匹配与未知的 grant。
	ErrInvalidGrant = errors.New("invalid OIDC/OAuth grant")
	// ErrRefreshTokenReplay 表示先前的 refresh token 被再次出示。返回该错误前其
	// 令牌族已被撤销；传输层应把它映射为 invalid_grant。
	ErrRefreshTokenReplay = errors.New("OIDC/OAuth refresh token replay detected")
	// ErrInvalidToken 标识不能用于 UserInfo 的令牌或主体。
	ErrInvalidToken = errors.New("invalid OIDC/OAuth token")

	// ErrNotFound 由仓储在租户范围内找不到记录时返回。应用方法会把它映射为
	// 协议安全的公共错误，避免暴露记录是否存在。
	ErrNotFound = errors.New("OIDC/OAuth record not found")
	// ErrAuthorizationCodeUnavailable 在授权码过期或已消费时原子返回。
	ErrAuthorizationCodeUnavailable = errors.New("OIDC/OAuth authorization code is unavailable")
)

// Clock 提供协议用 UTC 时间，使过期行为可确定性测试。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产环境时钟实现。
type SystemClock struct{}

// Now 返回当前 UTC 时刻。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// IDGenerator 生成持久化聚合 ID；生产调用方可使用共享 ULID 生成器。
type IDGenerator interface {
	New(at time.Time) (string, error)
}

// SecretGenerator 生成密码学强度足够、URL 安全的不透明值。本接口返回的值会
// 立即哈希后再持久化，绝不允许写入日志。
type SecretGenerator interface {
	NewSecret() (string, error)
}

// TokenIssuer 在服务完成持久化授权状态校验之后签署协议 access token 与 ID token。
// JWT/JWKS 的具体实现被刻意留在本模块之外。
type TokenIssuer interface {
	IssueOIDCTokens(context.Context, TokenIssue) (IssuedTokens, error)
}

// Repository 定义 OIDC/OAuth 运行时所需的持久化 MySQL 操作。消费或轮换机密的
// 命令必须在单个事务内完成"比较并转移"逻辑。
type Repository interface {
	FindClient(ctx context.Context, clientID string, now time.Time) (domain.Client, error)
	ResolveSessionSubject(ctx context.Context, sessionID string, now time.Time) (domain.SessionSubject, error)
	CreateAuthorizationCode(ctx context.Context, code domain.AuthorizationCode) error
	FindAuthorizationCode(ctx context.Context, codeHash [32]byte, now time.Time) (domain.AuthorizationCode, error)
	ConsumeAuthorizationCode(ctx context.Context, command ConsumeAuthorizationCodeCommand, now time.Time) (domain.TokenGrant, error)
	FindRefreshToken(ctx context.Context, tokenHash [32]byte, now time.Time) (domain.RefreshToken, error)
	RotateRefreshToken(ctx context.Context, command RotateRefreshTokenCommand, now time.Time) (domain.TokenGrant, error)
	RevokeToken(ctx context.Context, command RevokeTokenCommand, now time.Time) error
	IsTokenRevoked(ctx context.Context, tenantID string, tokenHash [32]byte, now time.Time) (bool, error)
	ResolveUserInfo(ctx context.Context, query UserInfoQuery, now time.Time) (domain.UserInfoSubject, error)
}

// AuthorizationInput 是浏览器授权请求在 HTTP 层完成本地会话认证后的形态。
// State 仅用于传输层重定向回显，刻意不持久化。
type AuthorizationInput struct {
	ClientID            string
	RedirectURI         string
	Scopes              []string
	State               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	SessionID           string
}

// AuthorizationResult 携带面向已注册精确 redirect URI 的一次性不透明授权码。
type AuthorizationResult struct {
	AuthorizationCode string
	RedirectURI       string
	Scope             string
	State             string
	ExpiresAt         time.Time
}

// ClientAuthentication 表示 token 端点的客户端认证。HTTP 适配器只能从传输层安全
// 来源（如 HTTP Basic）填充 ClientSecret。
type ClientAuthentication struct {
	ClientID     string
	ClientSecret string
	// ClientAssertion 是仅配合 private_key_jwt 使用的签名 JWT，绝不持久化。
	ClientAssertion         string
	ClientAssertionType     string
	ClientAssertionAudience string
}

// AuthorizationCodeExchangeInput 表示 grant_type=authorization_code。
type AuthorizationCodeExchangeInput struct {
	ClientAuthentication
	Code         string
	RedirectURI  string
	CodeVerifier string
}

// RefreshTokenInput 表示 grant_type=refresh_token。
type RefreshTokenInput struct {
	ClientAuthentication
	RefreshToken string
}

// RevokeTokenInput 表示 RFC 7009 风格的撤销请求，在 HTTP 层已选定受支持的
// token hint 后进入。未知令牌值刻意按成功的 no-op 处理。
type RevokeTokenInput struct {
	ClientAuthentication
	Token     string
	TokenType string
	ExpiresAt *time.Time
	Reason    string
}

// TokenIssue 是 OIDC JWT 适配器可签署的完整、已校验声明材料。令牌标识在持久化
// 之前就已固定，适配器可将其作为 jti 写入。
type TokenIssue struct {
	AccessTokenID        string
	TenantID             string
	OAuthClientID        string
	ClientID             string
	SessionID            string
	AccountID            string
	UserID               string
	Scopes               []string
	Nonce                string
	AuthorizedAt         time.Time
	IssuedAt             time.Time
	AccessTokenExpiresAt time.Time
	IssueIDToken         bool
}

// IssuedTokens 由签名适配器返回。refresh token 由本服务而非签名器生成与保存，
// 因此签名器永远接触不到长效 bearer 机密。
type IssuedTokens struct {
	AccessToken string
	IDToken     string
}

// TokenResult 可安全用于 token HTTP 响应。refresh token 仅在客户端注册了
// refresh_token grant 且 refresh token TTL 为正时才返回。
type TokenResult struct {
	AccessToken  string
	TokenType    string
	ExpiresIn    int64
	Scope        string
	IDToken      string
	RefreshToken string
}

// NewRefreshToken 描述授权码兑换或轮换时新增的持久化 refresh token 状态。
type NewRefreshToken struct {
	ID                   string
	TokenFamilyID        string
	ParentRefreshTokenID string
	TokenHash            [32]byte
	TenantID             string
	OAuthClientID        string
	SessionID            string
	AccountID            string
	UserID               string
	Scopes               []string
	AuthorizedAt         time.Time
	IssuedAt             time.Time
	ExpiresAt            time.Time
}

// ConsumeAuthorizationCodeCommand 把授权码消费绑定到精确的 client、redirect URI，
// 以及可选的 refresh token 创建。实现必须先按 CodeHash 加锁再转移状态。
type ConsumeAuthorizationCodeCommand struct {
	CodeHash    [32]byte
	ClientID    string
	RedirectURI string
	Refresh     *NewRefreshToken
}

// RotateRefreshTokenCommand 原子消费出示的 token 并创建其后继节点。
type RotateRefreshTokenCommand struct {
	TokenHash [32]byte
	ClientID  string
	Refresh   NewRefreshToken
}

// RevokeTokenCommand 记录 access token 摘要，或撤销匹配的 refresh token 族。
type RevokeTokenCommand struct {
	RevocationID  string
	TenantID      string
	OAuthClientID string
	TokenHash     [32]byte
	TokenType     string
	ExpiresAt     *time.Time
	Reason        string
}

// UserInfoInput 由已经验签且未撤销的用户 access token 构造。声明会重新对照
// MySQL 解析，因此停用的租户、会话、账号与用户都会被拒绝。
type UserInfoInput struct {
	TenantID      string
	OAuthClientID string
	SessionID     string
	UserID        string
	Scopes        []string
}

// UserInfoQuery 是 UserInfoInput 的仓储安全等价形态。
type UserInfoQuery struct {
	TenantID      string
	OAuthClientID string
	SessionID     string
	UserID        string
}

// UserInfo 只包含 access token 已授予 scope 所允许的声明。
type UserInfo struct {
	Subject           string
	Name              string
	PreferredUsername string
	Email             string
}
