// Package tokenissuer 把共享 OIDC JWT 签名器适配到 OIDC 应用的令牌边界。
package tokenissuer

import (
	"context"
	"errors"
	"fmt"

	oidcapplication "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

// jwtSigner 刻意保持狭窄，使协议应用层独立于具体 JWT 实现，
// 同时本适配器无需密钥文件即可单元测试。
type jwtSigner interface {
	Issuer() string
	IssueAccessToken(security.OIDCTokenClaims) (string, error)
	IssueIDToken(security.OIDCTokenClaims) (string, error)
}

// AuthorizationClaims 是签入 OAuth access token / ID token 的紧凑授权声明。
// refresh token 刻意留在本组件之外：它是由 OIDC 应用服务管理的机密，
// 仅以单向摘要持久化。
type AuthorizationClaims struct {
	TenantID string
	PersonID string
	Roles    []string
}

// AuthorizationResolver 解析 OAuth client 所属应用的紧凑身份声明；
// 详细权限改由 AuthorizationContextResolver 在线提供。
type AuthorizationResolver interface {
	ResolveOIDCAuthorization(context.Context, string, string, string) (AuthorizationClaims, error)
}

// DataScope 是应用角色的持久化数据范围边界。详细权限与范围数据由
// authorization-context API 在线提供，绝不复制进 OIDC token。
type DataScope struct {
	RoleCode        string
	ScopeType       string
	ScopeID         string
	EnvironmentCode string
}

// AuthorizationContext 是 authorization-context 端点返回的完整授权快照：
// 角色、权限、目录兼容窗口与授权修订号，供子系统做撤销与目录变化检测。
type AuthorizationContext struct {
	ClientID                   string
	ApplicationCode            string
	EnvironmentCode            string
	TenantID                   string
	PersonID                   string
	Roles                      []string
	Permissions                []string
	RoleConfigHash             string
	CatalogVersion             string
	CompatibleCatalogVersions  []string
	CompatibleRoleConfigHashes []string
	DataScopes                 []DataScope
	AuthorizationRevision      uint64
}

// AuthorizationContextResolver 按租户/client/用户解析当前应用级授权快照。
type AuthorizationContextResolver interface {
	ResolveOIDCAuthorizationContext(context.Context, string, string, string) (AuthorizationContext, error)
}

// Issuer 用平台 OIDC JWT 管理器签署终端用户的 access token 与可选 ID token。
type Issuer struct {
	signer   jwtSigner
	ids      oidcapplication.IDGenerator
	resolver AuthorizationResolver
}

// New 基于配置的 OIDC JWT 管理器创建令牌签发器。
func New(manager *security.OIDCJWTManager, ids oidcapplication.IDGenerator, resolvers ...AuthorizationResolver) (*Issuer, error) {
	return newIssuer(manager, ids, resolvers...)
}

func newIssuer(signer jwtSigner, ids oidcapplication.IDGenerator, resolvers ...AuthorizationResolver) (*Issuer, error) {
	if signer == nil || ids == nil {
		return nil, errors.New("OIDC token issuer signer and ID generator must not be nil")
	}
	if signer.Issuer() == "" {
		return nil, errors.New("OIDC token issuer issuer must not be empty")
	}
	var resolver AuthorizationResolver
	if len(resolvers) > 1 {
		return nil, errors.New("OIDC token issuer accepts at most one authorization resolver")
	}
	if len(resolvers) == 1 {
		resolver = resolvers[0]
		if resolver == nil {
			return nil, errors.New("OIDC token issuer authorization resolver must not be nil")
		}
	}
	return &Issuer{signer: signer, ids: ids, resolver: resolver}, nil
}

// IssueOIDCTokens 在授权码或刷新授权完成后签名访问令牌和可选 ID Token；刷新令牌始终由应用层
// 以不透明密钥管理，不进入 JWT 适配器，避免把两种生命周期混为一体。
func (issuer *Issuer) IssueOIDCTokens(ctx context.Context, issue oidcapplication.TokenIssue) (oidcapplication.IssuedTokens, error) {
	if issuer == nil || issuer.signer == nil || issuer.ids == nil {
		return oidcapplication.IssuedTokens{}, errors.New("OIDC token issuer is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return oidcapplication.IssuedTokens{}, err
	}

	authorization := AuthorizationClaims{TenantID: issue.TenantID}
	if issuer.resolver != nil {
		resolved, err := issuer.resolver.ResolveOIDCAuthorization(ctx, issue.TenantID, issue.ClientID, issue.UserID)
		if err != nil {
			return oidcapplication.IssuedTokens{}, fmt.Errorf("resolve OIDC application authorization: %w", err)
		}
		// 授权码或刷新令牌中的租户才是权威边界。即使 resolver 是内部适配器，也要把结果按
		// 不可信输入复核，防止装配错误把其他租户的组织和权限声明签进令牌。
		if resolved.TenantID != issue.TenantID {
			return oidcapplication.IssuedTokens{}, errors.New("resolved OIDC authorization tenant does not match token grant")
		}
		authorization = resolved
	}

	claims := security.OIDCTokenClaims{
		Issuer:             issuer.signer.Issuer(),
		Subject:            issue.UserID,
		Audience:           []string{issue.ClientID},
		IssuedAt:           issue.IssuedAt,
		ExpiresAt:          issue.AccessTokenExpiresAt,
		JWTID:              issue.AccessTokenID,
		SessionID:          issue.SessionID,
		AuthenticationTime: issue.AuthorizedAt,
		Scope:              append([]string(nil), issue.Scopes...),
		ClientID:           issue.ClientID,
		Nonce:              issue.Nonce,
		TenantID:           authorization.TenantID,
		PersonID:           authorization.PersonID,
		Roles:              append([]string(nil), authorization.Roles...),
	}
	accessToken, err := issuer.signer.IssueAccessToken(claims)
	if err != nil {
		return oidcapplication.IssuedTokens{}, fmt.Errorf("sign OIDC access token: %w", err)
	}

	result := oidcapplication.IssuedTokens{AccessToken: accessToken}
	if !issue.IssueIDToken {
		return result, nil
	}
	idTokenID, err := issuer.ids.New(issue.IssuedAt)
	if err != nil {
		return oidcapplication.IssuedTokens{}, fmt.Errorf("generate ID token ID: %w", err)
	}
	claims.JWTID = idTokenID
	// ID Token 与 Access Token 共享本次身份/授权快照，但使用独立 jti，便于审计和撤销语义区分。
	idToken, err := issuer.signer.IssueIDToken(claims)
	if err != nil {
		return oidcapplication.IssuedTokens{}, fmt.Errorf("sign OIDC ID token: %w", err)
	}
	result.IDToken = idToken
	return result, nil
}

// 编译期一致性断言保护 bootstrap 装配不偏离协议边界。
var _ oidcapplication.TokenIssuer = (*Issuer)(nil)
