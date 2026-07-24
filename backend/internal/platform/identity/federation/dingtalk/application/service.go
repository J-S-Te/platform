// Package application coordinates DingTalk QR-code authorization without coupling to Gin or GORM.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/federation/dingtalk/domain"
)

var (
	// ErrInvalidRequest identifies malformed browser-controlled input.
	ErrInvalidRequest = errors.New("invalid dingtalk QR login request")
	// ErrProviderUnavailable intentionally covers unknown, disabled and unusable providers.
	ErrProviderUnavailable = errors.New("dingtalk QR provider is unavailable")
	// ErrInvalidState intentionally covers unknown, expired, replayed and browser-mismatched state.
	ErrInvalidState = errors.New("dingtalk QR login state is invalid")
	// ErrAuthorizationDenied identifies an explicit denial reported by DingTalk.
	ErrAuthorizationDenied = errors.New("dingtalk authorization was denied")
	// ErrProtocolValidation deliberately collapses token and user-info protocol failures.
	ErrProtocolValidation = errors.New("dingtalk protocol validation failed")
	// ErrAccountNotBound means the verified DingTalk unionId has no active local binding.
	ErrAccountNotBound = errors.New("dingtalk identity is not bound to a local account")
	// ErrSessionIssue means a verified local identity could not create a platform login outcome.
	ErrSessionIssue = errors.New("dingtalk login session could not be created")
)

const (
	defaultStateTTL            = 5 * time.Minute
	maxAuthorizationCodeLength = 4096
)

// Clock makes QR-state expiration deterministic in tests.
type Clock interface{ Now() time.Time }

// SystemClock provides UTC production timestamps.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// SecretGenerator generates high-entropy URL-safe state and session identifiers.
type SecretGenerator interface{ NewSecret() (string, error) }

// ProviderResolver returns active DingTalk runtime configuration for one tenant.
type ProviderResolver interface {
	ResolveProvider(context.Context, string, string) (domain.Provider, error)
}

// AccountResolver hashes and resolves a verified DingTalk unionId to an active local account.
type AccountResolver interface {
	ResolveAccount(context.Context, string, string, string) (domain.LocalAccount, error)
}

// SessionIssuer creates either a completed local browser session or an MFA pre-authentication result.
type SessionIssuer interface {
	IssueBrowserSession(context.Context, domain.SessionIssue) (domain.BrowserSession, error)
}

// StateStore persists and atomically consumes encrypted one-time QR authorization state.
type StateStore interface {
	Save(context.Context, domain.State) error
	Consume(context.Context, [32]byte, [32]byte, time.Time) (domain.State, error)
}

// DingTalkRemote performs the two required DingTalk protocol calls. Access tokens are intentionally
// kept inside the remote implementation and never returned to application callers.
type DingTalkRemote interface {
	ResolveUser(context.Context, domain.Provider, string) (domain.UserProfile, error)
}

// Config bounds QR authorization-state lifetime.
type Config struct {
	StateTTL time.Duration
}

// Service creates DingTalk authorization URLs and validates consumed callback state.
type Service struct {
	providers ProviderResolver
	accounts  AccountResolver
	sessions  SessionIssuer
	states    StateStore
	secrets   SecretGenerator
	remote    DingTalkRemote
	clock     Clock
	config    Config
}

// CreateQRSessionInput is the browser-safe request for a new embedded QR login session.
type CreateQRSessionInput struct {
	TenantID       string
	ProviderCode   string
	ReturnTo       string
	BrowserBinding string
}

// QRSDKConfig contains the browser-safe parameters accepted by DingTalk's official DTFrameLogin SDK.
// The one-time state is returned only to the initiating browser and its digest is persisted server-side.
type QRSDKConfig struct {
	ClientID     string `json:"client_id"`
	RedirectURI  string `json:"redirect_uri"`
	ResponseType string `json:"response_type"`
	Scope        string `json:"scope"`
	State        string `json:"state"`
	Prompt       string `json:"prompt,omitempty"`
}

