// Package application coordinates identity authentication use cases.
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
	securityapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/security/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
)

var (
	// ErrUnauthenticated deliberately covers unknown accounts, invalid passwords and disabled
	// credentials so the login endpoint cannot be used to enumerate valid accounts.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrAccountLocked is returned only when an existing account has an active lock window.
	ErrAccountLocked = errors.New("account locked")
	// ErrConcurrentSession prevents one login account from creating a second active browser
	// session on another terminal.
	ErrConcurrentSession = errors.New("account already has an active session")

	dummyPasswordDigest   = make([]byte, 32)
	dummyPasswordMetadata = mustDummyPasswordMetadata()
)

// AccountLockedError includes the lock expiry for the documented 423 response.
type AccountLockedError struct {
	LockedUntil time.Time
	TenantID    string
	UserID      string
	UserName    string
	AccountID   string
	AccountName string
}

// ConcurrentSessionError retains trusted account identity for security audit logging while the
// public API returns only a stable conflict code and client-safe message.
type ConcurrentSessionError struct {
	TenantID    string
	UserID      string
	UserName    string
	AccountID   string
	AccountName string
}

// LoginFailedError retains trusted account identity for audit logging while preserving the public
// generic invalid-credential response.
type LoginFailedError struct {
	TenantID    string
	UserID      string
	UserName    string
	AccountID   string
	AccountName string
}

func (error LoginFailedError) Error() string { return ErrUnauthenticated.Error() }
func (error LoginFailedError) Unwrap() error { return ErrUnauthenticated }

func (error AccountLockedError) Error() string { return ErrAccountLocked.Error() }
func (error AccountLockedError) Unwrap() error { return ErrAccountLocked }

func (error ConcurrentSessionError) Error() string { return ErrConcurrentSession.Error() }
func (error ConcurrentSessionError) Unwrap() error { return ErrConcurrentSession }

// Repository defines persistence operations required by the password and session use cases.
type Repository interface {
	FindLoginAccount(ctx context.Context, accountName string) (domain.LoginAccount, error)
	FindFederatedLoginAccount(ctx context.Context, tenantID, userID, accountID string) (domain.LoginAccount, error)
	RecordSuccessfulPasswordVerification(ctx context.Context, account domain.LoginAccount, now time.Time) error
	CreateSession(ctx context.Context, account domain.LoginAccount, session domain.Session, idleTimeout time.Duration, replaceExisting bool) error
	FindPrincipalBySession(ctx context.Context, sessionID string, now time.Time, idleTimeout time.Duration) (domain.Principal, error)
	RecordSessionInteraction(ctx context.Context, sessionID string, interactedAt time.Time, idleTimeout time.Duration) error
	RefreshSession(ctx context.Context, sessionID string, refreshedAt, expiresAt time.Time) error
	RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time, reason string) error
	RevokeAccountSessions(ctx context.Context, tenantID, accountID string, revokedAt time.Time, reason string) error
}

// PasswordVerifier verifies the password credential supplied by a login request.
type PasswordVerifier interface {
	Verify(password, algorithm string, digest, metadata []byte) (bool, error)
}

// TokenManager signs and verifies the server-owned JWT browser cookie.
type TokenManager interface {
	Issue(claims security.TokenClaims) (string, error)
	Verify(token string, now time.Time) (security.TokenClaims, error)
}

// IDGenerator allows tests to use deterministic session IDs.
type IDGenerator interface {
	New(now time.Time) (string, error)
}

// Clock exposes the current time so expiry behavior can be tested deterministically.
type Clock interface {
	Now() time.Time
}

// Service implements password login and server-verified cookie session operations.
type Service struct {
	repository    Repository
	passwords     PasswordVerifier
	tokens        TokenManager
	ids           IDGenerator
	clock         Clock
	loginSecurity securityapplication.LoginSecurityService
	sessionTTL    time.Duration
}

// NewService validates and builds an authentication service.
func NewService(repository Repository, passwords PasswordVerifier, tokens TokenManager, ids IDGenerator, clock Clock, loginSecurity securityapplication.LoginSecurityService, sessionTTL time.Duration) (*Service, error) {
	if repository == nil || passwords == nil || tokens == nil || ids == nil || clock == nil || loginSecurity == nil {
		return nil, errors.New("identity authentication dependencies must not be nil")
	}
	if sessionTTL <= 0 {
		return nil, errors.New("identity session TTL must be greater than zero")
	}
	return &Service{repository: repository, passwords: passwords, tokens: tokens, ids: ids, clock: clock, loginSecurity: loginSecurity, sessionTTL: sessionTTL}, nil
}

