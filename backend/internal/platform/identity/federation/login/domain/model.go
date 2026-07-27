// Package domain defines the transport-neutral objects used by an external OIDC browser login.
package domain

import "time"

const (
	// ClientAuthenticationSecretBasic sends client credentials in the HTTP Basic Authorization header.
	ClientAuthenticationSecretBasic = "client_secret_basic"
	// ClientAuthenticationSecretPost sends client credentials in the token request form body.
	ClientAuthenticationSecretPost = "client_secret_post"
	// ClientAuthenticationNone is for public clients that do not hold a client secret.
	ClientAuthenticationNone = "none"
)

// Provider is the runtime-only configuration needed to execute one external OIDC login. ClientSecret
// is supplied by a secure configuration resolver and must never be persisted in login state or logged.
type Provider struct {
	TenantID                 string
	Code                     string
	Issuer                   string
	ClientID                 string
	ClientSecret             string
	RedirectURI              string
	Scopes                   []string
	ClientAuthenticationMode string
}

// Discovery is the verified subset of an OpenID Provider Configuration Document used during one
// authorization-code flow.
type Discovery struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	JWKSURI               string
}

// State is the server-side portion of a short-lived browser authorization attempt. StateHash is the
// lookup key; the raw state value is never persisted by the provided in-memory implementation.
type State struct {
	StateHash          [32]byte
	BrowserBindingHash [32]byte
	TenantID           string
	ProviderCode       string
	Issuer             string
	ClientID           string
	RedirectURI        string
	Discovery          Discovery
	Nonce              string
	PKCEVerifier       string
	ReturnTo           string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// TokenExchange is the private token-endpoint request assembled after the callback state is consumed.
type TokenExchange struct {
	TokenEndpoint            string
	ClientID                 string
	ClientSecret             string
	ClientAuthenticationMode string
	AuthorizationCode        string
	RedirectURI              string
	PKCEVerifier             string
}

// TokenResponse contains only fields needed to verify the upstream identity. Callers must discard
// AccessToken immediately after ID-token validation and must never write either token to logs.
type TokenResponse struct {
	IDToken     string
	AccessToken string
	TokenType   string
	ExpiresIn   int64
}

// JWK is a public JSON Web Key. Only asymmetric verification key types are accepted.
type JWK struct {
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
	Curve     string `json:"crv"`
	X         string `json:"x"`
	Y         string `json:"y"`
}

// JWKSet is a remote issuer's public signing-key set.
type JWKSet struct {
	Keys []JWK `json:"keys"`
}

// IDTokenClaims contains the claims trusted only after signature and protocol validation.
type IDTokenClaims struct {
	Issuer          string
	Subject         string
	Audience        []string
	Nonce           string
	IssuedAt        time.Time
	ExpiresAt       time.Time
	NotBefore       *time.Time
	AuthorizedParty string
	AccessTokenHash string
}

// LocalAccount is the local binding resolution result. It deliberately has no external-subject field.
type LocalAccount struct {
	TenantID   string
	ProviderID string
	BindingID  string
	UserID     string
	AccountID  string
}

// SessionIssue is the safe internal intent passed to the platform's session issuer. It contains no
// upstream token, assertion, subject, client secret, or authorization code.
type SessionIssue struct {
	TenantID             string
	UserID               string
	AccountID            string
	ProviderCode         string
	ProviderID           string
	BindingID            string
	AuthenticationMethod string
	IPAddress            []byte
	UserAgent            string
}

// BrowserSession is the trusted local authentication outcome returned only to the HTTP adapter.
// A valid value contains the server-issued HttpOnly session cookie and its expiration.
type BrowserSession struct {
	CookieValue string
	ExpiresAt   time.Time
}