// CreateQRSessionResult contains only values required to render the official DingTalk QR flow.
type CreateQRSessionResult struct {
	SessionID  string      `json:"session_id"`
	ExpiresAt  time.Time   `json:"expires_at"`
	RenderMode string      `json:"render_mode"`
	SDKConfig  QRSDKConfig `json:"sdk_config"`
}

// CallbackInput contains callback values that may originate from a browser. They are never logged.
type CallbackInput struct {
	State             string
	AuthorizationCode string
	ProviderError     string
	BrowserBinding    string
	IPAddress         []byte
	UserAgent         string
}

// CallbackLifecycle is safe state-derived context for lifecycle audit events. It never exposes a
// DingTalk authorization code, access token, AppSecret or unionId.
type CallbackLifecycle struct {
	TenantID     string
	ProviderCode string
	ProviderID   string
	BindingID    string
	UserID       string
	AccountID    string
}

// CallbackResult is the trusted local authentication outcome consumed only by the HTTP adapter.
type CallbackResult struct {
	Session    domain.BrowserSession
	RedirectTo string
	SessionID  string
	Lifecycle  CallbackLifecycle
}

// NewService validates every adapter because the QR flow cannot safely degrade to in-memory or
// unauthenticated behavior when an integration is miswired.
func NewService(providers ProviderResolver, accounts AccountResolver, sessions SessionIssuer, states StateStore, secrets SecretGenerator, remote DingTalkRemote, clock Clock, config Config) (*Service, error) {
	if providers == nil || accounts == nil || sessions == nil || states == nil || secrets == nil || remote == nil || clock == nil {
		return nil, errors.New("dingtalk QR login dependencies must not be nil")
	}
	if config.StateTTL == 0 {
		config.StateTTL = defaultStateTTL
	}
	if config.StateTTL < time.Minute || config.StateTTL > 15*time.Minute {
		return nil, errors.New("dingtalk QR login state TTL must be between one and fifteen minutes")
	}
	return &Service{providers: providers, accounts: accounts, sessions: sessions, states: states, secrets: secrets, remote: remote, clock: clock, config: config}, nil
}

// CreateQRSession persists a one-time encrypted state record and returns browser-safe parameters
// for DingTalk's official embedded QR SDK. No state, AppSecret or browser binding is persisted in plaintext.
func (service *Service) CreateQRSession(ctx context.Context, input CreateQRSessionInput) (CreateQRSessionResult, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.ProviderCode = strings.TrimSpace(input.ProviderCode)
	input.ReturnTo = strings.TrimSpace(input.ReturnTo)
	input.BrowserBinding = strings.TrimSpace(input.BrowserBinding)
	if input.TenantID == "" || input.ProviderCode == "" || !validReturnTo(input.ReturnTo) || input.BrowserBinding == "" {
		return CreateQRSessionResult{}, ErrInvalidRequest
	}

	provider, err := service.providers.ResolveProvider(ctx, input.TenantID, input.ProviderCode)
	if err != nil || !validProvider(provider, input.TenantID, input.ProviderCode) {
		return CreateQRSessionResult{}, ErrProviderUnavailable
	}
	stateValue, err := service.secrets.NewSecret()
	if err != nil || strings.TrimSpace(stateValue) == "" {
		return CreateQRSessionResult{}, ErrProviderUnavailable
	}
	sessionID, err := service.secrets.NewSecret()
	if err != nil || strings.TrimSpace(sessionID) == "" {
		return CreateQRSessionResult{}, ErrProviderUnavailable
	}

	now := service.clock.Now().UTC()
	stateHash := sha256.Sum256([]byte(stateValue))
	bindingHash := sha256.Sum256([]byte(input.BrowserBinding))
	state := domain.State{
		StateHash: stateHash, BrowserBindingHash: bindingHash, SessionID: sessionID,
		TenantID: input.TenantID, ProviderCode: input.ProviderCode, ProviderID: provider.ID,
		AppKey: provider.AppKey, RedirectURI: provider.RedirectURI, ReturnTo: input.ReturnTo,
		CreatedAt: now, ExpiresAt: now.Add(service.config.StateTTL),
	}
	if err := service.states.Save(ctx, state); err != nil {
		return CreateQRSessionResult{}, ErrProviderUnavailable
	}

	sdkConfig, err := qrSDKConfig(provider, stateValue)
	if err != nil {
		return CreateQRSessionResult{}, ErrProviderUnavailable
	}
	return CreateQRSessionResult{
		SessionID: sessionID, ExpiresAt: state.ExpiresAt, RenderMode: "dingtalk_frame", SDKConfig: sdkConfig,
	}, nil
}

