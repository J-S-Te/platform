// Package application 编排身份认证用例。
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	securityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/security/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

var (
	// 未知账号、错误密码和不可用凭据统一映射为未认证，避免攻击者根据错误差异枚举有效账号。
	ErrUnauthenticated = errors.New("unauthenticated")
	// 锁定窗口内的登录同样返回通用未认证（防枚举），锁定状态由安全模块承载。
	// 并发会话冲突阻止同一登录账号在另一终端再建立一个活动浏览器会话。
	ErrConcurrentSession = errors.New("account already has an active session")

	dummyPasswordDigest   = make([]byte, 32)
	dummyPasswordMetadata = mustDummyPasswordMetadata()
)

// ConcurrentSessionError 在公共 API 只返回稳定冲突码与客户端安全消息的同时，保留受信的
// 账号身份供安全审计日志使用。
type ConcurrentSessionError struct {
	TenantID    string
	UserID      string
	UserName    string
	AccountID   string
	AccountName string
}

// LoginFailedError 在保持公共通用“凭据无效”响应的同时，保留受信账号身份供审计日志使用。
type LoginFailedError struct {
	TenantID    string
	UserID      string
	UserName    string
	AccountID   string
	AccountName string
}

// Error 返回通用“凭据无效”文本；受信账号身份只能经结构化字段进入审计日志。
func (error LoginFailedError) Error() string { return ErrUnauthenticated.Error() }

// Unwrap 固定映射到 ErrUnauthenticated，保证调用方无法从错误链区分“账号不存在”与“密码错误”。
func (error LoginFailedError) Unwrap() error { return ErrUnauthenticated }

// Error 返回稳定冲突码对应的通用文本，不暴露会话细节。
func (error ConcurrentSessionError) Error() string { return ErrConcurrentSession.Error() }

// Unwrap 固定映射到 ErrConcurrentSession，使并发会话冲突可被 errors.Is 识别。
func (error ConcurrentSessionError) Unwrap() error { return ErrConcurrentSession }

// Repository 定义密码登录与会话用例所需的持久化操作。
type Repository interface {
	FindLoginAccount(ctx context.Context, accountName string) (domain.LoginAccount, error)
	FindLoginAccountByIdentityID(ctx context.Context, identityID string) (domain.LoginAccount, error)
	RecordSuccessfulPasswordVerification(ctx context.Context, account domain.LoginAccount, now time.Time) error
	CreateSession(ctx context.Context, account domain.LoginAccount, session domain.Session, idleTimeout time.Duration, replaceExisting bool) error
	FindPrincipalBySession(ctx context.Context, sessionID string, now time.Time, idleTimeout time.Duration) (domain.Principal, error)
	RecordSessionInteraction(ctx context.Context, sessionID string, interactedAt time.Time, idleTimeout time.Duration) error
	RefreshSession(ctx context.Context, sessionID string, refreshedAt time.Time) (time.Time, error)
	RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time, reason string) error
	RevokeAccountSessions(ctx context.Context, tenantID, accountID string, revokedAt time.Time, reason string) error
}

// PasswordVerifier 校验登录请求提供的密码凭据。
type PasswordVerifier interface {
	Verify(password, algorithm string, digest, metadata []byte) (bool, error)
}

// TokenManager 签发并校验服务端持有的 JWT 浏览器 Cookie。
type TokenManager interface {
	Issue(claims security.TokenClaims) (string, error)
	Verify(token string, now time.Time) (security.TokenClaims, error)
}

// IDGenerator 允许测试使用确定性的会话 ID。
type IDGenerator interface {
	New(now time.Time) (string, error)
}

// Clock 暴露当前时间，使过期行为可以被确定性测试。
type Clock interface {
	Now() time.Time
}

// ExternalSessionTerminator 为一个稳定的平台身份吊销外置身份代理持有的会话。实现必须
// 幂等：尚未投影的身份视为已登出。
type ExternalSessionTerminator interface {
	LogoutIdentitySessions(ctx context.Context, identityID string) error
}

// Service 实现密码登录与服务端校验的 Cookie 会话操作。
type Service struct {
	repository       Repository
	passwords        PasswordVerifier
	tokens           TokenManager
	ids              IDGenerator
	clock            Clock
	loginSecurity    securityapplication.LoginSecurityService
	sessionTTL       time.Duration
	externalSessions ExternalSessionTerminator
}

