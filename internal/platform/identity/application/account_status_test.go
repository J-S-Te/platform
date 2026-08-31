package application

import (
	"context"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type accountListRepositoryStub struct {
	ManagementRepository
	result PageResult[domain.Account]
}

func (stub *accountListRepositoryStub) ListAccounts(context.Context, string, PageRequest) (PageResult[domain.Account], error) {
	return stub.result, nil
}

type externalAccountStatusReaderStub struct {
	calls int
}

func (stub *externalAccountStatusReaderStub) ReadAccountStatus(context.Context, string, string) (string, bool, error) {
	stub.calls++
	return domain.StatusDisabled, true, nil
}

func TestListAccountsReflectsDisabledExternalAccount(t *testing.T) {
	userID := "user-1"
	repository := &accountListRepositoryStub{result: PageResult[domain.Account]{Items: []domain.Account{
		{ID: "account-keycloak", UserID: &userID, AuthSource: "KEYCLOAK", Status: domain.StatusActive},
		{ID: "account-local", AuthSource: "LOCAL", Status: domain.StatusActive},
	}}}
	reader := &externalAccountStatusReaderStub{}
	service, err := NewManagementService(repository, userCreateMobileProtectionStub{}, &sequenceIDGenerator{}, fixedClock{})
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}
	service.SetExternalAccountStatusReader(reader)

	result, err := service.ListAccounts(context.Background(), "tenant-1", PageRequest{})
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if result.Items[0].Status != domain.StatusDisabled {
		t.Fatalf("external account status = %q, want DISABLED", result.Items[0].Status)
	}
	if result.Items[1].Status != domain.StatusActive || reader.calls != 1 {
		t.Fatalf("local account status/calls = %q/%d, want ACTIVE/1", result.Items[1].Status, reader.calls)
	}
}
