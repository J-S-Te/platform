package migrations

import (
	"strings"
	"testing"
)

// 统一接入向导的操作表契约：只保存非秘密信息、单目标唯一执行槽、状态与阶段受 CHECK 约束，
// 且与既有部署状态表（subsystem_deployment_state）保持并存而非替代。
func TestSubsystemAccessOperationMigrationKeepsProgressAndConcurrency(t *testing.T) {
	body, err := Files.ReadFile("000122_subsystem_access_operations.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	text := string(body)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS subsystem_access_operation",
		"CREATE TABLE IF NOT EXISTS subsystem_access_operation_stage",
		// 幂等：同租户同幂等键只允许一条
		"uq_subsystem_access_operation_idempotency",
		// 并发：同目标只允许一个执行中操作
		"active_slot",
		"uq_subsystem_access_operation_active",
		// 阶段记录与操作一一对应且随操作删除
		"uq_subsystem_access_operation_stage",
		"fk_subsystem_access_stage_operation",
		// 状态机取值
		"chk_subsystem_access_operation_action",
		"chk_subsystem_access_operation_status",
		"chk_subsystem_access_operation_stage",
		"chk_subsystem_access_stage_status",
		// 租约与重试
		"lease_owner", "lease_expires_at", "attempt_count",
		// 结果证据引用（不落 Secret）
		"result_ref", "evidence_ref",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing %s", required)
		}
	}
	// 非秘密约束：向导表不得保存凭据类字段。
	for _, forbidden := range []string{"secret", "password", "credential", "token"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("operation tables must not persist %s material", forbidden)
		}
	}
	// 并存约束：不得改写既有部署状态表。
	for _, forbidden := range []string{"DROP TABLE", "ALTER TABLE subsystem_deployment_state"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("migration must stay additive, found %s", forbidden)
		}
	}
}

// 空默认值守卫：DEFAULT 后直接跟逗号或右括号（引号被吞掉的空串）会让迁移在真实数据库上无法执行；
// 该缺陷曾真实出现，故用文本守卫 + 隔离数据库执行测试双重拦截。
func TestSubsystemAccessOperationMigrationHasNoEmptyDefaultClause(t *testing.T) {
	body, err := Files.ReadFile("000122_subsystem_access_operations.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	text := string(body)
	if strings.Contains(text, "DEFAULT ,") || strings.Contains(text, "DEFAULT )") {
		t.Fatal("migration contains an empty DEFAULT clause; it would fail to execute on a real database")
	}
}
