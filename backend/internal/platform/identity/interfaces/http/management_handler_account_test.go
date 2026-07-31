package identityhttp

import (
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/domain"
)

func TestToAccountResponseIncludesLinkedUserReference(t *testing.T) {
	userID := "user-1"
	response := toAccountResponse(domain.Account{
		ID:     "account-1",
		UserID: &userID,
		User:   &domain.ReferenceName{ID: userID, Name: "张三"},
	})

	if response.UserID == nil || *response.UserID != userID {
		t.Fatalf("user_id = %#v, want %q", response.UserID, userID)
	}
	if response.User == nil {
		t.Fatal("linked user reference is nil")
	}
	if response.User.ID != userID || response.User.Name != "张三" {
		t.Fatalf("linked user = %#v", response.User)
	}
}

func TestToAccountResponseOmitsUserReferenceForServiceAccount(t *testing.T) {
	response := toAccountResponse(domain.Account{ID: "service-account-1"})
	if response.User != nil {
		t.Fatalf("service account user = %#v, want nil", response.User)
	}
}
