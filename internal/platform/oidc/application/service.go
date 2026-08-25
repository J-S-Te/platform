package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	"golang.org/x/crypto/bcrypt"
)

const maxAuthorizationCodeTTL = 10 * time.Minute

// Service 编排授权码、PKCE、刷新令牌轮换、撤销与 UserInfo 的状态转换。
// HTTP 参数解析、发现文档、JWKS 和 JWT 编码刻意留在边界适配器中，避免协议传输细节渗入事务规则。
type Service struct {
	repository           Repository
	issuer               TokenIssuer
	ids                  IDGenerator
	secrets              SecretGenerator
	clock                Clock
	authorizationCodeTTL time.Duration
}

// NewService validates dependencies and creates an OIDC/OAuth application service.
func NewService(repository Repository, issuer TokenIssuer, ids IDGenerator, secrets SecretGenerator, clock Clock, authorizationCodeTTL time.Duration) (*Service, error) {
	if repository == nil || issuer == nil || ids == nil || secrets == nil || clock == nil {
		return nil, errors.New("OIDC/OAuth service dependencies must not be nil")
	}
	if authorizationCodeTTL <= 0 || authorizationCodeTTL > maxAuthorizationCodeTTL {
		return nil, fmt.Errorf("OIDC/OAuth authorization code TTL must be within (0, %s]", maxAuthorizationCodeTTL)
	}
	return &Service{
		repository: repository, issuer: issuer, ids: ids, secrets: secrets, clock: clock,
		authorizationCodeTTL: authorizationCodeTTL,
	}, nil
}

// IsRegisteredRedirectURI reports whether redirectURI exactly matches an active client's registered URI.
// It intentionally fails closed for missing or disabled clients and never performs prefix matching.
func (service *Service) IsRegisteredRedirectURI(ctx context.Context, clientID, redirectURI string) (bool, error) {
	clientID, redirectURI = strings.TrimSpace(clientID), strings.TrimSpace(redirectURI)
	if clientID == "" || redirectURI == "" {
		return false, nil
	}
	client, err := service.repository.FindClient(ctx, clientID, service.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load OIDC/OAuth client redirect registrations: %w", err)
	}
	_, registered := client.RedirectURIs[redirectURI]
	return registered, nil
}

// Authorize 只有在回调地址精确命中登记值、客户端允许授权码模式且浏览器会话仍属于同一租户时，
// 才签发一次性授权码。数据库只保存摘要，原始授权码仅经浏览器回传给客户端。
func (service *Service) Authorize(ctx context.Context, input AuthorizationInput) (AuthorizationResult, error) {
	input = normalizeAuthorizationInput(input)
	if input.ClientID == "" || input.RedirectURI == "" || input.SessionID == "" || !validProtocolText(input.State, 2048) || !validProtocolText(input.Nonce, 255) {
		return AuthorizationResult{}, ErrInvalidRequest
	}

	now := service.now()
	client, err := service.repository.FindClient(ctx, input.ClientID, now)
	if err != nil {
		return AuthorizationResult{}, mapClientError(err)
	}
	if !has(client.GrantTypes, "authorization_code") {
		return AuthorizationResult{}, ErrUnauthorizedClient
	}
	if _, exists := client.RedirectURIs[input.RedirectURI]; !exists {
		return AuthorizationResult{}, ErrInvalidRequest
	}
	scopes, err := registeredScopes(client, input.Scopes)
	if err != nil {
		return AuthorizationResult{}, err
	}
	challenge, method, err := validatePKCE(client.RequirePKCE, input.CodeChallenge, input.CodeChallengeMethod)
	if err != nil {
		return AuthorizationResult{}, err
	}

	subject, err := service.repository.ResolveSessionSubject(ctx, input.SessionID, now)
	if err != nil {
		return AuthorizationResult{}, mapGrantError(err)
	}
	if subject.TenantID != client.TenantID || !subject.ExpiresAt.After(now) {
		return AuthorizationResult{}, ErrInvalidGrant
	}
	// 首次登录仍需修改初始密码的账号只能保留平台本地会话，用于完成改密；在改密完成前
	// 不得向任何下游应用签发授权码，避免客户凭固定初始口令直接进入业务系统。
	if subject.MustChangePassword {
		return AuthorizationResult{}, ErrAccessDenied
	}

	codeID, err := service.ids.New(now)
	if err != nil {
		return AuthorizationResult{}, fmt.Errorf("generate authorization code id: %w", err)
	}
	rawCode, err := service.secrets.NewSecret()
	if err != nil {
		return AuthorizationResult{}, fmt.Errorf("generate authorization code: %w", err)
	}
	if !validOpaqueSecret(rawCode) {
		return AuthorizationResult{}, errors.New("OIDC/OAuth secret generator returned an invalid authorization code")
	}
	expiresAt := now.Add(service.authorizationCodeTTL)
	code := domain.AuthorizationCode{
		ID: codeID, TenantID: subject.TenantID, OAuthClientID: client.ID, SessionID: subject.SessionID,
		AccountID: subject.AccountID, UserID: subject.UserID, CodeHash: digest(rawCode), RedirectURI: input.RedirectURI,
		Scopes: scopes, Nonce: input.Nonce, CodeChallenge: challenge, CodeChallengeMethod: method,
		CreatedAt: now, ExpiresAt: expiresAt, Status: domain.AuthorizationCodeStatusActive,
	}
	// state 不属于平台授权状态，也不进入授权码记录；平台仅原样回送，供客户端关联发起登录的浏览器会话。
	if err := service.repository.CreateAuthorizationCode(ctx, code); err != nil {
		return AuthorizationResult{}, fmt.Errorf("persist authorization code: %w", err)
	}
	return AuthorizationResult{
		AuthorizationCode: rawCode, RedirectURI: input.RedirectURI, Scope: strings.Join(scopes, " "), State: input.State, ExpiresAt: expiresAt,
	}, nil
}

