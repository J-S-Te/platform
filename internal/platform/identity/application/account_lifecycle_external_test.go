package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type accountLifecycleRepositoryStub struct {
	resetCalls      int
	initializeCalls int
	lastWrite       PasswordWrite
}

func (*accountLifecycleRepositoryStub) CreateLocalAccount(context.Context, LocalAccountCreateWrite) (domain.Account, error) {
	return domain.Account{}, errors.New("unexpected create")
}
func (repository *accountLifecycleRepositoryStub) InitializePassword(_ context.Context, write PasswordWrite) (domain.Account, error) {
	repository.initializeCalls++
	repository.lastWrite = write
	return domain.Account{ID: write.AccountID, PasswordInitialized: true}, nil
}
func (repository *accountLifecycleRepositoryStub) ResetPassword(_ context.Context, write PasswordWrite) (domain.Account, error) {
	repository.resetCalls++
	repository.lastWrite = write
	return domain.Account{}, ErrConflict
}
func (*accountLifecycleRepositoryStub) FindLocalPasswordCredential(context.Context, string, string) (LocalPasswordCredential, error) {
	return LocalPasswordCredential{}, errors.New("unexpected find")
}
func (*accountLifecycleRepositoryStub) ChangeOwnPassword(context.Context, PasswordWrite) error {
	return errors.New("unexpected change")
}

type passwordGeneratorStub struct{ password string }

func (generator passwordGeneratorStub) Generate() (string, error) { return generator.password, nil }

type verifierStub struct{}

func (verifierStub) Verify(string, string, []byte, []byte) (bool, error) { return false, nil }

func TestResetPasswordInitializesCredentialFreeExternalAccount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 2, 10, 0, 0, 0, time.UTC)
	repository := &accountLifecycleRepositoryStub{}
	service, err := NewAccountLifecycleService(
		repository,
		employeeOnboardingPasswordHasherStub{},
		verifierStub{},
		passwordGeneratorStub{password: "StrongExternal!2026"},
		&sequenceIDGenerator{ids: []string{"credential-1"}},
		fixedClock{now: now},
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.ResetPassword(context.Background(), PasswordResetInput{
		TenantID: "tenant-1", OperatorID: "operator-1", AccountID: "account-1", Version: 3,
	})
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if repository.resetCalls != 1 || repository.initializeCalls != 1 {
		t.Fatalf("reset calls=%d initialize calls=%d", repository.resetCalls, repository.initializeCalls)
	}
	if repository.lastWrite.CredentialID != "credential-1" || repository.lastWrite.RevokeReason != "ADMIN_PASSWORD_RESET" || repository.lastWrite.ExpectedVersion != 3 {
		t.Fatalf("initialize write=%+v", repository.lastWrite)
	}
	if result.AccountID != "account-1" || result.TemporaryPassword != "StrongExternal!2026" {
		t.Fatalf("result=%+v", result)
	}
}
