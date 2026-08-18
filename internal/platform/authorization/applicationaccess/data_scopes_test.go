package applicationaccess

import (
	"reflect"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
)

func TestDataScopesFromRolesDeduplicatesByEffectiveBoundary(t *testing.T) {
	roles := []RoleView{
		{Code: "crm_super_admin", ScopeType: "TENANT", ScopeID: "", EnvironmentCode: ""},
		{Code: "crm_super_admin", ScopeType: "TENANT", ScopeID: "", EnvironmentCode: ""},
		{Code: "sales", ScopeType: "ORG", ScopeID: "org-1", EnvironmentCode: "dev"},
		{Code: "sales", ScopeType: "ORG", ScopeID: "org-2", EnvironmentCode: "dev"},
		{Code: "sales", ScopeType: "ORG", ScopeID: "org-1", EnvironmentCode: "dev"},
		{Code: "auditor", ScopeType: "ENVIRONMENT", ScopeID: "env-1", EnvironmentCode: "dev"},
	}
	want := []tokenissuer.DataScope{
		{RoleCode: "crm_super_admin", ScopeType: "TENANT", ScopeID: "", EnvironmentCode: ""},
		{RoleCode: "sales", ScopeType: "ORG", ScopeID: "org-1", EnvironmentCode: "dev"},
		{RoleCode: "sales", ScopeType: "ORG", ScopeID: "org-2", EnvironmentCode: "dev"},
		{RoleCode: "auditor", ScopeType: "ENVIRONMENT", ScopeID: "env-1", EnvironmentCode: "dev"},
	}
	if got := dataScopesFromRoles(roles); !reflect.DeepEqual(got, want) {
		t.Fatalf("dataScopesFromRoles() = %#v, want %#v", got, want)
	}
}

func TestDataScopesFromRolesEmpty(t *testing.T) {
	if got := dataScopesFromRoles(nil); got == nil || len(got) != 0 {
		t.Fatalf("dataScopesFromRoles(nil) = %#v, want empty non-nil slice", got)
	}
}