// ExchangeAuthorizationCode 先校验客户端、回调地址、PKCE 与原浏览器会话，再由仓储原子消费授权码。
// 并发兑换可以同时完成无副作用的预计算，但只有持有行锁并成功提交的一方能获得响应和持久刷新令牌。
func (service *Service) ExchangeAuthorizationCode(ctx context.Context, input AuthorizationCodeExchangeInput) (TokenResult, error) {
	input = normalizeCodeExchangeInput(input)
	if input.ClientID == "" || input.Code == "" || input.RedirectURI == "" {
		return TokenResult{}, ErrInvalidRequest
	}
	now := service.now()
	client, err := service.authenticatedClient(ctx, input.ClientAuthentication, now)
	if err != nil {
		return TokenResult{}, err
	}
	if !has(client.GrantTypes, "authorization_code") {
		return TokenResult{}, ErrUnauthorizedClient
	}
	codeHash := digest(input.Code)
	code, err := service.repository.FindAuthorizationCode(ctx, codeHash, now)
	if err != nil {
		return TokenResult{}, mapGrantError(err)
	}
	if !validAuthorizationCode(code, client, input.RedirectURI, now) || !verifyPKCE(code.CodeChallenge, code.CodeChallengeMethod, input.CodeVerifier) {
		return TokenResult{}, ErrInvalidGrant
	}
	if err := service.validateGrantSession(ctx, code.TenantID, code.SessionID, code.AccountID, code.UserID, now); err != nil {
		return TokenResult{}, err
	}

	preview := grantFromAuthorizationCode(code, client)
	refresh, rawRefresh, err := service.newRefreshToken(client, preview, "", now)
	if err != nil {
		return TokenResult{}, err
	}
	result, err := service.issueTokens(ctx, client, preview, rawRefresh, now)
	if err != nil {
		return TokenResult{}, err
	}
	committedGrant, err := service.repository.ConsumeAuthorizationCode(ctx, ConsumeAuthorizationCodeCommand{
		CodeHash: codeHash, ClientID: client.ID, RedirectURI: input.RedirectURI, Refresh: refresh,
	}, now)
	if err != nil {
		return TokenResult{}, mapGrantError(err)
	}
	if !sameGrant(preview, committedGrant) {
		// 仓储在锁内重新读取客户端、会话和授权码；结果不一致说明预校验与提交边界发生异常漂移，
		// 此时不能把已预计算的令牌响应交给调用方。
		return TokenResult{}, errors.New("authorization code consumption returned a mismatched grant")
	}
	return result, nil
}