// LoginInput contains validated password-login data plus non-sensitive client metadata.
type LoginInput struct {
	Account                string
	Password               string
	IPAddress              net.IP
	UserAgent              string
	ReplaceExistingSession bool
}

// SessionResult is the non-sensitive API session representation plus the HttpOnly cookie value.
type SessionResult struct {
	ExpiresAt   time.Time
	RedirectURL string
	Token       string

	// Identity fields are server-side metadata for lifecycle auditing. They are deliberately not
	// included in the HTTP response body or browser cookie.
	SessionID               string
	TenantID                string
	UserID                  string
	UserName                string
	AccountID               string
	AccountName             string
	ReplacedExistingSession bool
}

// Login validates a local account's Argon2id password, persists iam_session and signs its cookie.
func (service *Service) Login(ctx context.Context, input LoginInput) (SessionResult, error) {
	accountName := strings.TrimSpace(input.Account)
	if accountName == "" || input.Password == "" {
		return SessionResult{}, ErrUnauthenticated
	}

	account, err := service.repository.FindLoginAccount(ctx, accountName)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			service.consumeUnknownAccountPassword(input.Password)
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("find password login account: %w", err)
	}

	now := service.clock.Now().UTC().Truncate(time.Second)
	matched, err := service.passwords.Verify(input.Password, account.HashAlgorithm, account.PasswordHash, account.AlgorithmParams)
	if err != nil {
		return SessionResult{}, fmt.Errorf("verify password credential: %w", err)
	}
	if account.LockedUntil != nil && account.LockedUntil.After(now) {
		return SessionResult{}, loginFailedError(account)
	}
	if !isLoginEligible(account, now) {
		return SessionResult{}, loginFailedError(account)
	}
	if !matched {
		_, err := service.loginSecurity.RecordFailedLogin(ctx, securityapplication.LoginFailureInput{
			TenantID: account.TenantID, AccountID: account.AccountID, AccountName: account.AccountName,
			IPAddress: input.IPAddress, UserAgent: input.UserAgent,
		})
		if err != nil {
			return SessionResult{}, fmt.Errorf("record failed password login: %w", err)
		}
		return SessionResult{}, loginFailedError(account)
	}

	if err := service.repository.RecordSuccessfulPasswordVerification(ctx, account, now); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("record successful password verification: %w", err)
	}

	return service.createSession(ctx, account, input.IPAddress, input.UserAgent, now, input.ReplaceExistingSession)
}

func (service *Service) consumeUnknownAccountPassword(password string) {
	// The fixed dummy credential deliberately performs the same Argon2id work as a real account.
	// The result and any verifier error are discarded so unknown accounts always receive the same
	// public authentication failure response.
	_, _ = service.passwords.Verify(password, "argon2id", dummyPasswordDigest, dummyPasswordMetadata)
}

func mustDummyPasswordMetadata() []byte {
	metadata, err := json.Marshal(security.DefaultArgon2idParams([]byte("fixed-dummy-salt")))
	if err != nil {
		panic("marshal dummy password metadata: " + err.Error())
	}
	return metadata
}

// FederatedLoginInput contains a trusted local identity resolved only after the external provider
// callback, ID token and account binding have been verified by the federation login service.
type FederatedLoginInput struct {
	TenantID  string
	UserID    string
	AccountID string
	IPAddress net.IP
	UserAgent string
}

// LoginFederated creates a normal platform login from an active local account that was resolved by
// an external identity binding. Upstream tokens, authorization codes and external subjects never
// enter this service.
func (service *Service) LoginFederated(ctx context.Context, input FederatedLoginInput) (SessionResult, error) {
	tenantID := strings.TrimSpace(input.TenantID)
	userID := strings.TrimSpace(input.UserID)
	accountID := strings.TrimSpace(input.AccountID)
	if tenantID == "" || userID == "" || accountID == "" {
		return SessionResult{}, ErrUnauthenticated
	}

	account, err := service.repository.FindFederatedLoginAccount(ctx, tenantID, userID, accountID)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("find federated login account: %w", err)
	}

	now := service.clock.Now().UTC().Truncate(time.Second)
	if account.LockedUntil != nil && account.LockedUntil.After(now) {
		return SessionResult{}, lockedError(account, account.LockedUntil.UTC())
	}
	if !isIdentityEligible(account) {
		return SessionResult{}, ErrUnauthenticated
	}

	return service.createSession(ctx, account, input.IPAddress, input.UserAgent, now, false)
}

