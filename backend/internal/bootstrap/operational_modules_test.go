package bootstrap

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	applicationregistryapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
)

func TestHardcodedInitialSubsystemAdministratorRoles(t *testing.T) {
	if got := hardcodedInitialSubsystemAdministratorRoles("contract_management"); !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("contract initial roles = %v", got)
	}
	if got := hardcodedInitialSubsystemAdministratorRoles("customer_and_opportunity"); !reflect.DeepEqual(got, []string{"sales_director", "team_lead", "technical_lead"}) {
		t.Fatalf("customer initial roles = %v", got)
	}
	if got := hardcodedInitialSubsystemAdministratorRoles("customer_portal"); len(got) != 0 {
		t.Fatalf("portal must not grant the internal onboarding operator a customer role: %v", got)
	}
	if got := hardcodedInitialSubsystemAdministratorRoles("project_management"); !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("project initial roles = %v", got)
	}
}

// TestInitialAdminRolesManifestDrivenMatchesHardcodedDefaults 验证 B1 解耦的等价性：
// 清单声明的初始管理员角色（与平台硬编码默认一致）在开关开启时生效；未声明或开关关闭时
// 一律回退硬编码默认，行为不变。
func TestInitialAdminRolesManifestDrivenMatchesHardcodedDefaults(t *testing.T) {
	capabilities := applicationregistryapplication.SubsystemProvisioningCapabilities{
		Targets: []applicationregistryapplication.SubsystemProvisioningTarget{
			{ApplicationCode: "contract_management", InitialAdminRoles: []string{"admin"}},
			{ApplicationCode: "customer_and_opportunity", InitialAdminRoles: []string{"sales_director", "team_lead", "technical_lead"}},
			{ApplicationCode: "customer_portal", InitialAdminRoles: []string{}},
			// project_management 故意不声明：应回退硬编码默认 ["admin"]。
			{ApplicationCode: "project_management"},
		},
	}
	lookup := initialAdminRolesByApplication(capabilities)
	manager := subsystemInitialAccessManager{
		initialAdminRolesByApplication: lookup,
		fromManifest:                   true,
		logger:                         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// 开关开启：已声明应用结果必须与硬编码默认一致（等价重构）。
	for code := range lookup {
		got := manager.initialAdministratorRoles(code)
		want := hardcodedInitialSubsystemAdministratorRoles(code)
		if !equalStringSlices(got, want) {
			t.Fatalf("manifest roles for %s = %v, want hardcoded default %v", code, got, want)
		}
	}
	// 未声明应用：回退硬编码默认。
	project := manager.initialAdministratorRoles("project_management")
	if !equalStringSlices(project, []string{"admin"}) {
		t.Fatalf("undeclared project fallback = %v, want [admin]", project)
	}
	unknown := manager.initialAdministratorRoles("unreviewed_system")
	if !equalStringSlices(unknown, hardcodedInitialSubsystemAdministratorRoles("unreviewed_system")) {
		t.Fatalf("unknown fallback = %v", unknown)
	}
	// 开关关闭：一律回退硬编码默认。
	manager.fromManifest = false
	for _, code := range []string{"contract_management", "customer_and_opportunity", "customer_portal", "project_management"} {
		got := manager.initialAdministratorRoles(code)
		want := hardcodedInitialSubsystemAdministratorRoles(code)
		if !equalStringSlices(got, want) {
			t.Fatalf("switch-off roles for %s = %v, want %v", code, got, want)
		}
	}
}