// CompleteCallback consumes state before contacting DingTalk, resolves only a pre-bound active
// account, and delegates the completed-session versus MFA decision to the existing session issuer.
func (service *Service) CompleteCallback(ctx context.Context, input CallbackInput) (CallbackResult, error) {
	input.State = strings.TrimSpace(input.State)
	input.AuthorizationCode = strings.TrimSpace(input.AuthorizationCode)
	input.ProviderError = strings.TrimSpace(input.ProviderError)
	input.BrowserBinding = strings.TrimSpace(input.BrowserBinding)
	if input.State == "" || input.BrowserBinding == "" {
		return CallbackResult{}, ErrInvalidState
	}

	stateHash := sha256.Sum256([]byte(input.State))
	bindingHash := sha256.Sum256([]byte(input.BrowserBinding))
	state, err := service.states.Consume(ctx, stateHash, bindingHash, service.clock.Now().UTC())
	if err != nil || !validState(state) {
		return CallbackResult{}, ErrInvalidState
	}
	lifecycle := CallbackLifecycle{TenantID: state.TenantID, ProviderCode: state.ProviderCode, ProviderID: state.ProviderID}
	if input.ProviderError != "" {
		return CallbackResult{SessionID: state.SessionID, Lifecycle: lifecycle}, ErrAuthorizationDenied
	}
	if len(input.AuthorizationCode) == 0 || len(input.AuthorizationCode) > maxAuthorizationCodeLength {
		return CallbackResult{SessionID: state.SessionID, Lifecycle: lifecycle}, ErrInvalidRequest
	}

	provider, err := service.providers.ResolveProvider(ctx, state.TenantID, state.ProviderCode)
	if err != nil || !validProvider(provider, state.TenantID, state.ProviderCode) || !matchesStateProvider(provider, state) {
		return CallbackResult{SessionID: state.SessionID, Lifecycle: lifecycle}, ErrProviderUnavailable
	}
	profile, err := service.remote.ResolveUser(ctx, provider, input.AuthorizationCode)
	if err != nil || strings.TrimSpace(profile.UnionID) == "" {
		return CallbackResult{SessionID: state.SessionID, Lifecycle: lifecycle}, ErrProtocolValidation
	}
	account, err := service.accounts.ResolveAccount(ctx, state.TenantID, state.ProviderCode, profile.UnionID)
	if err != nil || !validAccount(account, state.TenantID, provider.ID) {
		return CallbackResult{SessionID: state.SessionID, Lifecycle: lifecycle}, ErrAccountNotBound
	}
	lifecycle.BindingID, lifecycle.UserID, lifecycle.AccountID = account.BindingID, account.UserID, account.AccountID
	session, err := service.sessions.IssueBrowserSession(ctx, domain.SessionIssue{
		TenantID: account.TenantID, UserID: account.UserID, AccountID: account.AccountID,
		ProviderCode: state.ProviderCode, ProviderID: account.ProviderID, BindingID: account.BindingID,
		AuthenticationMethod: "DINGTALK_QR", IPAddress: append(net.IP(nil), input.IPAddress...), UserAgent: input.UserAgent,
	})
	if err != nil || !validBrowserSession(session, service.clock.Now().UTC()) {
		return CallbackResult{SessionID: state.SessionID, Lifecycle: lifecycle}, ErrSessionIssue
	}
	return CallbackResult{Session: session, RedirectTo: state.ReturnTo, SessionID: state.SessionID, Lifecycle: lifecycle}, nil
}

