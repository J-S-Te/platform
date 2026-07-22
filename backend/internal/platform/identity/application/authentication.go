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
	mfaapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/mfa/application"
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

// Repository defines persistence operations required by the password and session use cases.
type Repository interface {
	FindLoginAccount(ctx context.Context, accountName string) (domain.LoginAccount, error)
	FindFederatedLoginAccount(ctx context.Context, tenantID, userID, accountID string) (domain.LoginAccount, error)
	RecordSuccessfulPasswordVerification(ctx context.Context, account domain.LoginAccount, now time.Time) error
	CreateSession(ctx context.Context, account domain.LoginAccount, session domain.Session) error
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

// MFAService creates and verifies the short-lived, account-bound credential used between a
// successful password verification and browser-session issuance.
type MFAService interface {
	BeginLoginPreAuthentication(context.Context, mfaapplication.BeginLoginPreAuthenticationInput) (mfaapplication.LoginPreAuthentication, error)
	VerifyLoginPreAuthentication(context.Context, mfaapplication.VerifyLoginPreAuthenticationInput) (mfaapplication.LoginPreAuthenticationVerification, error)
}

// Service implements password login and server-verified cookie session operations.
type Service struct {
	repository    Repository
	passwords     PasswordVerifier
	tokens        TokenManager
	ids           IDGenerator
	clock         Clock
	loginSecurity securityapplication.LoginFailureRecorder
	mfa           MFAService
	sessionTTL    time.Duration
}

// NewService validates and builds an authentication service.
func NewService(repository Repository, passwords PasswordVerifier, tokens TokenManager, ids IDGenerator, clock Clock, loginSecurity securityapplication.LoginFailureRecorder, sessionTTL time.Duration, mfaServices ...MFAService) (*Service, error) {
	if repository == nil || passwords == nil || tokens == nil || ids == nil || clock == nil || loginSecurity == nil {
		return nil, errors.New("identity authentication dependencies must not be nil")
	}
	if sessionTTL <= 0 {
		return nil, errors.New("identity session TTL must be greater than zero")
	}
	if len(mfaServices) > 1 {
		return nil, errors.New("identity authentication accepts at most one MFA service")
	}
	var mfa MFAService
	if len(mfaServices) == 1 {
		mfa = mfaServices[0]
		if mfa == nil {
			return nil, errors.New("identity MFA service must not be nil when supplied")
		}
	}
	return &Service{repository: repository, passwords: passwords, tokens: tokens, ids: ids, clock: clock, loginSecurity: loginSecurity, mfa: mfa, sessionTTL: sessionTTL}, nil
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

	// Identity fields are server-side metadata for lifecycle auditing. They are deliberately not
	// included in the HTTP response body or browser cookie.
	SessionID   string
	TenantID    string
	UserID      string
	UserName    string
	AccountID   string
	AccountName string

	// MFARequired means the password was valid but no browser session or cookie may be issued yet.
	// PreAuthenticationCredential is high entropy, opaque and returned only to the current client.
	MFARequired                 bool
	PreAuthenticationCredential string
	PreAuthenticationExpiresAt  time.Time
	MFAMaxAttempts              uint16
}

// MFALoginResult contains either a completed browser session or a bounded failed MFA result.
type MFALoginResult struct {
	SessionResult
	Verified           bool
	VerificationMethod string
	AttemptsRemaining  uint16
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
		return SessionResult{}, lockedError(account, account.LockedUntil.UTC())
	}
	if !isLoginEligible(account, now) {
		return SessionResult{}, ErrUnauthenticated
	}

	matched, err := service.passwords.Verify(input.Password, account.HashAlgorithm, account.PasswordHash, account.AlgorithmParams)
	if err != nil {
		return SessionResult{}, fmt.Errorf("verify password credential: %w", err)
	}
	if !matched {
		result, err := service.loginSecurity.RecordFailedLogin(ctx, securityapplication.LoginFailureInput{
			TenantID: account.TenantID, AccountID: account.AccountID, AccountName: account.AccountName,
			IPAddress: input.IPAddress, UserAgent: input.UserAgent,
		})
		if err != nil {
			return SessionResult{}, fmt.Errorf("record failed password login: %w", err)
		}
		if result.LockedUntil != nil {
			return SessionResult{}, lockedError(account, result.LockedUntil.UTC())
		}
		return SessionResult{}, loginFailedError(account)
	}

	if err := service.repository.RecordSuccessfulPasswordVerification(ctx, account, now); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("record successful password verification: %w", err)
	}

	if service.mfa != nil {
		preAuthentication, err := service.mfa.BeginLoginPreAuthentication(ctx, mfaapplication.BeginLoginPreAuthenticationInput{TenantID: account.TenantID, AccountID: account.AccountID})
		if err != nil {
			return SessionResult{}, fmt.Errorf("begin MFA login pre-authentication: %w", err)
		}
		if preAuthentication.Required {
			return mfaRequiredSessionResult(account, preAuthentication), nil
		}
	}

	return service.createSession(ctx, account, input.IPAddress, input.UserAgent, now)
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

	if service.mfa != nil {
		preAuthentication, err := service.mfa.BeginLoginPreAuthentication(ctx, mfaapplication.BeginLoginPreAuthenticationInput{TenantID: account.TenantID, AccountID: account.AccountID})
		if err != nil {
			return SessionResult{}, fmt.Errorf("begin federated MFA login pre-authentication: %w", err)
		}
		if preAuthentication.Required {
			return mfaRequiredSessionResult(account, preAuthentication), nil
		}
	}

	return service.createSession(ctx, account, input.IPAddress, input.UserAgent, now)
}

