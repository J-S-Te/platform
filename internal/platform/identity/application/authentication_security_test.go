package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	securityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/security/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	sharedsecurity "github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

type authenticationRepositoryStub struct {
	account domain.LoginAccount
	err     error
}

func (stub authenticationRepositoryStub) FindLoginAccount(context.Context, string) (domain.LoginAccount, error) {
	return stub.account, stub.err
}
func (stub authenticationRepositoryStub) FindLoginAccountByIdentityID(context.Context, string) (domain.LoginAccount, error) {
	return stub.account, stub.err
}
func (authenticationRepositoryStub) RecordSuccessfulPasswordVerification(context.Context, domain.LoginAccount, time.Time) error {
	return nil
}
func (authenticationRepositoryStub) CreateSession(context.Context, domain.LoginAccount, domain.Session, time.Duration, bool) error {
	return nil
}
func (authenticationRepositoryStub) FindPrincipalBySession(context.Context, string, time.Time, time.Duration) (domain.Principal, error) {
	return domain.Principal{}, ErrUnauthenticated
}
func (authenticationRepositoryStub) RecordSessionInteraction(context.Context, string, time.Time, time.Duration) error {
	return nil
}
func (authenticationRepositoryStub) RefreshSession(context.Context, string, time.Time, time.Time) error {
	return nil
}
func (authenticationRepositoryStub) RevokeSession(context.Context, string, time.Time, string) error {
	return nil
}
func (authenticationRepositoryStub) RevokeAccountSessions(context.Context, string, string, time.Time, string) error {
	return nil
}

type passwordVerifierSpy struct {
	calls     int
	algorithm string
	digest    []byte
	metadata  []byte
	matched   bool
}

func (spy *passwordVerifierSpy) Verify(_ string, algorithm string, digest, metadata []byte) (bool, error) {
	spy.calls++
	spy.algorithm = algorithm
	spy.digest = append([]byte(nil), digest...)
	spy.metadata = append([]byte(nil), metadata...)
	return spy.matched, nil
}

type authenticationClockStub struct{ now time.Time }

func (stub authenticationClockStub) Now() time.Time { return stub.now }

type createSessionRepositorySpy struct {
	authenticationRepositoryStub
	createErr       error
	idleTimeout     time.Duration
	replaceExisting bool
}

func (spy *createSessionRepositorySpy) CreateSession(_ context.Context, _ domain.LoginAccount, _ domain.Session, idleTimeout time.Duration, replaceExisting bool) error {
	spy.idleTimeout = idleTimeout
	spy.replaceExisting = replaceExisting
	return spy.createErr
}

type authenticationTokenManagerStub struct{}

func (authenticationTokenManagerStub) Issue(sharedsecurity.TokenClaims) (string, error) {
	return "signed-session-token", nil
}

func (authenticationTokenManagerStub) Verify(string, time.Time) (sharedsecurity.TokenClaims, error) {
	return sharedsecurity.TokenClaims{}, nil
}

type authenticationIDGeneratorStub struct{}

func (authenticationIDGeneratorStub) New(time.Time) (string, error) { return "session-1", nil }

type loginSecurityStub struct{ idleTimeout time.Duration }

func (loginSecurityStub) RecordFailedLogin(context.Context, securityapplication.LoginFailureInput) (securityapplication.LoginFailureResult, error) {
	return securityapplication.LoginFailureResult{}, nil
}

func (stub loginSecurityStub) SessionIdleTimeout(context.Context, string) (time.Duration, error) {
	return stub.idleTimeout, nil
}

type logoutRepositorySpy struct {
	authenticationRepositoryStub
	calls *[]string
}

func (spy logoutRepositorySpy) RevokeAccountSessions(context.Context, string, string, time.Time, string) error {
	*spy.calls = append(*spy.calls, "platform")
	return nil
}

type externalSessionTerminatorSpy struct {
	calls      *[]string
	identityID string
	err        error
}

