package applicationaccess

import (
	"testing"
	"time"
)

func TestSortedUniqueTrimsDeduplicatesAndSorts(t *testing.T) {
	got := sortedUnique([]string{" contract.read ", "", "contract.read", "contract.create"})
	want := []string{"contract.create", "contract.read"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNormalizeRoleInputsAllowsMultipleRolesAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	got, err := normalizeRoleInputs([]RoleInput{
		{RoleCode: "audit_admin", ScopeType: "APPLICATION"},
		{RoleCode: "sales", ScopeType: "TENANT"},
		{RoleCode: "sales", ScopeType: "APPLICATION"},
	}, true, now)
	if err != nil {
		t.Fatalf("normalizeRoleInputs returned error: %v", err)
	}
	if len(got) != 2 || got[0].RoleCode != "audit_admin" || got[1].RoleCode != "sales" {
		t.Fatalf("unexpected normalized roles: %+v", got)
	}
}

func TestNormalizeRoleInputsAllowsEnvironmentScope(t *testing.T) {
	got, err := normalizeRoleInputs([]RoleInput{{RoleCode: "sales", ScopeType: "ENVIRONMENT", EnvironmentCode: "prod"}}, true, time.Now().UTC())
	if err != nil {
		t.Fatalf("normalizeRoleInputs returned error: %v", err)
	}
	if len(got) != 1 || got[0].ScopeType != "ENVIRONMENT" || got[0].EnvironmentCode != "prod" {
		t.Fatalf("unexpected normalized environment role: %+v", got)
	}
}

func TestNormalizeRoleInputsRejectsApplicationEnvironmentCode(t *testing.T) {
	_, err := normalizeRoleInputs([]RoleInput{{RoleCode: "sales", ScopeType: "APPLICATION", EnvironmentCode: "prod"}}, true, time.Now().UTC())
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestNormalizeRoleInputsRejectsExpiredRole(t *testing.T) {
	expired := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	_, err := normalizeRoleInputs([]RoleInput{{RoleCode: "sales", ValidUntil: &expired}}, true, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	if err == nil || !errorsIs(err, ErrValidation) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
