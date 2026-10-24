package bootstrapadmin

import (
	"errors"
	"testing"
	"time"
)

func validSnapshot() snapshot {
	return snapshot{
		TenantID:                "tenant-1",
		TenantCode:              "default",
		StateExists:             true,
		StateTenantID:           "tenant-1",
		StateUserID:             "user-bootstrap",
		StateAccountID:          "account-bootstrap",
		UserExists:              true,
		UserStatus:              activeStatus,
		AccountExists:           true,
		AccountUserID:           "user-bootstrap",
		AccountStatus:           activeStatus,
		GrantActive:             true,
		SuperAdminBindingExists: true,
	}
}

func TestEvaluateReturnsIdentityForCompleteInitialization(t *testing.T) {
	identity, initialized, err := evaluate(validSnapshot())
	if err != nil {
		t.Fatalf("evaluate() error = %v, want nil", err)
	}
	if !initialized {
		t.Fatal("evaluate() initialized = false, want true")
	}
	if identity.TenantID != "tenant-1" || identity.UserID != "user-bootstrap" || identity.AccountID != "account-bootstrap" {
		t.Fatalf("evaluate() identity = %#v", identity)
	}
}

func TestEvaluateTreatsUninitializedDefaultTenantAsNotInitialized(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.StateExists = false
	snapshot.SuperAdminBindingExists = false

	_, initialized, err := evaluate(snapshot)
	if err != nil {
		t.Fatalf("evaluate() error = %v, want nil", err)
	}
	if initialized {
		t.Fatal("evaluate() initialized = true, want false before bootstrap")
	}
}

func TestEvaluateReportsMissingRecordWhenDefaultTenantWasInitialized(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.StateExists = false

	_, initialized, err := evaluate(snapshot)
	if initialized {
		t.Fatal("evaluate() initialized = true, want fail closed")
	}
	if !errors.Is(err, ErrConfiguration) {
		t.Fatalf("evaluate() error = %v, want ErrConfiguration", err)
	}
}

func TestEvaluateIgnoresMissingRecordOutsideBootstrapTenant(t *testing.T) {
	snapshot := validSnapshot()
	snapshot.StateExists = false
	snapshot.TenantCode = "tenant-other"

	_, initialized, err := evaluate(snapshot)
	if err != nil {
		t.Fatalf("evaluate() error = %v, want nil", err)
	}
	if initialized {
		t.Fatal("evaluate() initialized = true, want false for a non-bootstrap tenant")
	}
}

func TestEvaluateFailsClosedForBrokenAssociations(t *testing.T) {
	cases := map[string]func(*snapshot){
		"missing user":            func(s *snapshot) { s.UserExists = false },
		"deleted user":            func(s *snapshot) { s.UserDeleted = true },
		"disabled user":           func(s *snapshot) { s.UserStatus = "DISABLED" },
		"missing account":         func(s *snapshot) { s.AccountExists = false },
		"unlinked account":        func(s *snapshot) { s.AccountUserID = "other-user" },
		"disabled account":        func(s *snapshot) { s.AccountStatus = "DISABLED" },
		"inactive grant":          func(s *snapshot) { s.GrantActive = false },
		"incomplete state user":   func(s *snapshot) { s.StateUserID = "" },
		"incomplete state acct":   func(s *snapshot) { s.StateAccountID = "" },
		"state tenant mismatched": func(s *snapshot) { s.StateTenantID = "tenant-other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			snapshot := validSnapshot()
			mutate(&snapshot)
			_, initialized, err := evaluate(snapshot)
			if initialized {
				t.Fatal("evaluate() initialized = true, want fail closed")
			}
			if !errors.Is(err, ErrConfiguration) {
				t.Fatalf("evaluate() error = %v, want ErrConfiguration", err)
			}
		})
	}
}

func TestIdentityProtectsOnlyTheInitializedIdentifiers(t *testing.T) {
	identity := Identity{TenantID: "tenant-1", UserID: "user-bootstrap", AccountID: "account-bootstrap"}
	if !identity.ProtectsUser("user-bootstrap") {
		t.Fatal("ProtectsUser() = false for the initialized user")
	}
	if identity.ProtectsUser("user-ordinary") {
		t.Fatal("ProtectsUser() = true for an ordinary user")
	}
	if identity.ProtectsUser("") {
		t.Fatal("ProtectsUser() = true for an empty user id")
	}
	if !identity.ProtectsAccount("account-bootstrap") {
		t.Fatal("ProtectsAccount() = false for the initialized account")
	}
	if identity.ProtectsAccount("account-ordinary") {
		t.Fatal("ProtectsAccount() = true for an ordinary account")
	}
	if (Identity{}).ProtectsUser("") {
		t.Fatal("empty identity must not protect an empty user id")
	}
}

func TestUserUpdateViolationOnlyBlocksWeakeningChanges(t *testing.T) {
	identity := Identity{UserID: "user-bootstrap", AccountID: "account-bootstrap"}
	disabled := "DISABLED"
	if !UserUpdateViolation(identity, "user-bootstrap", &disabled, false, nil) {
		t.Fatal("disabling the initialized user must be blocked")
	}
	if UserUpdateViolation(identity, "user-bootstrap", nil, false, nil) {
		t.Fatal("renaming the initialized user must stay allowed")
	}
	active := "ACTIVE"
	if UserUpdateViolation(identity, "user-bootstrap", &active, false, nil) {
		t.Fatal("an ACTIVE no-op status must stay allowed")
	}
	expiry := time.Now().UTC().Add(24 * time.Hour)
	if !UserUpdateViolation(identity, "user-bootstrap", nil, true, &expiry) {
		t.Fatal("a finite validity must be blocked")
	}
	if UserUpdateViolation(identity, "user-bootstrap", nil, true, nil) {
		t.Fatal("clearing the validity must stay allowed")
	}
	if UserUpdateViolation(identity, "user-ordinary", &disabled, true, &expiry) {
		t.Fatal("ordinary users must keep their existing management capabilities")
	}
}

func TestAccountUpdateViolationOnlyBlocksWeakeningChanges(t *testing.T) {
	identity := Identity{UserID: "user-bootstrap", AccountID: "account-bootstrap"}
	if !AccountUpdateViolation(identity, "account-bootstrap", "DISABLED", false, nil) {
		t.Fatal("disabling the initialized account must be blocked")
	}
	if AccountUpdateViolation(identity, "account-bootstrap", "ACTIVE", false, nil) {
		t.Fatal("an ACTIVE no-op status must stay allowed")
	}
	expiry := time.Now().UTC().Add(24 * time.Hour)
	if !AccountUpdateViolation(identity, "account-bootstrap", "ACTIVE", true, &expiry) {
		t.Fatal("a finite account validity must be blocked")
	}
	if AccountUpdateViolation(identity, "account-bootstrap", "ACTIVE", true, nil) {
		t.Fatal("clearing the account validity must stay allowed")
	}
	if AccountUpdateViolation(identity, "account-ordinary", "DISABLED", true, &expiry) {
		t.Fatal("ordinary accounts must keep their existing management capabilities")
	}
}