// NewService 校验依赖并构造认证服务；任一必需依赖为 nil 或 sessionTTL 非正时返回错误。
func NewService(repository Repository, passwords PasswordVerifier, tokens TokenManager, ids IDGenerator, clock Clock, loginSecurity securityapplication.LoginSecurityService, sessionTTL time.Duration, externalSessionTerminators ...ExternalSessionTerminator) (*Service, error) {
	if repository == nil || passwords == nil || tokens == nil || ids == nil || clock == nil || loginSecurity == nil {
		return nil, errors.New("identity authentication dependencies must not be nil")
	}
	if len(externalSessionTerminators) > 1 || (len(externalSessionTerminators) == 1 && externalSessionTerminators[0] == nil) {
		return nil, errors.New("identity authentication accepts at most one non-nil external session terminator")
	}
	if sessionTTL <= 0 {
		return nil, errors.New("identity session TTL must be greater than zero")
	}
	service := &Service{repository: repository, passwords: passwords, tokens: tokens, ids: ids, clock: clock, loginSecurity: loginSecurity, sessionTTL: sessionTTL}
	if len(externalSessionTerminators) == 1 {
		service.externalSessions = externalSessionTerminators[0]
	}
	return service, nil
}

// LoginInput 包含已校验的密码登录数据与非敏感的客户端元数据。
type LoginInput struct {
	Account                string
	Password               string
	IPAddress              net.IP
	UserAgent              string
	ReplaceExistingSession bool
	// CurrentPrincipal 是可能已存在的已认证浏览器身份。它只用于在签发新 Cookie 之前，
	// 让账号切换先吊销上一身份的平台与 Keycloak 会话。
	CurrentPrincipal *authctx.Principal
}

// OIDCLoginInput 携带平台所配置 OIDC 提供方已校验的稳定身份。令牌签名与 issuer 校验在
// OIDC HTTP 边界完成，之后才会调用本方法。
type OIDCLoginInput struct {
	IdentityID             string
	AccountName            string
	IPAddress              net.IP
	UserAgent              string
	ReplaceExistingSession bool
	CurrentPrincipal       *authctx.Principal
}

// SessionResult 是非敏感的 API 会话表示，附带 HttpOnly Cookie 值。
type SessionResult struct {
	ExpiresAt          time.Time
	RedirectURL        string
	Token              string
	MustChangePassword bool

	// 身份字段是服务端生命周期审计元数据，有意不进入 HTTP 响应体或浏览器 Cookie。
	SessionID               string
	TenantID                string
	UserID                  string
	UserName                string
	AccountID               string
	AccountName             string
	ReplacedExistingSession bool
}

// Login 校验本地账号的 Argon2id 密码，持久化 iam_session 并签发其 Cookie。未知账号同样
// 消耗一次等价的哈希计算以对抗枚举。
func (service *Service) Login(ctx context.Context, input LoginInput) (SessionResult, error) {
	accountName := strings.TrimSpace(input.Account)
	if accountName == "" || input.Password == "" {
		return SessionResult{}, ErrUnauthenticated
	}

	account, err := service.repository.FindLoginAccount(ctx, accountName)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			service.consumeUnknownAccountPassword(input.Password)
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("find password login account: %w", err)
	}

	now := service.clock.Now().UTC().Truncate(time.Second)
	matched, err := service.passwords.Verify(input.Password, account.HashAlgorithm, account.PasswordHash, account.AlgorithmParams)
	if err != nil {
		return SessionResult{}, fmt.Errorf("verify password credential: %w", err)
	}
	if !isLoginEligible(account, now) {
		return SessionResult{}, loginFailedError(account)
	}
	if !matched {
		_, err := service.loginSecurity.RecordFailedLogin(ctx, securityapplication.LoginFailureInput{
			TenantID: account.TenantID, AccountID: account.AccountID, AccountName: account.AccountName,
			IPAddress: input.IPAddress, UserAgent: input.UserAgent,
		})
		if err != nil {
			return SessionResult{}, fmt.Errorf("record failed password login: %w", err)
		}
		return SessionResult{}, loginFailedError(account)
	}

	if err := service.repository.RecordSuccessfulPasswordVerification(ctx, account, now); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("record successful password verification: %w", err)
	}
	if err := service.terminatePreviousBrowserIdentity(ctx, input.CurrentPrincipal, account.UserID); err != nil {
		return SessionResult{}, err
	}

	return service.createSession(ctx, account, input.IPAddress, input.UserAgent, now, input.ReplaceExistingSession)
}

