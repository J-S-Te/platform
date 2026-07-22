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
type Issuer struct {
	signer jwtSigner
	ids    oidcapplication.IDGenerator
}

// New creates a token issuer backed by the configured OIDC JWT manager.
func New(manager *security.OIDCJWTManager, ids oidcapplication.IDGenerator) (*Issuer, error) {
	return newIssuer(manager, ids)
}

func newIssuer(signer jwtSigner, ids oidcapplication.IDGenerator) (*Issuer, error) {
	if signer == nil || ids == nil {
		return nil, errors.New("OIDC token issuer signer and ID generator must not be nil")
	}
	if signer.Issuer() == "" {
		return nil, errors.New("OIDC token issuer issuer must not be empty")
	}
	return &Issuer{signer: signer, ids: ids}, nil
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