// Refresh 每次使用后都会生成后继节点并消费当前节点。再次提交已消费节点被视为密钥泄露信号，
// 仓储会在锁定令牌族后撤销整条后代链，而不是只拒绝这一次请求。
func (service *Service) Refresh(ctx context.Context, input RefreshTokenInput) (TokenResult, error) {
	input = normalizeRefreshInput(input)
	if input.ClientID == "" || input.RefreshToken == "" {
		return TokenResult{}, ErrInvalidRequest
	}
	now := service.now()
	client, err := service.authenticatedClient(ctx, input.ClientAuthentication, now)
	if err != nil {
		return TokenResult{}, err
	}
	if !has(client.GrantTypes, "refresh_token") {
		return TokenResult{}, ErrUnauthorizedClient
	}
	tokenHash := digest(input.RefreshToken)
	current, err := service.repository.FindRefreshToken(ctx, tokenHash, now)
	if err != nil {
		return TokenResult{}, mapGrantError(err)
	}
	if current.OAuthClientID != client.ID || current.TenantID != client.TenantID {
		return TokenResult{}, ErrInvalidGrant
	}
	// 刷新令牌继承最初浏览器 SSO 会话的存续条件。即使历史数据尚未级联清理令牌族，
	// 全局退出、账号禁用或会话过期也必须立即阻止继续签发。
	if err := service.validateGrantSession(ctx, current.TenantID, current.SessionID, current.AccountID, current.UserID, now); err != nil {
		return TokenResult{}, err
	}
	preview := grantFromRefreshToken(current, client)
	if !validRefreshToken(current, now) {
		// 已消费节点不能在应用层直接返回：必须进入仓储的事务分支，取得令牌族锁并完成整族撤销，
		// 然后才向协议层返回不泄漏内部状态的重放错误。
		if current.Status == domain.RefreshTokenStatusConsumed || current.UsedAt != nil {
			replayRefresh, _, generateErr := service.newRefreshToken(client, preview, current.ID, now)
			if generateErr != nil {
				return TokenResult{}, generateErr
			}
			if replayRefresh == nil {
				return TokenResult{}, ErrInvalidGrant
			}
			_, replayErr := service.repository.RotateRefreshToken(ctx, RotateRefreshTokenCommand{
				TokenHash: tokenHash, ClientID: client.ID, Refresh: *replayRefresh,
			}, now)
			if errors.Is(replayErr, ErrRefreshTokenReplay) {
				return TokenResult{}, ErrRefreshTokenReplay
			}
			return TokenResult{}, mapGrantError(replayErr)
		}
		return TokenResult{}, ErrInvalidGrant
	}
	refresh, rawRefresh, err := service.newRefreshToken(client, preview, current.ID, now)
	if err != nil {
		return TokenResult{}, err
	}
	result, err := service.issueTokens(ctx, client, preview, rawRefresh, now)
	if err != nil {
		return TokenResult{}, err
	}
	committedGrant, err := service.repository.RotateRefreshToken(ctx, RotateRefreshTokenCommand{
		TokenHash: tokenHash, ClientID: client.ID, Refresh: *refresh,
	}, now)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenReplay) {
			return TokenResult{}, ErrRefreshTokenReplay
		}
		return TokenResult{}, mapGrantError(err)
	}
	if !sameGrant(preview, committedGrant) {
		return TokenResult{}, errors.New("refresh token rotation returned a mismatched grant")
	}
	return result, nil
}

func (service *Service) validateGrantSession(ctx context.Context, tenantID, sessionID, accountID, userID string, now time.Time) error {
	// 令牌族保存的四元组必须与当前会话完全一致，不能仅凭 session_id 存在就续期；
	// 这同时防止跨租户碰撞和会话记录被重新绑定后继续使用旧授权。
	subject, err := service.repository.ResolveSessionSubject(ctx, sessionID, now)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrInvalidGrant
		}
		return fmt.Errorf("resolve OAuth grant browser session: %w", err)
	}
	if subject.TenantID != tenantID || subject.SessionID != sessionID || subject.AccountID != accountID ||
		subject.UserID != userID || !subject.ExpiresAt.After(now) || subject.MustChangePassword {
		return ErrInvalidGrant
	}
	return nil
}