// LoginOIDC 创建与密码登录相同的服务端平台会话，但不查询密码状态或代理校验结果。
// 代理证据只是接入/切换门禁，绝不是该路径的运行时前置条件。
func (service *Service) LoginOIDC(ctx context.Context, input OIDCLoginInput) (SessionResult, error) {
	identityID := strings.TrimSpace(input.IdentityID)
	if identityID == "" {
		return SessionResult{}, ErrUnauthenticated
	}
	account, err := service.repository.FindLoginAccountByIdentityID(ctx, identityID)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("find OIDC login account: %w", err)
	}
	now := service.clock.Now().UTC().Truncate(time.Second)
	if account.UserID != identityID || !isSessionLoginEligible(account, now) {
		return SessionResult{}, ErrUnauthenticated
	}
	if err := service.terminatePreviousBrowserIdentity(ctx, input.CurrentPrincipal, account.UserID); err != nil {
		return SessionResult{}, err
	}
	return service.createSession(ctx, account, input.IPAddress, input.UserAgent, now, input.ReplaceExistingSession)
}

// terminatePreviousBrowserIdentity 保证平台 Cookie 与 Keycloak Realm 会话属于同一用户。
// 缺少该步骤时，浏览器仍持有 A 的平台会话却以 B 登录，会让 A 在 Keycloak 保持已认证，
// 下一次代理式子系统登录随即以 different_user_authenticated 失败。
func (service *Service) terminatePreviousBrowserIdentity(ctx context.Context, current *authctx.Principal, nextUserID string) error {
	if current == nil || current.User.ID == "" || current.User.ID == nextUserID {
		return nil
	}
	if err := service.Logout(ctx, *current); err != nil {
		return fmt.Errorf("terminate previous browser identity before account switch: %w", err)
	}
	return nil
}

func (service *Service) consumeUnknownAccountPassword(password string) {
	// 对不存在的账号仍执行一次固定参数的 Argon2id 校验，使其计算成本接近真实账号；
	// 结果和校验器错误均不向外暴露，从响应内容与耗时两侧降低账号枚举风险。
	_, _ = service.passwords.Verify(password, "argon2id", dummyPasswordDigest, dummyPasswordMetadata)
}

func mustDummyPasswordMetadata() []byte {
	metadata, err := json.Marshal(security.DefaultArgon2idParams([]byte("fixed-dummy-salt")))
	if err != nil {
		panic("marshal dummy password metadata: " + err.Error())
	}
	return metadata
}

func (service *Service) createSession(ctx context.Context, account domain.LoginAccount, ipAddress net.IP, userAgent string, now time.Time, replaceExisting bool) (SessionResult, error) {
	// 绝对有效期由平台会话 TTL 决定，空闲有效期则读取租户当前安全策略。二者由仓储
	// 同时校验，因此放宽 Cookie/JWT 的 exp 也不能绕过服务端空闲超时。
	idleTimeout, err := service.loginSecurity.SessionIdleTimeout(ctx, account.TenantID)
	if err != nil {
		return SessionResult{}, fmt.Errorf("read session inactivity policy: %w", err)
	}
	if idleTimeout <= 0 {
		return SessionResult{}, errors.New("session inactivity policy must be greater than zero")
	}

	sessionID, err := service.ids.New(now)
	if err != nil {
		return SessionResult{}, fmt.Errorf("generate session ID: %w", err)
	}
	expiresAt := now.Add(service.sessionTTL).UTC().Truncate(time.Second)
	token, err := service.tokens.Issue(security.TokenClaims{
		SessionID: sessionID, UserID: account.UserID, TenantID: account.TenantID, AccountID: account.AccountID,
		IssuedAt: now, ExpiresAt: expiresAt,
	})
	if err != nil {
		return SessionResult{}, fmt.Errorf("issue session token: %w", err)
	}

	session := domain.Session{
		ID: sessionID, TenantID: account.TenantID, AccountID: account.AccountID,
		CreatedAt: now, ExpiresAt: expiresAt, IPAddress: normalizeIP(ipAddress),
		UserAgent: truncateUserAgent(userAgent),
	}
	if err := service.repository.CreateSession(ctx, account, session, idleTimeout, replaceExisting); err != nil {
		if errors.Is(err, ErrConcurrentSession) {
			return SessionResult{}, concurrentSessionError(account)
		}
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("persist login session: %w", err)
	}

	return SessionResult{
		ExpiresAt: expiresAt, RedirectURL: "/", Token: token, MustChangePassword: account.MustChangePassword,
		SessionID: sessionID, TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName,
		ReplacedExistingSession: replaceExisting,
	}, nil
}

