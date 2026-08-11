package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/security/domain"
)

type sessionTimeoutRepositoryStub struct {
	policy domain.LoginPolicy
	err    error
}

func (stub *sessionTimeoutRepositoryStub) GetLoginPolicy(context.Context, string) (domain.LoginPolicy, error) {
	return stub.policy, stub.err
}

func (stub *sessionTimeoutRepositoryStub) UpdateLoginPolicy(context.Context, LoginPolicyUpdateInput, time.Time) (domain.LoginPolicy, error) {
	return domain.LoginPolicy{}, errors.New("not implemented")
}

func (stub *sessionTimeoutRepositoryStub) RecordFailedLogin(context.Context, LoginFailureInput, domain.LoginPolicy, time.Time) (LoginFailureResult, error) {
	return LoginFailureResult{}, errors.New("not implemented")
}

func (stub *sessionTimeoutRepositoryStub) ListLockedAccounts(context.Context, string, PageRequest, time.Time) (PageResult[domain.LockedAccount], error) {
	return PageResult[domain.LockedAccount]{}, errors.New("not implemented")
}

func (stub *sessionTimeoutRepositoryStub) UnlockAccount(context.Context, UnlockInput, time.Time) (domain.LockedAccount, error) {
	return domain.LockedAccount{}, errors.New("not implemented")
}

type sessionTimeoutClockStub struct{}

func (sessionTimeoutClockStub) Now() time.Time {
	return time.Date(2026, time.July, 27, 0, 0, 0, 0, time.UTC)
}

func TestSessionIdleTimeoutUsesPersistedPolicyAndDefault(t *testing.T) {
	t.Parallel()

	repository := &sessionTimeoutRepositoryStub{policy: domain.LoginPolicy{TenantID: "tenant-1", IdleTimeoutSeconds: 900}}
	service, err := NewService(repository, sessionTimeoutClockStub{})
	if err != nil {
		t.Fatalf("construct security service: %v", err)
	}

	timeout, err := service.SessionIdleTimeout(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("read persisted idle timeout: %v", err)
	}
	if timeout != 15*time.Minute {
		t.Fatalf("persisted idle timeout = %s, want 15m", timeout)
	}

	repository.err = ErrNotFound
	timeout, err = service.SessionIdleTimeout(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("read default idle timeout: %v", err)
	}
	if timeout != 30*time.Minute {
		t.Fatalf("default idle timeout = %s, want 30m", timeout)
	}
}

func TestUpdateLoginPolicyRejectsUnsafeIdleTimeout(t *testing.T) {
	t.Parallel()

	service, err := NewService(&sessionTimeoutRepositoryStub{}, sessionTimeoutClockStub{})
	if err != nil {
		t.Fatalf("construct security service: %v", err)
	}

	_, err = service.UpdateLoginPolicy(context.Background(), LoginPolicyUpdateInput{
		TenantID: "tenant-1", OperatorID: "admin-1",
		MaxFailedAttempts: 5, LockoutDurationSeconds: 900, FailureResetWindowSeconds: 1800,
		IdleTimeoutSeconds: 59,
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
}