// mfaRequiredSessionResult retains only trusted local identity metadata needed by server-side
// adapters before MFA completion. These fields are never serialized in the public MFA response.
func mfaRequiredSessionResult(account domain.LoginAccount, preAuthentication mfaapplication.LoginPreAuthentication) SessionResult {
	return SessionResult{
		TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName,
		MFARequired: true, PreAuthenticationCredential: preAuthentication.Credential,
		PreAuthenticationExpiresAt: preAuthentication.ExpiresAt, MFAMaxAttempts: preAuthentication.MaxAttempts,
	}
}

// CompleteMFALogin verifies the opaque pre-authentication credential and creates the normal browser
// session only after its bound TOTP or recovery code succeeds.
func (service *Service) CompleteMFALogin(ctx context.Context, credential, code string, ipAddress net.IP, userAgent string) (MFALoginResult, error) {
	if service.mfa == nil {
		return MFALoginResult{}, ErrUnauthenticated
	}
	verification, err := service.mfa.VerifyLoginPreAuthentication(ctx, mfaapplication.VerifyLoginPreAuthenticationInput{Credential: credential, Code: code})
	if err != nil {
		return MFALoginResult{}, err
	}
	if !verification.Verified {
		return MFALoginResult{Verified: false, AttemptsRemaining: verification.AttemptsRemaining}, nil
	}
	account := domain.LoginAccount{
		TenantID: verification.Identity.TenantID, TenantName: verification.Identity.TenantName, TenantCode: verification.Identity.TenantCode, TenantStatus: domain.StatusActive,
		UserID: verification.Identity.UserID, UserName: verification.Identity.UserName, UserStatus: domain.StatusActive,
		AccountID: verification.Identity.AccountID, AccountName: verification.Identity.AccountName, AccountStatus: domain.StatusActive,
		CredentialStatus: domain.StatusActive,
	}
	session, err := service.createSession(ctx, account, ipAddress, userAgent, service.clock.Now().UTC().Truncate(time.Second))
	if err != nil {
		return MFALoginResult{}, err
	}
	return MFALoginResult{SessionResult: session, Verified: true, VerificationMethod: verification.VerificationMethod, AttemptsRemaining: verification.AttemptsRemaining}, nil
}

func (service *Service) createSession(ctx context.Context, account domain.LoginAccount, ipAddress net.IP, userAgent string, now time.Time) (SessionResult, error) {
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
	if err := service.repository.CreateSession(ctx, account, session); err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return SessionResult{}, ErrUnauthenticated
		}
		return SessionResult{}, fmt.Errorf("persist login session: %w", err)
	}

	return SessionResult{
		ExpiresAt: expiresAt, RedirectURL: "/", Token: token,
		SessionID: sessionID, TenantID: account.TenantID, UserID: account.UserID, UserName: account.UserName,
		AccountID: account.AccountID, AccountName: account.AccountName,
	}, nil
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
