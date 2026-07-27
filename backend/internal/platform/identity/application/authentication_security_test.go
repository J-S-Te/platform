package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
)

type authenticationRepositoryStub struct {
	account domain.LoginAccount
	err     error
}

func (stub authenticationRepositoryStub) FindLoginAccount(context.Context, string) (domain.LoginAccount, error) {
	return stub.account, stub.err
}
func (authenticationRepositoryStub) FindFederatedLoginAccount(context.Context, string, string, string) (domain.LoginAccount, error) {
	return domain.LoginAccount{}, ErrUnauthenticated
}
func (authenticationRepositoryStub) RecordSuccessfulPasswordVerification(context.Context, domain.LoginAccount, time.Time) error {
	return nil
}
func (authenticationRepositoryStub) CreateSession(context.Context, domain.LoginAccount, domain.Session) error {
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