func concurrentSessionError(account domain.LoginAccount) ConcurrentSessionError {
	return ConcurrentSessionError{TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName}
}

func loginFailedError(account domain.LoginAccount) LoginFailedError {
	return LoginFailedError{TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName}
}

// Authenticate 不把已签名 JWT 当作完整授权快照：JWT 只提供不可篡改的会话和主体标识，
// 仓储仍会复核当前会话、账号、用户、租户状态，并重新加载平台角色与权限。
func (service *Service) Authenticate(ctx context.Context, token string) (authctx.Principal, error) {
	now := service.clock.Now().UTC()
	claims, err := service.tokens.Verify(token, now)
	if err != nil {
		return authctx.Principal{}, ErrUnauthenticated
	}
	idleTimeout, err := service.loginSecurity.SessionIdleTimeout(ctx, claims.TenantID)
	if err != nil {
		return authctx.Principal{}, fmt.Errorf("read session inactivity policy: %w", err)
	}
	principal, err := service.repository.FindPrincipalBySession(ctx, claims.SessionID, now, idleTimeout)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return authctx.Principal{}, ErrUnauthenticated
		}
		return authctx.Principal{}, fmt.Errorf("find active session principal: %w", err)
	}
	if principal.SessionID != claims.SessionID || principal.Tenant.ID != claims.TenantID ||
		principal.User.ID != claims.UserID || principal.Account.ID != claims.AccountID {
		return authctx.Principal{}, ErrUnauthenticated
	}
	return toAuthContextPrincipal(principal), nil
}

// RecordInteraction 只在浏览器上报可信点击、按键、滚动或触摸后推进交互时间；普通鉴权
// 请求不会续期空闲窗口，避免轮询、后台标签页或恶意请求让无人操作的会话永久存活。
func (service *Service) RecordInteraction(ctx context.Context, principal authctx.Principal) error {
	if principal.SessionID == "" || principal.Tenant.ID == "" || principal.Account.ID == "" {
		return ErrUnauthenticated
	}
	now := service.clock.Now().UTC()
	idleTimeout, err := service.loginSecurity.SessionIdleTimeout(ctx, principal.Tenant.ID)
	if err != nil {
		return fmt.Errorf("read session inactivity policy: %w", err)
	}
	if err := service.repository.RecordSessionInteraction(ctx, principal.SessionID, now, idleTimeout); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			if revokeErr := service.repository.RevokeAccountSessions(ctx, principal.Tenant.ID, principal.Account.ID, now, "IDLE_TIMEOUT"); revokeErr != nil {
				return fmt.Errorf("revoke expired account sessions: %w", revokeErr)
			}
			return ErrUnauthenticated
		}
		return fmt.Errorf("record active browser interaction: %w", err)
	}
	return nil
}

// Refresh 续期当前活跃会话并返回新的 JWT Cookie 值。
func (service *Service) Refresh(ctx context.Context, principal authctx.Principal) (SessionResult, error) {
	if principal.SessionID == "" || principal.Tenant.ID == "" || principal.User.ID == "" || principal.Account.ID == "" {
		return SessionResult{}, ErrUnauthenticated
	}
	now := service.clock.Now().UTC().Truncate(time.Second)
	expiresAt, err := service.repository.RefreshSession(ctx, principal.SessionID, now)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("refresh active session: %w", err)
	}
	if !expiresAt.After(now) {
		return SessionResult{}, ErrUnauthenticated
	}
	token, err := service.tokens.Issue(security.TokenClaims{
		SessionID: principal.SessionID, UserID: principal.User.ID, TenantID: principal.Tenant.ID,
		AccountID: principal.Account.ID, IssuedAt: now, ExpiresAt: expiresAt,
	})
	if err != nil {
		return SessionResult{}, fmt.Errorf("issue refreshed session token: %w", err)
	}
	return SessionResult{ExpiresAt: expiresAt, RedirectURL: "/", Token: token}, nil
}