func qrSDKConfig(provider domain.Provider, state string) (QRSDKConfig, error) {
	endpoint, err := url.Parse(domain.AuthorizationEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host != "login.dingtalk.com" || endpoint.Path != "/oauth2/auth" {
		return QRSDKConfig{}, ErrProviderUnavailable
	}
	scopes := append([]string(nil), provider.Scopes...)
	if !containsScope(scopes, "openid") {
		scopes = append(scopes, "openid")
	}
	return QRSDKConfig{
		ClientID: provider.AppKey, RedirectURI: provider.RedirectURI, ResponseType: "code",
		Scope: strings.Join(scopes, " "), State: state, Prompt: "consent",
	}, nil
}

func validProvider(provider domain.Provider, tenantID, providerCode string) bool {
	redirectURI, err := url.Parse(strings.TrimSpace(provider.RedirectURI))
	if err != nil || redirectURI.Scheme != "https" || redirectURI.Host == "" || redirectURI.User != nil {
		return false
	}
	return provider.TenantID == tenantID && provider.Code == providerCode && strings.TrimSpace(provider.ID) != "" &&
		strings.TrimSpace(provider.AppKey) != "" && strings.TrimSpace(provider.AppSecret) != "" && len(provider.Scopes) > 0
}

func validState(state domain.State) bool {
	return strings.TrimSpace(state.SessionID) != "" && strings.TrimSpace(state.TenantID) != "" &&
		strings.TrimSpace(state.ProviderCode) != "" && strings.TrimSpace(state.ProviderID) != "" &&
		strings.TrimSpace(state.AppKey) != "" && validReturnTo(state.ReturnTo) && !state.ExpiresAt.IsZero()
}

func matchesStateProvider(provider domain.Provider, state domain.State) bool {
	return provider.ID == state.ProviderID && provider.AppKey == state.AppKey && provider.RedirectURI == state.RedirectURI
}

func validAccount(account domain.LocalAccount, tenantID, providerID string) bool {
	return account.TenantID == tenantID && account.ProviderID == providerID && strings.TrimSpace(account.BindingID) != "" &&
		strings.TrimSpace(account.UserID) != "" && strings.TrimSpace(account.AccountID) != ""
}

func validBrowserSession(session domain.BrowserSession, now time.Time) bool {
	if session.MFARequired {
		return strings.TrimSpace(session.PreAuthenticationCredential) != "" && session.PreAuthenticationExpiresAt.After(now) && session.MFAMaxAttempts > 0
	}
	return strings.TrimSpace(session.CookieValue) != "" && session.ExpiresAt.After(now)
}

func validReturnTo(value string) bool {
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") ||
		strings.Contains(value, `\`) || containsControlCharacter(value) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.IsAbs() || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" {
		return false
	}
	decodedPath, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || !strings.HasPrefix(decodedPath, "/") || strings.HasPrefix(decodedPath, "//") ||
		strings.Contains(decodedPath, `\`) || containsControlCharacter(decodedPath) {
		return false
	}
	decodedValue, err := url.QueryUnescape(value)
	return err == nil && !strings.Contains(decodedValue, `\`) && !containsControlCharacter(decodedValue)
}

func containsControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func containsScope(scopes []string, wanted string) bool {
	for _, scope := range scopes {
		if strings.EqualFold(strings.TrimSpace(scope), wanted) {
			return true
		}
	}
	return false
}