// Revoke persists a token revocation. Refresh-token revocation invalidates the complete family;
// unknown tokens still return success in accordance with RFC 7009 and are stored as access-token
// digests only when the caller explicitly identifies them as access tokens.
func (service *Service) Revoke(ctx context.Context, input RevokeTokenInput) error {
	input = normalizeRevokeInput(input)
	if input.ClientID == "" || input.Token == "" || !oneOf(input.TokenType, domain.TokenTypeAccess, domain.TokenTypeRefresh) || !validProtocolText(input.Reason, 128) {
		return ErrInvalidRequest
	}
	now := service.now()
	client, err := service.authenticatedClient(ctx, input.ClientAuthentication, now)
	if err != nil {
		return err
	}
	var expiresAt *time.Time
	if input.ExpiresAt != nil {
		expires := input.ExpiresAt.UTC()
		expiresAt = &expires
	}
	revocationID, err := service.ids.New(now)
	if err != nil {
		return fmt.Errorf("generate OIDC/OAuth revocation id: %w", err)
	}
	if err := service.repository.RevokeToken(ctx, RevokeTokenCommand{
		RevocationID: revocationID, TenantID: client.TenantID, OAuthClientID: client.ID, TokenHash: digest(input.Token), TokenType: input.TokenType,
		ExpiresAt: expiresAt, Reason: input.Reason,
	}, now); err != nil {
		return fmt.Errorf("revoke OIDC/OAuth token: %w", err)
	}
	return nil
}

// IsAccessTokenRevoked hashes an opaque bearer value locally and reads the tenant-scoped durable
// revocation record. Callers should invoke it after signature and claim validation.
func (service *Service) IsAccessTokenRevoked(ctx context.Context, tenantID, rawAccessToken string) (bool, error) {
	tenantID, rawAccessToken = strings.TrimSpace(tenantID), strings.TrimSpace(rawAccessToken)
	if tenantID == "" || rawAccessToken == "" {
		return false, ErrInvalidRequest
	}
	revoked, err := service.repository.IsTokenRevoked(ctx, tenantID, digest(rawAccessToken), service.now())
	if err != nil {
		return false, fmt.Errorf("look up OIDC/OAuth token revocation: %w", err)
	}
	return revoked, nil
}

// ResolveUserInfo 不把已验签 JWT 当成永久身份快照，而是重新确认客户端、会话、账号、用户和租户均有效；
// 最终披露字段仍受令牌 scope 限制，profile 与 email 不能互相隐式扩权。
func (service *Service) ResolveUserInfo(ctx context.Context, input UserInfoInput) (UserInfo, error) {
	input = normalizeUserInfoInput(input)
	if input.TenantID == "" || input.OAuthClientID == "" || input.SessionID == "" || input.UserID == "" || !hasSlice(input.Scopes, "openid") {
		return UserInfo{}, ErrInvalidToken
	}
	subject, err := service.repository.ResolveUserInfo(ctx, UserInfoQuery{
		TenantID: input.TenantID, OAuthClientID: input.OAuthClientID, SessionID: input.SessionID, UserID: input.UserID,
	}, service.now())
	if err != nil {
		return UserInfo{}, ErrInvalidToken
	}
	if subject.TenantID != input.TenantID || subject.OAuthClientID != input.OAuthClientID || subject.SessionID != input.SessionID || subject.UserID != input.UserID {
		return UserInfo{}, ErrInvalidToken
	}
	result := UserInfo{Subject: subject.UserID}
	if hasSlice(input.Scopes, "profile") {
		result.Name, result.PreferredUsername = subject.DisplayName, subject.PreferredUsername
	}
	if hasSlice(input.Scopes, "email") {
		result.Email = subject.Email
	}
	return result, nil
}

