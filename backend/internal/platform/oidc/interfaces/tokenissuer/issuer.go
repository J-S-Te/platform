// Package tokenissuer adapts the shared OIDC JWT signer to the OIDC application token boundary.
package tokenissuer

import (
	"context"
	"errors"
	"fmt"

	oidcapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
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
	TenantID        string
	PersonID        string
	PrimaryOrgID    string
	OrganizationIDs []string
	Roles           []string
	Permissions     []string
	RoleConfigHash  string
	AuthzRevision   uint64
}

// AuthorizationResolver resolves permissions for the OAuth client's application.
// Implementations must not return permissions belonging to a different application.
type AuthorizationResolver interface {
	ResolveOIDCAuthorization(context.Context, string, string, string) (AuthorizationClaims, error)
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

// IssueOIDCTokens implements application.TokenIssuer after authorization-code or refresh-grant
// validation. It never receives or emits a refresh-token secret.
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
		// The authorization-code or refresh-token grant is the authoritative
		// tenant boundary. A resolver is an internal adapter, but its result must
		// still be treated as untrusted at this boundary so a wiring regression
		// cannot sign a token carrying another tenant's organization claims.
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
		PrimaryOrgID:       authorization.PrimaryOrgID,
		OrganizationIDs:    append([]string(nil), authorization.OrganizationIDs...),
		Roles:              append([]string(nil), authorization.Roles...),
		Permissions:        append([]string(nil), authorization.Permissions...),
		RoleConfigHash:     authorization.RoleConfigHash,
		AuthzRevision:      authorization.AuthzRevision,
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
	idToken, err := issuer.signer.IssueIDToken(claims)
	if err != nil {
		return oidcapplication.IssuedTokens{}, fmt.Errorf("sign OIDC ID token: %w", err)
	}
	result.IDToken = idToken
	return result, nil
}

// Compile-time conformance protects the bootstrap wiring from protocol-boundary drift.
var _ oidcapplication.TokenIssuer = (*Issuer)(nil)
