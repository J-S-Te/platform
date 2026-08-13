// Package tokenissuer adapts the shared OIDC JWT signer to the OIDC application token boundary.
package tokenissuer

import (
	"context"
	"errors"
	"fmt"

	oidcapplication "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

// jwtSigner is deliberately narrow so the protocol application layer remains independent from a
// concrete JWT implementation, while this adapter can still be unit tested without key files.
type jwtSigner interface {
	Issuer() string
	IssueAccessToken(security.OIDCTokenClaims) (string, error)
	IssueIDToken(security.OIDCTokenClaims) (string, error)
}

// Issuer signs end-user OAuth access tokens and optional OpenID Connect ID tokens. Refresh tokens
// intentionally remain outside this component because they are opaque secrets managed by the OIDC
// application service and stored only as one-way digests.
type AuthorizationClaims struct {
	TenantID string
	PersonID string
	Roles    []string
}

// AuthorizationResolver resolves the compact identity claims for the OAuth client's application.
// Detailed permissions are served by AuthorizationContextResolver instead.
type AuthorizationResolver interface {
	ResolveOIDCAuthorization(context.Context, string, string, string) (AuthorizationClaims, error)
}

// DataScope is the durable scope boundary of an application role. Detailed
// permission and scope data is served online by the authorization-context API,
// never copied into an OIDC token.
type DataScope struct {
	RoleCode        string
	ScopeType       string
	ScopeID         string
	EnvironmentCode string
}

type AuthorizationContext struct {
	ClientID              string
	ApplicationCode       string
	EnvironmentCode       string
	TenantID              string
	PersonID              string
	Roles                 []string
	Permissions           []string
	DataScopes            []DataScope
	AuthorizationRevision uint64
}

type AuthorizationContextResolver interface {
	ResolveOIDCAuthorizationContext(context.Context, string, string, string) (AuthorizationContext, error)
}

type Issuer struct {
	signer   jwtSigner
	ids      oidcapplication.IDGenerator
	resolver AuthorizationResolver
}

// New creates a token issuer backed by the configured OIDC JWT manager.
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

// Compile-time conformance protects the bootstrap wiring from protocol-boundary drift.
var _ oidcapplication.TokenIssuer = (*Issuer)(nil)