func (service *Service) authenticatedClient(ctx context.Context, input ClientAuthentication, now time.Time) (domain.Client, error) {
	clientID := strings.TrimSpace(input.ClientID)
	if clientID == "" {
		return domain.Client{}, ErrInvalidClient
	}
	client, err := service.repository.FindClient(ctx, clientID, now)
	if err != nil {
		return domain.Client{}, mapClientError(err)
	}
	switch client.TokenAuthMethod {
	// 认证方法由登记数据决定，请求不能通过同时携带多种凭据来协商或降级认证方式。
	case "none":
		if strings.TrimSpace(input.ClientSecret) != "" || client.ClientType != "public" {
			return domain.Client{}, ErrInvalidClient
		}
	case "client_secret_basic":
		if input.ClientSecret == "" || !verifyClientSecret(client.Credentials, input.ClientSecret, now) {
			return domain.Client{}, ErrInvalidClient
		}
	case "private_key_jwt":
		if input.ClientSecret != "" || authenticatePrivateKeyJWT(ctx, service.repository, client, input, now) != nil {
			return domain.Client{}, ErrInvalidClient
		}
	default:
		return domain.Client{}, ErrInvalidClient
	}
	return client, nil
}

func (service *Service) newRefreshToken(client domain.Client, grant domain.TokenGrant, parentID string, now time.Time) (*NewRefreshToken, string, error) {
	if !has(client.GrantTypes, "refresh_token") || client.RefreshTokenTTLSeconds == 0 {
		return nil, "", nil
	}
	familyID := ""
	if parentID == "" {
		var err error
		familyID, err = service.ids.New(now)
		if err != nil {
			return nil, "", fmt.Errorf("generate refresh token family id: %w", err)
		}
	}
	refreshID, err := service.ids.New(now)
	if err != nil {
		return nil, "", fmt.Errorf("generate refresh token id: %w", err)
	}
	rawRefresh, err := service.secrets.NewSecret()
	if err != nil {
		return nil, "", fmt.Errorf("generate refresh token: %w", err)
	}
	if !validOpaqueSecret(rawRefresh) {
		return nil, "", errors.New("OIDC/OAuth secret generator returned an invalid refresh token")
	}
	expiresAt := now.Add(time.Duration(client.RefreshTokenTTLSeconds) * time.Second)
	return &NewRefreshToken{
		ID: refreshID, TokenFamilyID: familyID, ParentRefreshTokenID: parentID, TokenHash: digest(rawRefresh),
		TenantID: grant.TenantID, OAuthClientID: grant.OAuthClientID, SessionID: grant.SessionID, AccountID: grant.AccountID,
		UserID: grant.UserID, Scopes: append([]string(nil), grant.Scopes...), AuthorizedAt: grant.AuthorizedAt, IssuedAt: now, ExpiresAt: expiresAt,
	}, rawRefresh, nil
}

func (service *Service) issueTokens(ctx context.Context, client domain.Client, grant domain.TokenGrant, rawRefresh string, now time.Time) (TokenResult, error) {
	if client.AccessTokenTTLSeconds == 0 {
		return TokenResult{}, errors.New("OIDC/OAuth client access token TTL must be greater than zero")
	}
	accessTokenID, err := service.ids.New(now)
	if err != nil {
		return TokenResult{}, fmt.Errorf("generate access token id: %w", err)
	}
	ttl := time.Duration(client.AccessTokenTTLSeconds) * time.Second
	issued, err := service.issuer.IssueOIDCTokens(ctx, TokenIssue{
		AccessTokenID: accessTokenID, TenantID: grant.TenantID, OAuthClientID: grant.OAuthClientID, ClientID: client.ClientID,
		SessionID: grant.SessionID, AccountID: grant.AccountID, UserID: grant.UserID, Scopes: append([]string(nil), grant.Scopes...),
		Nonce: grant.Nonce, AuthorizedAt: grant.AuthorizedAt, IssuedAt: now, AccessTokenExpiresAt: now.Add(ttl),
		IssueIDToken: hasSlice(grant.Scopes, "openid"),
	})
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue OIDC/OAuth tokens: %w", err)
	}
	if strings.TrimSpace(issued.AccessToken) == "" || (hasSlice(grant.Scopes, "openid") && strings.TrimSpace(issued.IDToken) == "") {
		return TokenResult{}, errors.New("OIDC/OAuth token issuer returned incomplete tokens")
	}
	return TokenResult{
		AccessToken: issued.AccessToken, TokenType: "Bearer", ExpiresIn: int64(ttl / time.Second), Scope: strings.Join(grant.Scopes, " "),
		IDToken: issued.IDToken, RefreshToken: rawRefresh,
	}, nil
}

