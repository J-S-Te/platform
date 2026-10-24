// Package oidchttp 把 OIDC/OAuth 应用服务适配为协议 HTTP 端点。
package oidchttp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

const (
	maxAuthorizeRequestBytes = 16 << 10
	maxTokenRequestBytes     = 64 << 10
)

// Service 是本 HTTP 适配器使用的 OIDC 应用边界。保持为接口使 HTTP 测试独立于
// 持久化，并允许 bootstrap 直接注入 application.Service。
type Service interface {
	Authorize(context.Context, application.AuthorizationInput) (application.AuthorizationResult, error)
	ResolveRequestObject(context.Context, application.RequestObjectAuthorizationInput) (application.AuthorizationInput, error)
	PushAuthorizationRequest(context.Context, application.PushAuthorizationRequestInput) (application.PushAuthorizationRequestResult, error)
	ConsumePushedAuthorizationRequest(context.Context, application.ConsumePushedAuthorizationRequestInput) (application.AuthorizationInput, error)
	DecideConsent(context.Context, application.ConsentInput) (application.ConsentDecision, error)
	GrantConsent(context.Context, application.ConsentInput) error
	RevokeConsent(context.Context, string, string, string) error
	ExchangeAuthorizationCode(context.Context, application.AuthorizationCodeExchangeInput) (application.TokenResult, error)
	Refresh(context.Context, application.RefreshTokenInput) (application.TokenResult, error)
	Revoke(context.Context, application.RevokeTokenInput) error
	IsAccessTokenRevoked(context.Context, string, string) (bool, error)
	ResolveUserInfo(context.Context, application.UserInfoInput) (application.UserInfo, error)
}

// LegacyClientCredentialsIssuer 在平台把全部公共 OAuth 端点迁移到本 Handler 期间，
// 保留既有 client_credentials 行为。
type LegacyClientCredentialsIssuer interface {
	IssueClientCredentials(context.Context, string, string, []string) (applicationregistryapplication.TokenResult, error)
}

// BrowserSessionAuthenticator 校验平台签名的浏览器会话 cookie。身份应用服务已
// 暴露同形状方法，bootstrap 可直接注入。
type BrowserSessionAuthenticator interface {
	Authenticate(context.Context, string) (authctx.Principal, error)
}

// BrowserSessionLogout 撤销一个已认证的平台会话。它刻意与 BrowserSessionAuthenticator
// 分开：部署可能先发布 OIDC，之后再接线 RP-initiated logout。
type BrowserSessionLogout interface {
	Logout(context.Context, authctx.Principal) error
}

// JWTManager 表示协议边缘所需的 OIDC 签名密钥操作。
type JWTManager interface {
	Issuer() string
	JWKS() security.OIDCJWKS
	VerifyAccessToken(token, expectedAudience string, now time.Time) (security.OIDCTokenClaims, error)
	VerifyIDToken(token, expectedAudience string, now time.Time) (security.OIDCTokenClaims, error)
}

// PostLogoutRedirectValidator 针对专用 post-logout 登记集合校验 URI；
// 绝不复用普通 OAuth redirect URI。
type PostLogoutRedirectValidator interface {
	IsRegisteredPostLogoutRedirectURI(context.Context, string, string) (bool, error)
}

// AccessTokenSubject 携带紧凑公共 JWT 中省略的租户内持久标识。解析器必须把签名
// 的 client/session/user 标识绑定到活动存储记录。
type AccessTokenSubject struct {
	TenantID      string
	OAuthClientID string
	LoginIP       string
}

// AccessTokenSubjectResolver 解析 application.ResolveUserInfo 与撤销检查所需的
// 持久化 OIDC client 与租户 ID；缺失时 UserInfo fail-closed。
type AccessTokenSubjectResolver interface {
	ResolveAccessTokenSubject(context.Context, string, string, string) (AccessTokenSubject, error)
}

// ExternalAuthorizationTokenVerifier 是面向 Keycloak 等上游身份提供方的窄信任边界。
// 仅 authorization-context 使用它；平台常规 OAuth 端点继续接受平台签发的 access token。
type ExternalAuthorizationTokenClaims struct {
	Subject         string
	IdentityID      string
	TenantID        string
	SessionID       string
	AuthorizedParty string
	Audience        []string
	TokenUse        string
}

// ExternalAuthorizationTokenVerifier 验签外部签发的访问 token 并提取紧凑声明。
type ExternalAuthorizationTokenVerifier interface {
	Verify(context.Context, string) (ExternalAuthorizationTokenClaims, error)
}

// PersonnelDirectoryEntry 是暴露给持 profile scope 的子系统会话的最小租户人员投影；
// 登录名与工号被刻意排除。
type PersonnelDirectoryEntry struct {
	UserID      string   `json:"user_id"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles" gorm:"-"`
}

// PersonnelDirectoryResolver 按租户与 OAuth client 列出可授权的活动人员。
type PersonnelDirectoryResolver interface {
	ListActivePersonnel(context.Context, string, string) ([]PersonnelDirectoryEntry, error)
}

// Clock 使 JWT 校验在单元测试中可确定性复现。
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

// Now 返回当前 UTC 时间。
func (systemClock) Now() time.Time { return time.Now().UTC() }

