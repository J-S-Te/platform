// Package application 实现机器客户端的 OAuth Client Credentials 签发与 Bearer 令牌认证。
package application

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrUnauthenticated 刻意同时覆盖"客户端不存在"与"secret 错误"两种情况，
	// 使令牌端点无法被用来探测某个 client 标识符是否已注册。
	ErrUnauthenticated = errors.New("application client authentication failed")
	// ErrInvalidGrant 表示客户端存在但未登记所请求的授权类型或认证方式。
	ErrInvalidGrant = errors.New("unsupported OAuth grant")
	// ErrInvalidScope 表示请求的 scope 超出客户端登记集合，按 OAuth 语义必须拒绝而非裁剪。
	ErrInvalidScope = errors.New("requested OAuth scope is not allowed")
)

// Clock 隔离时间源，供服务与测试注入；所有时间戳必须走 UTC。
type Clock interface{ Now() time.Time }

// SystemClock 是生产环境的真实时钟实现，统一返回 UTC。
type SystemClock struct{}

// Now 返回当前 UTC 时间；签发与校验共用同一时间口径，避免时钟漂移误判过期。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Repository 只暴露令牌生命周期所需的两类登记读取：按客户端密钥认证查找、
// 按 ID 查 ACTIVE 客户端。实现不得返回跨租户或含明文 secret 的数据。
type Repository interface {
	FindForClientCredentials(context.Context, string, time.Time) (domain.OAuthClient, []domain.ClientCredential, error)
	FindActiveByID(context.Context, string, time.Time) (domain.OAuthClient, error)
}

// TokenManager 把 Ed25519 JWT 实现与 OAuth 应用服务隔离；
// 服务层只面向 claims 契约，不接触签名细节。
type TokenManager interface {
	Issue(security.ApplicationTokenClaims) (string, error)
	Verify(string, time.Time) (security.ApplicationTokenClaims, error)
}

// TokenResult 是令牌端点的成功响应负载，字段与 OAuth 2.0 响应一一对应。
type TokenResult struct {
	AccessToken string
	ExpiresIn   int64
	Scope       string
}

// Service 签发短时效应用令牌，并在认证时回查数据库确认客户端登记仍然有效。
// 后一次回查使客户端停用/吊销即时生效，无需 Redis 或进程内 denylist。
type Service struct {
	repository Repository
	tokens     TokenManager
	clock      Clock
}

// NewService 装配应用令牌服务；任一依赖缺失即拒绝构造，认证组件不允许半可用。
func NewService(repository Repository, tokens TokenManager, clock Clock) (*Service, error) {
	if repository == nil || tokens == nil || clock == nil {
		return nil, errors.New("application registry service dependencies must not be nil")
	}
	return &Service{repository: repository, tokens: tokens, clock: clock}, nil
}

// IssueClientCredentials 校验 HTTP Basic 客户端认证并签发访问令牌。
// 认证失败统一折叠为 ErrUnauthenticated/ErrInvalidGrant，不区分具体失败环节，
// 防止把错误细 feed 给探测者。
func (s *Service) IssueClientCredentials(ctx context.Context, clientID, clientSecret string, requestedScopes []string) (TokenResult, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || clientSecret == "" {
		return TokenResult{}, ErrUnauthenticated
	}

	now := s.clock.Now().UTC()
	client, credentials, err := s.repository.FindForClientCredentials(ctx, clientID, now)
	if err != nil {
		return TokenResult{}, mapAuthenticationError(err)
	}
	if client.TokenAuthMethod != "client_secret_basic" || !has(client.GrantTypes, "client_credentials") {
		return TokenResult{}, ErrInvalidGrant
	}
	if !verifyAnySecret(credentials, clientSecret) {
		return TokenResult{}, ErrUnauthenticated
	}

	// 空 scope 遵循客户端登记的默认集合；显式 scope 则必须逐项属于登记集合，不能通过
	// token 请求临时扩大机器客户端能力。
	scopes, err := allowedScopes(client.Scopes, requestedScopes)
	if err != nil {
		return TokenResult{}, err
	}
	if client.AccessTokenTTLSeconds == 0 {
		return TokenResult{}, fmt.Errorf("application client %q has an invalid access token TTL", client.ClientID)
	}

	ttl := time.Duration(client.AccessTokenTTLSeconds) * time.Second
	token, err := s.tokens.Issue(security.ApplicationTokenClaims{
		OAuthClientID:   client.ID,
		ClientID:        client.ClientID,
		TenantID:        client.TenantID,
		ApplicationID:   client.ApplicationID,
		ApplicationCode: client.ApplicationCode,
		EnvironmentID:   client.EnvironmentID,
		EnvironmentCode: client.EnvironmentCode,
		Scopes:          scopes,
		IssuedAt:        now,
		ExpiresAt:       now.Add(ttl),
	})
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue application access token: %w", err)
	}
	return TokenResult{AccessToken: token, ExpiresIn: int64(ttl / time.Second), Scope: strings.Join(scopes, " ")}, nil
}