func (service *Service) now() time.Time { return service.clock.Now().UTC() }

func digest(value string) [sha256.Size]byte { return sha256.Sum256([]byte(value)) }

func grantFromAuthorizationCode(code domain.AuthorizationCode, client domain.Client) domain.TokenGrant {
	return domain.TokenGrant{
		TenantID: code.TenantID, OAuthClientID: code.OAuthClientID, ClientID: client.ClientID, SessionID: code.SessionID,
		AccountID: code.AccountID, UserID: code.UserID, Scopes: append([]string(nil), code.Scopes...), Nonce: code.Nonce, AuthorizedAt: code.CreatedAt,
	}
}

func grantFromRefreshToken(token domain.RefreshToken, client domain.Client) domain.TokenGrant {
	return domain.TokenGrant{
		TenantID: token.TenantID, OAuthClientID: token.OAuthClientID, ClientID: client.ClientID, SessionID: token.SessionID,
		AccountID: token.AccountID, UserID: token.UserID, Scopes: append([]string(nil), token.Scopes...), AuthorizedAt: token.AuthorizedAt,
	}
}

func sameGrant(left, right domain.TokenGrant) bool {
	if left.TenantID != right.TenantID || left.OAuthClientID != right.OAuthClientID || left.ClientID != right.ClientID ||
		left.SessionID != right.SessionID || left.AccountID != right.AccountID || left.UserID != right.UserID || left.Nonce != right.Nonce ||
		!left.AuthorizedAt.Equal(right.AuthorizedAt) || len(left.Scopes) != len(right.Scopes) {
		return false
	}
	for index := range left.Scopes {
		if left.Scopes[index] != right.Scopes[index] {
			return false
		}
	}
	return true
}

func verifyClientSecret(credentials []domain.ClientCredential, secret string, now time.Time) bool {
	for _, credential := range credentials {
		if len(credential.SecretHash) == 0 || (credential.ValidUntil != nil && !credential.ValidUntil.After(now)) {
			continue
		}
		if bcrypt.CompareHashAndPassword(credential.SecretHash, []byte(secret)) == nil {
			return true
		}
	}
	return false
}

func registeredScopes(client domain.Client, requested []string) ([]string, error) {
	scopes := normalizeScopes(requested)
	if len(scopes) == 0 {
		for scope := range client.Scopes {
			scopes = append(scopes, scope)
		}
		sort.Strings(scopes)
	}
	for _, scope := range scopes {
		if _, allowed := client.Scopes[scope]; !allowed {
			return nil, ErrInvalidScope
		}
	}
	return scopes, nil
}

func validAuthorizationCode(code domain.AuthorizationCode, client domain.Client, redirectURI string, now time.Time) bool {
	return code.TenantID == client.TenantID && code.OAuthClientID == client.ID && code.RedirectURI == redirectURI &&
		code.Status == domain.AuthorizationCodeStatusActive && code.ConsumedAt == nil && code.ExpiresAt.After(now) &&
		code.SessionID != "" && code.AccountID != "" && code.UserID != ""
}

func validRefreshToken(token domain.RefreshToken, now time.Time) bool {
	return token.Status == domain.RefreshTokenStatusActive && token.UsedAt == nil && token.RevokedAt == nil && token.ExpiresAt.After(now) &&
		token.SessionID != "" && token.AccountID != "" && token.UserID != ""
}

func mapClientError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidClient
	}
	return err
}

func mapGrantError(err error) error {
	if errors.Is(err, ErrRefreshTokenReplay) {
		return ErrRefreshTokenReplay
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrAuthorizationCodeUnavailable) {
		return ErrInvalidGrant
	}
	return err
}
