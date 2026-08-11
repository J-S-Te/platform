// Package application coordinates OIDC/OAuth protocol state without HTTP dependencies.
package application

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
)

var (
	// ErrInvalidRequest identifies malformed protocol input.
	ErrInvalidRequest = errors.New("invalid OIDC/OAuth request")
	// ErrInvalidClient deliberately covers unknown, disabled, and improperly authenticated clients.
	ErrInvalidClient = errors.New("invalid OIDC/OAuth client")
	// ErrUnauthorizedClient means a valid client is not registered for the requested grant.
	ErrUnauthorizedClient = errors.New("OIDC/OAuth client is not authorized for this grant")
	// ErrAccessDenied means the authenticated resource owner has no application-level access.
	// Transport adapters must expose only the standard OAuth error code and must not leak role data.
	ErrAccessDenied = errors.New("OIDC/OAuth resource owner access is denied")
	// ErrInvalidScope identifies an unregistered or malformed requested scope.
	ErrInvalidScope = errors.New("invalid OIDC/OAuth scope")
	// ErrInvalidGrant deliberately covers expired, consumed, mismatched, and unknown grants.
	ErrInvalidGrant = errors.New("invalid OIDC/OAuth grant")
	// ErrRefreshTokenReplay indicates a prior refresh token was presented again. Its token family is
	// revoked before this error is returned; transport should map it to invalid_grant.
	ErrRefreshTokenReplay = errors.New("OIDC/OAuth refresh token replay detected")
	// ErrInvalidToken identifies a token or subject that cannot be used for UserInfo.
	ErrInvalidToken = errors.New("invalid OIDC/OAuth token")

	// ErrNotFound is returned by repositories for an absent tenant-scoped record. Application
	// methods map it to a public protocol-safe error instead of exposing record existence.
	ErrNotFound = errors.New("OIDC/OAuth record not found")
	// ErrAuthorizationCodeUnavailable is returned atomically when a code is expired or consumed.
	ErrAuthorizationCodeUnavailable = errors.New("OIDC/OAuth authorization code is unavailable")
)

// Clock supplies UTC protocol time and enables deterministic expiry tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock implementation.
type SystemClock struct{}

// Now returns the current UTC instant.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// IDGenerator supplies durable aggregate IDs. Production callers can use the shared ULID generator.
type IDGenerator interface {
	New(at time.Time) (string, error)
}

// SecretGenerator supplies a cryptographically strong, URL-safe opaque value. Values returned by
// this interface are immediately hashed before persistence and must never be logged.
type SecretGenerator interface {
	NewSecret() (string, error)
}

// TokenIssuer signs protocol access and ID tokens after the service has validated the durable
// authorization state. JWT/JWKS implementation is intentionally outside this module.
type TokenIssuer interface {
	IssueOIDCTokens(context.Context, TokenIssue) (IssuedTokens, error)
}

// Repository defines the durable MySQL operations required by OIDC/OAuth runtime. Commands that
// consume or rotate secrets must execute their compare-and-transition logic in one transaction.
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

// AuthorizationInput is a browser authorization request after HTTP has authenticated the local
// session. State is echoed only for the transport redirect and is intentionally not persisted.
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

// AuthorizationResult contains the opaque one-time code for a registered exact redirect URI.
type AuthorizationResult struct {
	AuthorizationCode string
	RedirectURI       string
	Scope             string
	State             string
	ExpiresAt         time.Time
}

// ClientAuthentication represents token-endpoint client authentication. The HTTP adapter should
// populate ClientSecret only from a transport-safe credential source such as HTTP Basic.
type ClientAuthentication struct {
	ClientID     string
	ClientSecret string
	// ClientAssertion is a signed JWT used only with private_key_jwt. It is never persisted.
	ClientAssertion         string
	ClientAssertionType     string
	ClientAssertionAudience string
}

// AuthorizationCodeExchangeInput represents grant_type=authorization_code.
type AuthorizationCodeExchangeInput struct {
	ClientAuthentication
	Code         string
	RedirectURI  string
	CodeVerifier string
}

// RefreshTokenInput represents grant_type=refresh_token.
type RefreshTokenInput struct {
	ClientAuthentication
	RefreshToken string
}

// RevokeTokenInput represents RFC 7009-style revocation after the HTTP layer has selected a
// supported token hint. Unknown token values are deliberately accepted as a successful no-op.
type RevokeTokenInput struct {
	ClientAuthentication
	Token     string
	TokenType string
	ExpiresAt *time.Time
	Reason    string
}

// TokenIssue is the complete, validated claim material that an OIDC JWT adapter may sign. The
// token identifier is stable before persistence so an adapter can include it as jti.
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

// IssuedTokens is returned by the signing adapter. Refresh tokens are generated and retained by
// this service rather than the signer, so the signer never receives a long-lived bearer secret.
type IssuedTokens struct {
	AccessToken string
	IDToken     string
}

// TokenResult is safe for a token HTTP response. The refresh token is present only when the client
// is registered for refresh_token and has a positive refresh token TTL.
type TokenResult struct {
	AccessToken  string
	TokenType    string
	ExpiresIn    int64
	Scope        string
	IDToken      string
	RefreshToken string
}

// NewRefreshToken describes the durable state added at authorization-code exchange or rotation.
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

// ConsumeAuthorizationCodeCommand binds consumption to the exact client, redirect URI, and
// optional refresh token creation. Implementations must lock by CodeHash before transitioning.
type ConsumeAuthorizationCodeCommand struct {
	CodeHash    [32]byte
	ClientID    string
	RedirectURI string
	Refresh     *NewRefreshToken
}

// RotateRefreshTokenCommand atomically consumes the presented token and creates its successor.
type RotateRefreshTokenCommand struct {
	TokenHash [32]byte
	ClientID  string
	Refresh   NewRefreshToken
}

// RevokeTokenCommand records an access-token digest or revokes the matching refresh-token family.
type RevokeTokenCommand struct {
	RevocationID  string
	TenantID      string
	OAuthClientID string
	TokenHash     [32]byte
	TokenType     string
	ExpiresAt     *time.Time
	Reason        string
}

// UserInfoInput is constructed from an already verified, non-revoked user access token. Claims
// are re-resolved against MySQL so disabled tenants, sessions, accounts and users are rejected.
type UserInfoInput struct {
	TenantID      string
	OAuthClientID string
	SessionID     string
	UserID        string
	Scopes        []string
}

// UserInfoQuery is the repository-safe equivalent of UserInfoInput.
type UserInfoQuery struct {
	TenantID      string
	OAuthClientID string
	SessionID     string
	UserID        string
}

// UserInfo contains only claims permitted by the access token's granted scopes.
type UserInfo struct {
	Subject           string
	Name              string
	PreferredUsername string
	Email             string
}