func (service *Service) createSession(ctx context.Context, account domain.LoginAccount, ipAddress net.IP, userAgent string, now time.Time, replaceExisting bool) (SessionResult, error) {
	idleTimeout, err := service.loginSecurity.SessionIdleTimeout(ctx, account.TenantID)
	if err != nil {
		return SessionResult{}, fmt.Errorf("read session inactivity policy: %w", err)
	}
	if idleTimeout <= 0 {
		return SessionResult{}, errors.New("session inactivity policy must be greater than zero")
	}

	sessionID, err := service.ids.New(now)
	if err != nil {
		return SessionResult{}, fmt.Errorf("generate session ID: %w", err)
	}
	expiresAt := now.Add(service.sessionTTL).UTC().Truncate(time.Second)
	token, err := service.tokens.Issue(security.TokenClaims{
		SessionID: sessionID, UserID: account.UserID, TenantID: account.TenantID, AccountID: account.AccountID,
		IssuedAt: now, ExpiresAt: expiresAt,
	})
	if err != nil {
		return SessionResult{}, fmt.Errorf("issue session token: %w", err)
	}

	session := domain.Session{
		ID: sessionID, TenantID: account.TenantID, AccountID: account.AccountID,
		CreatedAt: now, ExpiresAt: expiresAt, IPAddress: normalizeIP(ipAddress),
		UserAgent: truncateUserAgent(userAgent),
	}
	if err := service.repository.CreateSession(ctx, account, session, idleTimeout, replaceExisting); err != nil {
		if errors.Is(err, ErrConcurrentSession) {
			return SessionResult{}, concurrentSessionError(account)
		}
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("persist login session: %w", err)
	}

	return SessionResult{
		ExpiresAt: expiresAt, RedirectURL: "/", Token: token,
		SessionID: sessionID, TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName,
		ReplacedExistingSession: replaceExisting,
	}, nil
}

func concurrentSessionError(account domain.LoginAccount) ConcurrentSessionError {
	return ConcurrentSessionError{TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName}
}

func lockedError(account domain.LoginAccount, lockedUntil time.Time) AccountLockedError {
	return AccountLockedError{LockedUntil: lockedUntil, TenantID: account.TenantID, UserID: account.UserID,
		UserName: account.UserName, AccountID: account.AccountID, AccountName: account.AccountName}
}

func loginFailedError(account domain.LoginAccount) LoginFailedError {
	return LoginFailedError{TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName}
}

// Authenticate verifies the signed cookie and cross-checks it against current session, account,
// user and tenant state before a protected handler may trust the principal.
func (service *Service) Authenticate(ctx context.Context, token string) (authctx.Principal, error) {
	now := service.clock.Now().UTC()
	claims, err := service.tokens.Verify(token, now)
	if err != nil {
		return authctx.Principal{}, ErrUnauthenticated
	}
	idleTimeout, err := service.loginSecurity.SessionIdleTimeout(ctx, claims.TenantID)
	if err != nil {
		return authctx.Principal{}, fmt.Errorf("read session inactivity policy: %w", err)
	}
	principal, err := service.repository.FindPrincipalBySession(ctx, claims.SessionID, now, idleTimeout)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return authctx.Principal{}, ErrUnauthenticated
		}
		return authctx.Principal{}, fmt.Errorf("find active session principal: %w", err)
	}
	if principal.SessionID != claims.SessionID || principal.Tenant.ID != claims.TenantID ||
		principal.User.ID != claims.UserID || principal.Account.ID != claims.AccountID {
		return authctx.Principal{}, ErrUnauthenticated
	}
	return toAuthContextPrincipal(principal), nil
}

// RecordInteraction marks a session as actively used only after a browser reports a trusted
// click, key press, scroll, or touch event. Authentication itself intentionally does not extend
// idle expiry, so background requests cannot prevent automatic logout.
func (service *Service) RecordInteraction(ctx context.Context, principal authctx.Principal) error {
	if principal.SessionID == "" || principal.Tenant.ID == "" || principal.Account.ID == "" {
		return ErrUnauthenticated
	}
	now := service.clock.Now().UTC()
	idleTimeout, err := service.loginSecurity.SessionIdleTimeout(ctx, principal.Tenant.ID)
	if err != nil {
		return fmt.Errorf("read session inactivity policy: %w", err)
	}
	if err := service.repository.RecordSessionInteraction(ctx, principal.SessionID, now, idleTimeout); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			if revokeErr := service.repository.RevokeAccountSessions(ctx, principal.Tenant.ID, principal.Account.ID, now, "IDLE_TIMEOUT"); revokeErr != nil {
				return fmt.Errorf("revoke expired account sessions: %w", revokeErr)
			}
			return ErrUnauthenticated
		}
		return fmt.Errorf("record active browser interaction: %w", err)
	}
	return nil
}

