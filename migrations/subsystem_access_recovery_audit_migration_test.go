package migrations

import (
	"strings"
	"testing"
)

// 取代收口审计表的迁移契约：只追加、记录审核基准与状态快照、且不改写既有表。
func TestSubsystemAccessRecoveryAuditMigrationIsAppendOnly(t *testing.T) {
	body, err := Files.ReadFile("000141_subsystem_access_recovery_audit.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	text := string(body)
	lower := strings.ToLower(text)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS subsystem_access_recovery_audit",
		// 租户/目标隔离
		"tenant_id", "application_code", "environment_code",
		// 原任务与原代次
		"operation_id", "generation",
		// 审核基准：原回执清单摘要与当前清单摘要
		"receipt_manifest_checksum", "current_profile_checksum",
		// 收口前的部署状态快照
		"state_status", "state_generation", "state_desired_manifest_checksum", "state_applied_manifest_checksum",
		// 操作者与原因
		"operator_id", "reason", "created_at",
		// 只追加：数据库层拒绝改写与删除
		"subsystem_access_recovery_audit_no_update",
		"subsystem_access_recovery_audit_no_delete",
		"SIGNAL SQLSTATE",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing %s", required)
		}
	}
	// 非秘密约束：审计行不得保存凭据或运行配置内容。
	for _, forbidden := range []string{"secret", "password", "credential"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("recovery audit must not persist %s material", forbidden)
		}
	}
	// 附加约束：不得删除、改写或重建既有表；本迁移只新增一张表及其触发器。
	for _, forbidden := range []string{"DROP TABLE", "ALTER TABLE", "UPDATE subsystem_access", "DELETE FROM subsystem_access"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("migration must stay additive, found %s", forbidden)
		}
	}
	if strings.Contains(text, "DEFAULT ,") || strings.Contains(text, "DEFAULT )") {
		t.Fatal("migration contains an empty DEFAULT clause; it would fail to execute on a real database")
	}
}
