// Package domain defines transport-neutral objects for DingTalk QR-code login.
package domain

import (
	"time"

	logindomain "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/login/domain"
)

const (
	// AuthorizationEndpoint is the only DingTalk browser authorization endpoint accepted by this adapter.
	AuthorizationEndpoint = "https://login.dingtalk.com/oauth2/auth"
	// TokenEndpoint is the official DingTalk endpoint for exchanging an authorization code.
	TokenEndpoint = "https://api.dingtalk.com/v1.0/oauth2/userAccessToken"
	// UserInfoEndpoint is the official DingTalk endpoint that returns the verified current user.
	UserInfoEndpoint = "https://api.dingtalk.com/v1.0/contact/users/me"
)

// Provider is the runtime-only DingTalk configuration resolved from the federation control plane.
// AppKey is a compatibility field that carries the ISV application SuiteKey. AppSecret carries
// the SuiteSecret/client secret; it is decrypted only for the outbound token request and must never
// be persisted or logged.
type Provider struct {
	ID          string
	TenantID    string
	Code        string
	AppKey      string
	AppSecret   string
	RedirectURI string
	Scopes      []string
}

// State is the private server-side portion of one short-lived DingTalk QR authorization attempt.
// The raw State and BrowserBinding values are represented only by their SHA-256 hashes.
type State struct {
	StateHash          [32]byte
	BrowserBindingHash [32]byte
	SessionID          string
	TenantID           string
	ProviderCode       string
	ProviderID         string
	AppKey             string
	RedirectURI        string
	ReturnTo           string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// UserProfile contains the only external identifier accepted by the DingTalk login adapter. UnionID
// is runtime-only: it is hashed inside the account resolver and must not be written to logs or MySQL.
type UserProfile struct {
	UnionID string
}

// LocalAccount, SessionIssue and BrowserSession intentionally reuse the existing federated-login
// contracts. This permits bootstrap to inject the same FederatedSessionIssuer for a completed
// session or an MFA pre-authentication challenge without duplicating security decisions.
type LocalAccount = logindomain.LocalAccount
type SessionIssue = logindomain.SessionIssue
type BrowserSession = logindomain.BrowserSession
