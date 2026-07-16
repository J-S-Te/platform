// Package application coordinates identity authentication use cases.
package application

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
)

var (
	// ErrUnauthenticated deliberately covers unknown accounts, invalid passwords and disabled
	// credentials so the login endpoint cannot be used to enumerate valid accounts.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrAccountLocked is returned only when an existing account has an active lock window.
	ErrAccountLocked = errors.New("account locked")
)

// AccountLockedError includes the lock expiry for the documented 423 response.
type AccountLockedError struct {
	LockedUntil time.Time
}

func (error AccountLockedError) Error() string { return ErrAccountLocked.Error() }
func (error AccountLockedError) Unwrap() error { return ErrAccountLocked }

// Repository defines persistence operations required by the password and session use cases.
type Repository interface {
	FindLoginAccount(ctx context.Context, accountName string) (domain.LoginAccount, error)
	RecordFailedPasswordAttempt(ctx context.Context, accountID string, attemptedAt time.Time) error
	CreateSessionForLogin(ctx context.Context, account domain.LoginAccount, session domain.Session) error
	FindPrincipalBySession(ctx context.Context, sessionID string, now time.Time) (domain.Principal, error)
	RefreshSession(ctx context.Context, sessionID string, refreshedAt, expiresAt time.Time) error
	RevokeSession(ctx context.Context, sessionID string, revokedAt time.Time, reason string) error
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
	repository Repository
	passwords  PasswordVerifier
	tokens     TokenManager
	ids        IDGenerator
	clock      Clock
	sessionTTL time.Duration
}

// NewService validates and builds an authentication service.
func NewService(repository Repository, passwords PasswordVerifier, tokens TokenManager, ids IDGenerator, clock Clock, sessionTTL time.Duration) (*Service, error) {
	if repository == nil || passwords == nil || tokens == nil || ids == nil || clock == nil {
		return nil, errors.New("identity authentication dependencies must not be nil")
	}
	if sessionTTL <= 0 {
		return nil, errors.New("identity session TTL must be greater than zero")
	}
	return &Service{repository: repository, passwords: passwords, tokens: tokens, ids: ids, clock: clock, sessionTTL: sessionTTL}, nil
}

// LoginInput contains validated password-login data plus non-sensitive client metadata.
type LoginInput struct {
	Account   string
	Password  string
	IPAddress net.IP
	UserAgent string
}

// SessionResult is the non-sensitive API session representation plus the HttpOnly cookie value.
type SessionResult struct {
	ExpiresAt   time.Time
	RedirectURL string
	Token       string
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
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("find password login account: %w", err)
	}

	now := service.clock.Now().UTC().Truncate(time.Second)
	if account.LockedUntil != nil && account.LockedUntil.After(now) {
		return SessionResult{}, AccountLockedError{LockedUntil: account.LockedUntil.UTC()}
	}
	if !isLoginEligible(account, now) {
		return SessionResult{}, ErrUnauthenticated
	}

	matched, err := service.passwords.Verify(input.Password, account.HashAlgorithm, account.PasswordHash, account.AlgorithmParams)
	if err != nil {
		return SessionResult{}, fmt.Errorf("verify password credential: %w", err)
	}
	if !matched {
		if err := service.repository.RecordFailedPasswordAttempt(ctx, account.AccountID, now); err != nil {
			return SessionResult{}, fmt.Errorf("record failed password attempt: %w", err)
		}
		return SessionResult{}, ErrUnauthenticated
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
		CreatedAt: now, ExpiresAt: expiresAt, IPAddress: normalizeIP(input.IPAddress),
		UserAgent: truncateUserAgent(input.UserAgent),
	}
	if err := service.repository.CreateSessionForLogin(ctx, account, session); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("persist login session: %w", err)
	}

	return SessionResult{ExpiresAt: expiresAt, RedirectURL: "/", Token: token}, nil
}

// Authenticate verifies the signed cookie and cross-checks it against current session, account,
// user and tenant state before a protected handler may trust the principal.
func (service *Service) Authenticate(ctx context.Context, token string) (authctx.Principal, error) {
	now := service.clock.Now().UTC()
	claims, err := service.tokens.Verify(token, now)
	if err != nil {
		return authctx.Principal{}, ErrUnauthenticated
	}
	principal, err := service.repository.FindPrincipalBySession(ctx, claims.SessionID, now)
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

// Logout revokes only the current verified session. The caller must clear the matching cookie.
func (service *Service) Logout(ctx context.Context, principal authctx.Principal) error {
	if principal.SessionID == "" {
		return ErrUnauthenticated
	}
	if err := service.repository.RevokeSession(ctx, principal.SessionID, service.clock.Now().UTC(), "LOGOUT"); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return ErrUnauthenticated
		}
		return fmt.Errorf("revoke current session: %w", err)
	}
	return nil
}

func isLoginEligible(account domain.LoginAccount, now time.Time) bool {
	return account.TenantStatus == "ACTIVE" && account.UserStatus == "ACTIVE" &&
		account.AccountStatus == "ACTIVE" && account.CredentialStatus == "ACTIVE" &&
		(account.CredentialExpiry == nil || account.CredentialExpiry.After(now))
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