// Config 列出 OIDC HTTP 边缘全部由 bootstrap 注入的依赖。SessionAuthenticator、
// SessionLogout、LegacyClientCredentialsIssuer 与 PostLogoutRedirectValidator 刻意
// 可选：依赖缺失适配器的端点一律 fail-closed，而不是放行输入。
type Config struct {
	Service                       Service
	JWTManager                    JWTManager
	LegacyClientCredentialsIssuer LegacyClientCredentialsIssuer
	SessionAuthenticator          BrowserSessionAuthenticator
	SessionLogout                 BrowserSessionLogout
	PostLogoutRedirectValidator   PostLogoutRedirectValidator
	AccessTokenSubjectResolver    AccessTokenSubjectResolver
	ExternalAuthorizationVerifier ExternalAuthorizationTokenVerifier
	AuthorizationResolver         tokenissuer.AuthorizationResolver
	AuthorizationContextResolver  tokenissuer.AuthorizationContextResolver
	PersonnelDirectoryResolver    PersonnelDirectoryResolver
	// AllowLegacyPlatformAccessToken 只控制 Keycloak 切换后 authorization-context
	// 是否继续接受平台签发的 access token。标准 UserInfo 端点必须继续接受本授权
	// 服务器签发的令牌，因为 Keycloak Broker 依赖它。
	AllowLegacyPlatformAccessToken bool
	// CustomerBindingResolver 在 EmitCustomerRef 打开时向 authorization-context 响应
	// 追加 customer_ref 声明；实现由外部客户绑定模块注入。
	CustomerBindingResolver CustomerBindingResolver
	EmitCustomerRef         bool
	SessionCookieName       string
	SessionCookieSecure     bool
	SessionCookieSameSite   http.SameSite
	Clock                   Clock
	Logger                  *slog.Logger
}

// Handler 只承载传输层策略；持久化协议状态仍留在 application.Service 中。
type Handler struct {
	service                        Service
	jwtManager                     JWTManager
	legacyIssuer                   LegacyClientCredentialsIssuer
	sessionAuth                    BrowserSessionAuthenticator
	sessionLogout                  BrowserSessionLogout
	logoutRedirects                PostLogoutRedirectValidator
	accessTokenSubjects            AccessTokenSubjectResolver
	externalAuthorizationVerifier  ExternalAuthorizationTokenVerifier
	authorizationResolver          tokenissuer.AuthorizationResolver
	authorizationContextResolver   tokenissuer.AuthorizationContextResolver
	personnelDirectory             PersonnelDirectoryResolver
	allowLegacyPlatformAccessToken bool
	customerBindingResolver        CustomerBindingResolver
	emitCustomerRef                bool
	brokerSelfHealer               BrokerDriftSelfHealer
	cookie                         cookieConfig
	clock                          Clock
	logger                         *slog.Logger
}

// BrokerDriftSelfHealer 修复 Keycloak 侧存储的 broker client secret 与平台活动凭据
// 不再匹配的平台 OAuth broker client。它在 token 端点拒绝 broker 客户端认证时触发，
// 这是平台能观察到 Keycloak 持陈旧 secret 的最早时机。实现必须并发安全并限流修复，
// 避免故障风暴。
type BrokerDriftSelfHealer interface {
	HealBrokerSecretDrift(ctx context.Context, clientID string) error
}

// SetBrokerDriftSelfHealer 安装 broker secret 漂移自愈器；传 nil 表示端点在 broker
// 凭据无效时保持 fail-closed，不做修复尝试。
func (h *Handler) SetBrokerDriftSelfHealer(healer BrokerDriftSelfHealer) {
	h.brokerSelfHealer = healer
}

// CustomerBindingResolver 解析平台身份在同一应用内绑定的 CRM 客户引用。任何错误
// 都意味着该声明从 authorization-context 响应中省略；消费方对缺失值 fail-closed。
type CustomerBindingResolver interface {
	ResolveCustomerBinding(ctx context.Context, tenantID, platformUserID, applicationCode string) (string, error)
}

type cookieConfig struct {
	name     string
	secure   bool
	sameSite http.SameSite
}

// NewHandler 校验必需的协议依赖；具体的 *security.OIDCJWTManager 与
// *oidcapplication.Service 无需适配器即可满足 Config 中的接口。
func NewHandler(config Config) (*Handler, error) {
	if config.Service == nil || config.JWTManager == nil {
		return nil, errors.New("OIDC HTTP handler service and JWT manager must not be nil")
	}
	if strings.TrimSpace(config.JWTManager.Issuer()) == "" {
		return nil, errors.New("OIDC HTTP handler JWT issuer must not be empty")
	}
	if strings.TrimSpace(config.SessionCookieName) == "" {
		return nil, errors.New("OIDC HTTP handler session cookie name must not be empty")
	}
	if config.Clock == nil {
		config.Clock = systemClock{}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Handler{
		service: config.Service, jwtManager: config.JWTManager, legacyIssuer: config.LegacyClientCredentialsIssuer,
		sessionAuth: config.SessionAuthenticator, sessionLogout: config.SessionLogout,
		logoutRedirects: config.PostLogoutRedirectValidator, accessTokenSubjects: config.AccessTokenSubjectResolver,
		externalAuthorizationVerifier: config.ExternalAuthorizationVerifier,
		authorizationResolver:         config.AuthorizationResolver, authorizationContextResolver: config.AuthorizationContextResolver,
		personnelDirectory:             config.PersonnelDirectoryResolver,
		allowLegacyPlatformAccessToken: config.AllowLegacyPlatformAccessToken,
		customerBindingResolver:        config.CustomerBindingResolver,
		emitCustomerRef:                config.EmitCustomerRef && config.CustomerBindingResolver != nil,
		cookie:                         cookieConfig{name: config.SessionCookieName, secure: config.SessionCookieSecure, sameSite: config.SessionCookieSameSite},
		clock:                          config.Clock, logger: config.Logger,
	}, nil
}