// Logout 按租户账号撤销全部活动会话，而不是只撤销当前 SessionID；从任一子系统退出后，
// 同一统一登录会话在其他入口也会立即失效，保持单点退出语义。
func (service *Service) Logout(ctx context.Context, principal authctx.Principal) error {
	if principal.SessionID == "" || principal.Tenant.ID == "" || principal.User.ID == "" || principal.Account.ID == "" {
		return ErrUnauthenticated
	}
	// 先吊销代理会话；Keycloak 不可用时失败关闭，保留仍有效的平台会话，避免在旧的
	// Realm 会话尚存时浏览器切换到另一个平台账号。
	if service.externalSessions != nil {
		if err := service.externalSessions.LogoutIdentitySessions(ctx, principal.User.ID); err != nil {
			return fmt.Errorf("revoke external identity sessions: %w", err)
		}
	}
	if err := service.repository.RevokeAccountSessions(ctx, principal.Tenant.ID, principal.Account.ID, service.clock.Now().UTC(), "GLOBAL_LOGOUT"); err != nil {
		return fmt.Errorf("revoke account sessions: %w", err)
	}
	return nil
}

// SessionIdleTimeout 返回当前租户的浏览器生命周期空闲超时。
func (service *Service) SessionIdleTimeout(ctx context.Context, tenantID string) (time.Duration, error) {
	if strings.TrimSpace(tenantID) == "" {
		return 0, ErrUnauthenticated
	}
	timeout, err := service.loginSecurity.SessionIdleTimeout(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("read session inactivity policy: %w", err)
	}
	return timeout, nil
}

func isLoginEligible(account domain.LoginAccount, now time.Time) bool {
	if !isSessionLoginEligible(account, now) || account.CredentialStatus != domain.StatusActive {
		return false
	}
	return account.CredentialExpiry == nil || account.CredentialExpiry.After(now)
}

// isSessionLoginEligible 汇集所有创建平台会话的认证方式共享的登录条件。锁定检查放在这里，
// 使本地密码连续失败后被锁定的账号也无法经 OIDC 建立新会话。
func isSessionLoginEligible(account domain.LoginAccount, now time.Time) bool {
	return isIdentityEligible(account) && (account.LockedUntil == nil || !account.LockedUntil.After(now))
}

func isIdentityEligible(account domain.LoginAccount) bool {
	return account.TenantStatus == domain.StatusActive &&
		account.UserStatus == domain.StatusActive &&
		account.AccountStatus == domain.StatusActive
}

func normalizeIP(ip net.IP) []byte {
	if ip == nil {
		return nil
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return append([]byte(nil), ipv4...)
	}
	if ipv6 := ip.To16(); ipv6 != nil {
		return append([]byte(nil), ipv6...)
	}
	return nil
}

func truncateUserAgent(userAgent string) string {
	const maxUserAgentBytes = 1000
	if len(userAgent) <= maxUserAgentBytes {
		return userAgent
	}
	return userAgent[:maxUserAgentBytes]
}

func toAuthContextPrincipal(principal domain.Principal) authctx.Principal {
	roles := make([]authctx.ReferenceName, len(principal.Roles))
	for index, role := range principal.Roles {
		roles[index] = authctx.ReferenceName{ID: role.ID, Name: role.Name, Code: role.Code}
	}
	// 审计与展示依赖 User.Name（display_name）。历史/投影账号可能没有 display_name，
	// 此时回退到 Account.Name（COALESCE(username, id) 恒有值），避免操作人/用户名落空。
	userName := principal.User.Name
	if strings.TrimSpace(userName) == "" {
		userName = principal.Account.Name
	}
	return authctx.Principal{
		SessionID:       principal.SessionID,
		LoginIP:         append(net.IP(nil), principal.LoginIP...),
		Tenant:          authctx.ReferenceName{ID: principal.Tenant.ID, Name: principal.Tenant.Name, Code: principal.Tenant.Code},
		User:            authctx.ReferenceName{ID: principal.User.ID, Name: userName, Code: principal.User.Code},
		Account:         authctx.ReferenceName{ID: principal.Account.ID, Name: principal.Account.Name, Code: principal.Account.Code},
		Roles:           roles,
		PermissionCodes: append([]string(nil), principal.PermissionCodes...),
	}
}
