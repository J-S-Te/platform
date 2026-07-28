package sys004

import (
	"reflect"
	"testing"
)

func TestSYS004CatalogIsStable(t *testing.T) {
	wantPermissions := []string{
		"all", "approval.process", "approval.view", "audit.read", "audit.view", "contract.create",
		"contract.delete", "contract.edit", "contract.read", "contract_template.manage", "contract_template.read",
		"contract_type.manage", "customer.create", "customer.delete", "customer.edit", "customer.read",
		"dashboard", "user.manage",
	}
	if got := PermissionCodes(); !reflect.DeepEqual(got, wantPermissions) {
		t.Fatalf("PermissionCodes() = %#v, want %#v", got, wantPermissions)
	}
	wantRoles := []string{"admin", "audit_admin", "finance_director", "sales", "sales_director", "tech_director"}
	if got := RoleCodes(); !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("RoleCodes() = %#v, want %#v", got, wantRoles)
	}
	const wantHash = "4bf7340872f586174d367416be7914fccbd4af0a5e9da5179025a1e5bc01ea21"
	if got := RoleConfigHash(); got != wantHash {
		t.Fatalf("RoleConfigHash() = %q, want %q", got, wantHash)
	}
}

func TestProtectedPermissionsCannotBeGrantedDirectly(t *testing.T) {
	for _, code := range []string{"all", "user.manage", "unknown.permission"} {
		if IsCustomPermissionAllowed(code) {
			t.Fatalf("IsCustomPermissionAllowed(%q) = true, want false", code)
		}
	}
	if !IsCustomPermissionAllowed("contract_template.manage") {
		t.Fatal("contract_template.manage should be allowed as a custom permission")
	}
}
