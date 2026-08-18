package application

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type bootstrapRepositorySpy struct {
	BootstrapRepository
	writes []BootstrapWrite
}

func (repository *bootstrapRepositorySpy) BootstrapFirstSuperAdmin(_ context.Context, write BootstrapWrite) (BootstrapResult, error) {
	repository.writes = append(repository.writes, write)
	return BootstrapResult{BootstrapID: write.BootstrapID, UserID: write.UserID}, nil
}

type bootstrapPasswordHasherStub struct{}

func (bootstrapPasswordHasherStub) Hash(string) ([]byte, []byte, error) {
	return []byte("password-digest"), []byte("algorithm-params"), nil
}

func TestBootstrapCreatesPrimaryMembershipWrite(t *testing.T) {
	t.Parallel()

	repository := &bootstrapRepositorySpy{}
	now := time.Date(2026, time.August, 18, 8, 0, 0, 0, time.UTC)
	service, err := NewBootstrapService(
		repository,
		bootstrapPasswordHasherStub{},
		&sequenceIDGenerator{ids: []string{"bootstrap-1", "user-1", "account-1", "credential-1", "membership-1", "role-binding-1"}},
		fixedClock{now: now},
	)
	if err != nil {
		t.Fatalf("construct bootstrap service: %v", err)
	}

	_, err = service.InitializeFirstSuperAdmin(context.Background(), BootstrapInput{
		DisplayName: "平台管理员",
		AccountName: "platform-admin",
		Password:    "StrongPassword!2026",
	})
	if err != nil {
		t.Fatalf("initialize first super administrator: %v", err)
	}
	if len(repository.writes) != 1 {
		t.Fatalf("bootstrap writes = %d, want 1", len(repository.writes))
	}

	write := repository.writes[0]
	if write.MembershipID != "membership-1" {
		t.Errorf("membership ID = %q, want %q", write.MembershipID, "membership-1")
	}
	if got, want := []string{write.BootstrapID, write.UserID, write.AccountID, write.CredentialID, write.MembershipID, write.RoleBindingID}, []string{"bootstrap-1", "user-1", "account-1", "credential-1", "membership-1", "role-binding-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("generated IDs = %#v, want %#v", got, want)
	}
	if !write.InitializedAt.Equal(now) {
		t.Errorf("initialized at = %s, want %s", write.InitializedAt, now)
	}
}
