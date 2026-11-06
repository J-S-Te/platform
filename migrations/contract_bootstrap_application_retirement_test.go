package migrations_test

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/migrations"
)

// 合同管理不再由迁移内嵌注册：000136 只在完全没有使用证据时删除 000045 的引导应用行。
// 该静态门禁锁定安全边界，避免后续误改成无条件删除真实接入记录。
func TestLegacyContractBootstrapApplicationRetirementKeepsEvidence(t *testing.T) {
	content, err := migrations.Files.ReadFile("000136_retire_contract_management_bootstrap_application.sql")
	if err != nil {
		t.Fatalf("read contract bootstrap retirement migration: %v", err)
	}
	sql := string(content)
	required := []string{
		"DELETE application",
		"application.id = '01J00000000000000000000300'",
		"application.code = 'contract_management'",
		"application.created_by IS NULL",
		"NOT EXISTS (SELECT 1 FROM platform_application_environment",
		"NOT EXISTS (SELECT 1 FROM platform_application_login_target",
		"NOT EXISTS (SELECT 1 FROM platform_oauth_client",
		"NOT EXISTS (SELECT 1 FROM subsystem_deployment_state",
		"NOT EXISTS (SELECT 1 FROM subsystem_service_instance",
		"NOT EXISTS (SELECT 1 FROM authz_authorization_catalog",
		"NOT EXISTS (SELECT 1 FROM authz_role",
		"NOT EXISTS (SELECT 1 FROM authz_permission",
		"NOT EXISTS (SELECT 1 FROM keycloak_application_client_mapping",
		"NOT EXISTS (SELECT 1 FROM keycloak_authorization_projection",
	}
	for _, fragment := range required {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("contract bootstrap retirement migration is missing %q", fragment)
		}
	}
	if strings.Contains(sql, "DELETE FROM platform_application\n") {
		t.Fatal("contract bootstrap retirement must stay evidence-gated, not unconditional")
	}
}