func (spy *externalSessionTerminatorSpy) LogoutIdentitySessions(_ context.Context, identityID string) error {
	*spy.calls = append(*spy.calls, "keycloak")
	spy.identityID = identityID
	return spy.err
}

func TestLogoutRevokesKeycloakBeforePlatformSessions(t *testing.T) {
	calls := []string{}
	external := &externalSessionTerminatorSpy{calls: &calls}
	service := &Service{
		repository:       logoutRepositorySpy{calls: &calls},
		clock:            authenticationClockStub{now: time.Date(2026, time.August, 13, 10, 0, 0, 0, time.UTC)},
		externalSessions: external,
	}
	principal := authctx.Principal{
		SessionID: "session-1",
		Tenant:    authctx.ReferenceName{ID: "tenant-1"},
		User:      authctx.ReferenceName{ID: "identity-1"},
		Account:   authctx.ReferenceName{ID: "account-1"},
	}

	if err := service.Logout(context.Background(), principal); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if got, want := calls, []string{"keycloak", "platform"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("logout calls = %v, want %v", got, want)
	}
	if external.identityID != "identity-1" {
		t.Fatalf("Keycloak identity ID = %q, want identity-1", external.identityID)
	}
}

func TestLogoutFailsClosedWhenKeycloakRevocationFails(t *testing.T) {
	calls := []string{}
	external := &externalSessionTerminatorSpy{calls: &calls, err: errors.New("keycloak unavailable")}
	service := &Service{
		repository:       logoutRepositorySpy{calls: &calls},
		clock:            authenticationClockStub{now: time.Now()},
		externalSessions: external,
	}
	principal := authctx.Principal{
		SessionID: "session-1",
		Tenant:    authctx.ReferenceName{ID: "tenant-1"},
		User:      authctx.ReferenceName{ID: "identity-1"},
		Account:   authctx.ReferenceName{ID: "account-1"},
	}

	err := service.Logout(context.Background(), principal)
	if err == nil || !strings.Contains(err.Error(), "revoke external identity sessions") {
		t.Fatalf("Logout() error = %v, want external session revocation error", err)
	}
	if got, want := calls, []string{"keycloak"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("logout calls = %v, want %v", got, want)
	}
}

func TestUnknownAccountConsumesPasswordVerificationWork(t *testing.T) {
	verifier := &passwordVerifierSpy{}
	service := &Service{
		repository: authenticationRepositoryStub{err: ErrUnauthenticated},
		passwords:  verifier,
	}

	_, err := service.Login(context.Background(), LoginInput{Account: "missing-account", Password: "secret"})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Login() error = %v, want ErrUnauthenticated", err)
	}
	if verifier.calls != 1 || verifier.algorithm != "argon2id" || len(verifier.digest) != 32 || len(verifier.metadata) == 0 {
		t.Fatalf("dummy verification = calls:%d algorithm:%q digest:%d metadata:%d", verifier.calls, verifier.algorithm, len(verifier.digest), len(verifier.metadata))
	}
}

func TestLockedPasswordAccountReturnsGenericFailureAfterVerification(t *testing.T) {
	now := time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC)
	lockedUntil := now.Add(time.Hour)
	verifier := &passwordVerifierSpy{matched: true}
	service := &Service{
		repository: authenticationRepositoryStub{account: domain.LoginAccount{
			TenantID: "tenant", UserID: "user", AccountID: "account", AccountName: "alice",
			LockedUntil: &lockedUntil, HashAlgorithm: "argon2id", PasswordHash: make([]byte, 32), AlgorithmParams: []byte(`{}`),
		}},
		passwords: verifier,
		clock:     authenticationClockStub{now: now},
	}

	_, err := service.Login(context.Background(), LoginInput{Account: "alice", Password: "secret"})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Login() error = %v, want generic ErrUnauthenticated", err)
	}
	var locked AccountLockedError
	if errors.As(err, &locked) {
		t.Fatal("Login() exposed AccountLockedError")
	}
	if verifier.calls != 1 {
		t.Fatalf("password verification calls = %d, want 1", verifier.calls)
	}
}

