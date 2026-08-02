// Package domain defines protocol-neutral OIDC/OAuth runtime state.
package domain

import "time"

const (
	// ClientStatusActive is the only client status eligible for protocol processing.
	ClientStatusActive = "ACTIVE"
	// SessionStatusActive is the only local session status eligible for authorization.
	SessionStatusActive = "ACTIVE"
	// AuthorizationCodeStatusActive marks an authorization code that has not been exchanged.
	AuthorizationCodeStatusActive = "ACTIVE"
	// AuthorizationCodeStatusConsumed marks a code permanently used by the token endpoint.
	AuthorizationCodeStatusConsumed = "CONSUMED"
	// AuthorizationCodeStatusRevoked marks an unconsumed code invalidated with its browser session.
	AuthorizationCodeStatusRevoked = "REVOKED"
	// TokenFamilyStatusActive marks a refresh-token family that can still rotate.
	TokenFamilyStatusActive = "ACTIVE"
	// TokenFamilyStatusRevoked marks a family invalid after logout, revocation, or replay.
	TokenFamilyStatusRevoked = "REVOKED"
	// RefreshTokenStatusActive marks the single refresh token that can be exchanged next.
	RefreshTokenStatusActive = "ACTIVE"
	// RefreshTokenStatusConsumed marks a refresh token replaced during rotation.
	RefreshTokenStatusConsumed = "CONSUMED"
	// RefreshTokenStatusRevoked marks a refresh token invalidated with its family.
	RefreshTokenStatusRevoked = "REVOKED"
	// TokenTypeAccess identifies a bearer access-token revocation record.
	TokenTypeAccess = "access_token"
	// TokenTypeRefresh identifies a refresh-token revocation record.
	TokenTypeRefresh = "refresh_token"
)

// Client is the active client registration used by protocol runtime. It contains no raw secret
// material; ClientCredentials retains only the one-way verifier hashes read from storage.
type Client struct {
	ID                     string
	TenantID               string
	ClientID               string
	ClientType             string
	TokenAuthMethod        string
	AccessTokenTTLSeconds  uint
	RefreshTokenTTLSeconds uint
	RequirePKCE            bool
	GrantTypes             map[string]struct{}
	Scopes                 map[string]struct{}
	RedirectURIs           map[string]struct{}
	Credentials            []ClientCredential
	JWKs                   []ClientJWK
}

// ClientCredential is a currently usable client-secret verifier. The plaintext secret is never
// copied into domain state, storage, logs, or returned values.
type ClientCredential struct {
	SecretHash []byte
	ValidUntil *time.Time
}

// SessionSubject is the local authenticated browser session to which an authorization code is
// bound. The repository only returns it when tenant, session, account and user remain active.
type SessionSubject struct {
	TenantID  string
	SessionID string
	AccountID string
	UserID    string
	ExpiresAt time.Time
}

// AuthorizationCode is the persistent, one-time protocol credential. CodeHash is always a
// SHA-256 digest of a cryptographically random value and must never contain the raw code.
type AuthorizationCode struct {
	ID                  string
	TenantID            string
	OAuthClientID       string
	SessionID           string
	AccountID           string
	UserID              string
	CodeHash            [32]byte
	RedirectURI         string
	Scopes              []string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
	Status              string
}

// RefreshToken is a persistent rotation node. A family has at most one ACTIVE token; reuse of a
// consumed node revokes every remaining token in that family.
type RefreshToken struct {
	ID                   string
	TenantID             string
	OAuthClientID        string
	SessionID            string
	AccountID            string
	UserID               string
	Scopes               []string
	AuthorizedAt         time.Time
	TokenFamilyID        string
	ParentRefreshTokenID string
	TokenHash            [32]byte
	IssuedAt             time.Time
	ExpiresAt            time.Time
	UsedAt               *time.Time
	RevokedAt            *time.Time
	RevokeReason         string
	Status               string
}

// TokenGrant is a validated subject/client binding from which an access token, optional ID token,
// and a rotated refresh token may safely be issued.
type TokenGrant struct {
	TenantID      string
	OAuthClientID string
	ClientID      string
	SessionID     string
	AccountID     string
	UserID        string
	Scopes        []string
	Nonce         string
	AuthorizedAt  time.Time
}

// UserInfoSubject is a current tenant-scoped user projection. It deliberately contains only
// claims that UserInfo may selectively expose according to granted scopes.
type UserInfoSubject struct {
	TenantID          string
	OAuthClientID     string
	SessionID         string
	UserID            string
	DisplayName       string
	PreferredUsername string
	Email             string
}

// ClientJWK is an active public signing key registered for private_key_jwt or signed request objects.
// The JWK is public-only; private JWK members are rejected by the application service.
type ClientJWK struct {
	PublicJWK  []byte
	KeyID      string
	ValidUntil *time.Time
}
