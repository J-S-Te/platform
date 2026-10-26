package infrastructure

import (
	"testing"
	"time"
)

func TestToDomainAccountPreservesLockedUntil(t *testing.T) {
	lockedUntil := time.Date(2026, 10, 26, 11, 40, 27, 0, time.UTC)
	account := toDomainAccount(accountModel{ID: "account-1", TenantID: "tenant-1", Status: "ACTIVE", LockedUntil: &lockedUntil})
	if account.LockedUntil == nil || !account.LockedUntil.Equal(lockedUntil) {
		t.Fatalf("locked until not mapped: %v", account.LockedUntil)
	}
}