func TestCreateSessionRejectsConcurrentTerminalLogin(t *testing.T) {
	now := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)
	repository := &createSessionRepositorySpy{createErr: ErrConcurrentSession}
	service := &Service{
		repository: repository,
		tokens:     authenticationTokenManagerStub{}, ids: authenticationIDGeneratorStub{},
		loginSecurity: loginSecurityStub{idleTimeout: 30 * time.Minute}, sessionTTL: 12 * time.Hour,
	}
	account := domain.LoginAccount{
		TenantID: "tenant-1", UserID: "user-1", UserName: "张三",
		AccountID: "account-1", AccountName: "zhangsan",
	}

	_, err := service.createSession(context.Background(), account, nil, "test-terminal", now, false)
	if !errors.Is(err, ErrConcurrentSession) {
		t.Fatalf("createSession() error = %v, want ErrConcurrentSession", err)
	}
	var concurrent ConcurrentSessionError
	if !errors.As(err, &concurrent) {
		t.Fatalf("createSession() error type = %T, want ConcurrentSessionError", err)
	}
	if concurrent.TenantID != account.TenantID || concurrent.UserID != account.UserID ||
		concurrent.AccountID != account.AccountID || concurrent.AccountName != account.AccountName {
		t.Fatalf("ConcurrentSessionError = %#v, want trusted account identity", concurrent)
	}
	if repository.idleTimeout != 30*time.Minute {
		t.Fatalf("CreateSession() idle timeout = %s, want 30m", repository.idleTimeout)
	}
}

func TestLoginOIDCUsesStableIdentityWithoutPasswordVerification(t *testing.T) {
	now := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)
	verifier := &passwordVerifierSpy{}
	account := domain.LoginAccount{TenantID: "tenant-1", TenantStatus: domain.StatusActive, UserID: "identity-1", UserStatus: domain.StatusActive, AccountID: "account-1", AccountName: "alice", AccountStatus: domain.StatusActive}
	service := &Service{
		repository: authenticationRepositoryStub{account: account}, passwords: verifier,
		tokens: authenticationTokenManagerStub{}, ids: authenticationIDGeneratorStub{}, clock: authenticationClockStub{now: now},
		loginSecurity: loginSecurityStub{idleTimeout: 30 * time.Minute}, sessionTTL: 12 * time.Hour,
	}
	result, err := service.LoginOIDC(context.Background(), OIDCLoginInput{IdentityID: "identity-1"})
	if err != nil {
		t.Fatalf("LoginOIDC() error = %v", err)
	}
	if result.UserID != "identity-1" || verifier.calls != 0 {
		t.Fatalf("result=%#v password verification calls=%d", result, verifier.calls)
	}
}

func TestCreateSessionForwardsReplacementChoice(t *testing.T) {
	now := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)
	repository := &createSessionRepositorySpy{}
	service := &Service{
		repository: repository,
		tokens:     authenticationTokenManagerStub{}, ids: authenticationIDGeneratorStub{},
		loginSecurity: loginSecurityStub{idleTimeout: 30 * time.Minute}, sessionTTL: 12 * time.Hour,
	}
	account := domain.LoginAccount{
		TenantID: "tenant-1", UserID: "user-1", UserName: "张三",
		AccountID: "account-1", AccountName: "zhangsan",
	}

	result, err := service.createSession(context.Background(), account, nil, "replacement-terminal", now, true)
	if err != nil {
		t.Fatalf("createSession() error = %v", err)
	}
	if !repository.replaceExisting {
		t.Fatal("CreateSession() did not receive replaceExisting=true")
	}
	if !result.ReplacedExistingSession {
		t.Fatal("SessionResult.ReplacedExistingSession = false, want true")
	}
}