// Authenticate 校验 Bearer 令牌，并确认其引用的应用客户端当前仍为 ACTIVE
// 且仍绑定在同一租户/应用/环境上。JWT 已过期或边界不一致一律 ErrUnauthenticated。
func (s *Service) Authenticate(ctx context.Context, token string) (appctx.Principal, error) {
	claims, err := s.tokens.Verify(strings.TrimSpace(token), s.clock.Now().UTC())
	if err != nil {
		return appctx.Principal{}, ErrUnauthenticated
	}
	client, err := s.repository.FindActiveByID(ctx, claims.OAuthClientID, s.clock.Now().UTC())
	// JWT 签名有效只证明“曾被平台签发”；这里把令牌内边界与当前登记再次对齐，使客户
	// 端禁用、迁移或重新绑定后，旧令牌不能继续代表原资源范围。
	if err != nil || client.ClientID != claims.ClientID || client.TenantID != claims.TenantID ||
		client.ApplicationID != claims.ApplicationID || client.EnvironmentID != claims.EnvironmentID ||
		client.ApplicationCode != claims.ApplicationCode || client.EnvironmentCode != claims.EnvironmentCode {
		return appctx.Principal{}, ErrUnauthenticated
	}

	scopes := make(map[string]struct{}, len(claims.Scopes))
	for _, scope := range claims.Scopes {
		if !has(client.Scopes, scope) {
			return appctx.Principal{}, ErrUnauthenticated
		}
		scopes[scope] = struct{}{}
	}
	return appctx.Principal{
		OAuthClientID:   claims.OAuthClientID,
		ClientID:        claims.ClientID,
		TenantID:        claims.TenantID,
		ApplicationID:   claims.ApplicationID,
		ApplicationCode: claims.ApplicationCode,
		EnvironmentID:   claims.EnvironmentID,
		EnvironmentCode: claims.EnvironmentCode,
		Scopes:          scopes,
	}, nil
}

func mapAuthenticationError(err error) error {
	if errors.Is(err, ErrUnauthenticated) {
		return ErrUnauthenticated
	}
	return err
}

func verifyAnySecret(credentials []domain.ClientCredential, secret string) bool {
	matched := false
	for _, credential := range credentials {
		if len(credential.SecretHash) == 0 {
			continue
		}
		if bcrypt.CompareHashAndPassword(credential.SecretHash, []byte(secret)) == nil {
			matched = true
		}
	}
	return matched
}

func allowedScopes(allowed map[string]struct{}, requested []string) ([]string, error) {
	if len(requested) == 0 {
		requested = make([]string, 0, len(allowed))
		for scope := range allowed {
			requested = append(requested, scope)
		}
	}

	seen := make(map[string]struct{}, len(requested))
	result := make([]string, 0, len(requested))
	for _, scope := range requested {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if !has(allowed, scope) {
			return nil, ErrInvalidScope
		}
		if _, exists := seen[scope]; !exists {
			seen[scope] = struct{}{}
			result = append(result, scope)
		}
	}
	sort.Strings(result)
	return result, nil
}

func has(values map[string]struct{}, value string) bool {
	_, ok := values[value]
	return ok
}