// Refresh renews the current active session and returns a new JWT Cookie value.
func (service *Service) Refresh(ctx context.Context, principal authctx.Principal) (SessionResult, error) {
	if principal.SessionID == "" || principal.Tenant.ID == "" || principal.User.ID == "" || principal.Account.ID == "" {
		return SessionResult{}, ErrUnauthenticated
	}
	now := service.clock.Now().UTC().Truncate(time.Second)
	expiresAt := now.Add(service.sessionTTL).UTC().Truncate(time.Second)
	if err := service.repository.RefreshSession(ctx, principal.SessionID, now, expiresAt); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("refresh active session: %w", err)
	}
	token, err := service.tokens.Issue(security.TokenClaims{
		SessionID: principal.SessionID, UserID: principal.User.ID, TenantID: principal.Tenant.ID,
		AccountID: principal.Account.ID, IssuedAt: now, ExpiresAt: expiresAt,
	})
	if err != nil {
		return SessionResult{}, fmt.Errorf("issue refreshed session token: %w", err)
	}
	return SessionResult{ExpiresAt: expiresAt, RedirectURL: "/", Token: token}, nil
}

// Logout revokes every active session for the current tenant account. This is intentionally
// account-wide so signing out from any child application invalidates the SSO session everywhere.
func (service *Service) Logout(ctx context.Context, principal authctx.Principal) error {
	if principal.SessionID == "" || principal.Tenant.ID == "" || principal.Account.ID == "" {
		return ErrUnauthenticated
	}
	if err := service.repository.RevokeAccountSessions(ctx, principal.Tenant.ID, principal.Account.ID, service.clock.Now().UTC(), "GLOBAL_LOGOUT"); err != nil {
		return fmt.Errorf("revoke account sessions: %w", err)
	}
	return nil
}

// SessionIdleTimeout returns the current tenant inactivity timeout for the browser lifecycle.
func (service *Service) SessionIdleTimeout(ctx context.Context, tenantID string) (time.Duration, error) {
	if strings.TrimSpace(tenantID) == "" {
		return 0, ErrUnauthenticated
	}
	timeout, err := service.loginSecurity.SessionIdleTimeout(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("read session inactivity policy: %w", err)
	}
	return timeout, nil
}

func isLoginEligible(account domain.LoginAccount, now time.Time) bool {
	if !isIdentityEligible(account) || account.CredentialStatus != domain.StatusActive {
		return false
	}
	return account.CredentialExpiry == nil || account.CredentialExpiry.After(now)
}

func isIdentityEligible(account domain.LoginAccount) bool {
	return account.TenantStatus == domain.StatusActive &&
		account.UserStatus == domain.StatusActive &&
		account.AccountStatus == domain.StatusActive
}

func normalizeIP(ip net.IP) []byte {
	if ip == nil {
		return nil
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return append([]byte(nil), ipv4...)
	}
	if ipv6 := ip.To16(); ipv6 != nil {
		return append([]byte(nil), ipv6...)
	}
	return nil
}

func truncateUserAgent(userAgent string) string {
	const maxUserAgentBytes = 1000
	if len(userAgent) <= maxUserAgentBytes {
		return userAgent
	}
	return userAgent[:maxUserAgentBytes]
}

func toAuthContextPrincipal(principal domain.Principal) authctx.Principal {
	roles := make([]authctx.ReferenceName, len(principal.Roles))
	for index, role := range principal.Roles {
		roles[index] = authctx.ReferenceName{ID: role.ID, Name: role.Name, Code: role.Code}
	}
	return authctx.Principal{
		SessionID:       principal.SessionID,
		Tenant:          authctx.ReferenceName{ID: principal.Tenant.ID, Name: principal.Tenant.Name, Code: principal.Tenant.Code},
		User:            authctx.ReferenceName{ID: principal.User.ID, Name: principal.User.Name, Code: principal.User.Code},
		Account:         authctx.ReferenceName{ID: principal.Account.ID, Name: principal.Account.Name, Code: principal.Account.Code},
		Roles:           roles,
		PermissionCodes: append([]string(nil), principal.PermissionCodes...),
	}
}
