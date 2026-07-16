package application

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

func TestLoginCreatesSessionAndSignsCookieToken(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{loginAccount: activeLoginAccount()}
	tokens := &fakeTokenManager{}
	service := newTestService(t, repository, fakePasswordVerifier{matched: true}, tokens, now)

	result, err := service.Login(context.Background(), LoginInput{
		Account: "admin", Password: "correct-password", IPAddress: net.ParseIP("192.0.2.15"),
		UserAgent: strings.Repeat("a", 1200),
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if result.RedirectURL != "/" {
		t.Fatalf("redirect URL = %q, want /", result.RedirectURL)
	}
	if result.ExpiresAt != now.Add(8*time.Hour) {
		t.Fatalf("expires at = %s, want %s", result.ExpiresAt, now.Add(8*time.Hour))
	}
	if repository.createdSession.ID != "01J00000000000000000000099" {
		t.Fatalf("session ID = %q", repository.createdSession.ID)
	}
	if got := len(repository.createdSession.IPAddress); got != net.IPv4len {
		t.Fatalf("stored IPv4 length = %d, want %d", got, net.IPv4len)
	}
	if got := len(repository.createdSession.UserAgent); got != 1000 {
		t.Fatalf("stored user agent length = %d, want 1000", got)
	}
	if tokens.issued.SessionID != repository.createdSession.ID || tokens.issued.AccountID != "account-1" {
		t.Fatalf("issued claims = %#v", tokens.issued)
	}
	if result.Token == "" {
		t.Fatal("login did not return an HttpOnly cookie token value")
	}
}

func TestLoginRejectsWrongPasswordWithoutCreatingSession(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{loginAccount: activeLoginAccount()}
	service := newTestService(t, repository, fakePasswordVerifier{matched: false}, &fakeTokenManager{}, now)

	_, err := service.Login(context.Background(), LoginInput{Account: "admin", Password: "incorrect"})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("login error = %v, want ErrUnauthenticated", err)
	}
	if repository.failedAccountID != "account-1" {
		t.Fatalf("failed credential account = %q, want account-1", repository.failedAccountID)
	}
	if repository.createdSession.ID != "" {
		t.Fatal("wrong password created a session")
	}
}

func TestLoginReturnsLockedAccountErrorBeforePasswordVerification(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	lockedUntil := now.Add(15 * time.Minute)
	account := activeLoginAccount()
	account.LockedUntil = &lockedUntil
	repository := &fakeRepository{loginAccount: account}
	verifier := fakePasswordVerifier{matched: true}
	service := newTestService(t, repository, verifier, &fakeTokenManager{}, now)

	_, err := service.Login(context.Background(), LoginInput{Account: "admin", Password: "password"})
	var locked AccountLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("login error = %v, want AccountLockedError", err)
	}
	if !locked.LockedUntil.Equal(lockedUntil) {
		t.Fatalf("locked until = %s, want %s", locked.LockedUntil, lockedUntil)
	}
}

func TestAuthenticateRejectsTokenWhoseClaimsDoNotMatchPersistedSession(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	repository := &fakeRepository{principal: domain.Principal{
		SessionID: "session-1",
		Tenant:    domain.ReferenceName{ID: "tenant-1", Name: "Tenant"},
		User:      domain.ReferenceName{ID: "user-1", Name: "User"},
		Account:   domain.ReferenceName{ID: "account-1", Name: "admin"},
	}}
	tokens := &fakeTokenManager{verified: security.TokenClaims{
		SessionID: "session-1", TenantID: "tenant-1", UserID: "other-user", AccountID: "account-1",
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	service := newTestService(t, repository, fakePasswordVerifier{matched: true}, tokens, now)

	_, err := service.Authenticate(context.Background(), "token")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("authenticate error = %v, want ErrUnauthenticated", err)
	}
}

func newTestService(t *testing.T, repository *fakeRepository, verifier fakePasswordVerifier, tokens *fakeTokenManager, now time.Time) *Service {
	t.Helper()
	service, err := NewService(repository, verifier, tokens, fakeIDGenerator{}, fixedClock{now: now}, 8*time.Hour)
	if err != nil {
		t.Fatalf("new authentication service: %v", err)
	}
	return service
}

func activeLoginAccount() domain.LoginAccount {
	return domain.LoginAccount{
		TenantID: "tenant-1", TenantName: "Tenant", TenantCode: "default", TenantStatus: "ACTIVE",
		UserID: "user-1", UserName: "Administrator", UserStatus: "ACTIVE",
		AccountID: "account-1", AccountName: "admin", AccountStatus: "ACTIVE",
		PasswordHash: []byte("digest"), HashAlgorithm: "argon2id", AlgorithmParams: []byte(`{}`), CredentialStatus: "ACTIVE",
	}
}

type fakeRepository struct {
	loginAccount     domain.LoginAccount
	findLoginError   error
	failedAccountID  string
	createdSession   domain.Session
	principal        domain.Principal
	createSessionErr error
}

func (repository *fakeRepository) FindLoginAccount(_ context.Context, _ string) (domain.LoginAccount, error) {
	return repository.loginAccount, repository.findLoginError
}

func (repository *fakeRepository) RecordFailedPasswordAttempt(_ context.Context, accountID string, _ time.Time) error {
	repository.failedAccountID = accountID
	return nil
}

func (repository *fakeRepository) CreateSessionForLogin(_ context.Context, _ domain.LoginAccount, session domain.Session) error {
	repository.createdSession = session
	return repository.createSessionErr
}

func (repository *fakeRepository) FindPrincipalBySession(_ context.Context, _ string, _ time.Time) (domain.Principal, error) {
	return repository.principal, nil
}

func (repository *fakeRepository) RefreshSession(context.Context, string, time.Time, time.Time) error {
	return nil
}
func (repository *fakeRepository) RevokeSession(context.Context, string, time.Time, string) error {
	return nil
}

type fakePasswordVerifier struct {
	matched bool
}

func (verifier fakePasswordVerifier) Verify(string, string, []byte, []byte) (bool, error) {
	return verifier.matched, nil
}

type fakeTokenManager struct {
	issued   security.TokenClaims
	verified security.TokenClaims
}

func (manager *fakeTokenManager) Issue(claims security.TokenClaims) (string, error) {
	manager.issued = claims
	return "signed-token", nil
}

func (manager *fakeTokenManager) Verify(string, time.Time) (security.TokenClaims, error) {
	return manager.verified, nil
}

type fakeIDGenerator struct{}

func (fakeIDGenerator) New(time.Time) (string, error) { return "01J00000000000000000000099", nil }

type fixedClock struct {
	now time.Time
}

func (clock fixedClock) Now() time.Time { return clock.now }
