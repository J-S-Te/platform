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

type externalAccountStatusSynchronizerStub struct {
	calls   int
	userIDs []string
}

func (stub *externalAccountStatusSynchronizerStub) DisableExternalIdentity(_ context.Context, _ string, userID string) error {
	stub.calls++
	stub.userIDs = append(stub.userIDs, userID)
	return nil
}

func TestListAccountsReflectsDisabledExternalAccount(t *testing.T) {
	userID := "user-1"
	repository := &accountListRepositoryStub{result: PageResult[domain.Account]{Items: []domain.Account{
		{ID: "account-keycloak", UserID: &userID, AuthSource: "KEYCLOAK", Status: domain.StatusActive},
		{ID: "account-local", AuthSource: "LOCAL", Status: domain.StatusActive, UserID: &userID},
	}}}
	reader := &externalAccountStatusReaderStub{}
	synchronizer := &externalAccountStatusSynchronizerStub{}
	service, err := NewManagementService(repository, userCreateMobileProtectionStub{}, &sequenceIDGenerator{}, fixedClock{})
	if err != nil {
		t.Fatalf("construct management service: %v", err)
	}
	service.SetExternalAccountStatusReader(reader)
	service.SetExternalAccountStatusSynchronizer(synchronizer)

	result, err := service.ListAccounts(context.Background(), "tenant-1", PageRequest{})
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if result.Items[0].Status != domain.StatusDisabled {
		t.Fatalf("external account status = %q, want DISABLED", result.Items[0].Status)
	}
	if result.Items[1].Status != domain.StatusDisabled || reader.calls != 2 {
		t.Fatalf("local-linked account status/calls = %q/%d, want DISABLED/2", result.Items[1].Status, reader.calls)
	}
	// 读取账号列表是只读操作：外置停用只反映在返回值里，不能写回业务状态。
	// 允许 GET 触发停用会让只读权限升级为"停用用户、账号、凭据、任职和会话"。
	if synchronizer.calls != 0 {
		t.Fatalf("synchronizer calls = %d, want 0（只读列表不得写业务状态）", synchronizer.calls)
	}
}
