package positiongrant

import (
	"testing"
)

func TestGeneratedTemplateCodeUsesStableManagedPrefix(t *testing.T) {
	t.Parallel()

	const id = "01KYDVHC000000000000000003"
	if got, want := generatedTemplateCode(id), "POSITION-TEMPLATE-01KYDVHC000000000000000003"; got != want {
		t.Errorf("generated template code = %q, want %q", got, want)
	}
}

func TestNormalizeTemplateInputDoesNotRequireClientCode(t *testing.T) {
	t.Parallel()

	input := TemplateInput{
		Name:  "  销售人员授权模板  ",
		Roles: []TemplateRoleInput{{ApplicationID: "application-1", RoleID: "role-1"}},
	}
	if err := normalizeTemplateInput(&input); err != nil {
		t.Fatalf("normalize template input: %v", err)
	}
	if input.Name != "销售人员授权模板" {
		t.Errorf("normalized name = %q", input.Name)
	}
}

func TestValidateTemplateRoleSetRejectsSameRoleAcrossScopes(t *testing.T) {
	t.Parallel()

	err := validateTemplateRoleSet([]TemplateRoleInput{
		{ApplicationID: "application-1", RoleID: "role-sales", ScopeType: scopeTenant},
		{ApplicationID: "application-1", RoleID: "role-sales", ScopeType: scopeEnvironment, ScopeID: "environment-1"},
	})
	if err == nil {
		t.Fatal("validateTemplateRoleSet should reject the same application role across scopes")
	}
}

func TestActiveRoleIDsByApplicationIgnoresDisabledItems(t *testing.T) {
	t.Parallel()

	roles := activeRoleIDsByApplication([]TemplateRoleInput{
		{ApplicationID: "application-1", RoleID: "role-1", Status: activeStatus},
		{ApplicationID: "application-1", RoleID: "role-2", Status: disabledStatus},
	})
	if got := len(roles["application-1"]); got != 1 {
		t.Fatalf("active role count = %d, want 1", got)
	}
	if _, ok := roles["application-1"]["role-1"]; !ok {
		t.Fatal("active role was not retained")
	}
}
