package migrations

import (
	"strings"
	"testing"
)

// 审计导出一次性下载票据表契约：只保存 SHA-256 摘要，唯一键保证一张票据只有一行，
// 租户/任务索引支撑按"当前租户 + 当前任务"的兑换与清理查询。
func TestAuditExportDownloadTicketMigrationStoresOnlyDigests(t *testing.T) {
	body, err := Files.ReadFile("000139_audit_export_download_ticket.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	text := string(body)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS audit_export_download_ticket",
		"id CHAR(26)",
		"tenant_id CHAR(26)",
		"job_id CHAR(26)",
		// 摘要列固定 32 字节（SHA-256），不接受变长文本：变长列容易被误写成明文。
		"token_hash BINARY(32) NOT NULL",
		"expires_at DATETIME(3) NOT NULL",
		"used_at DATETIME(3) NULL",
		"created_at DATETIME(3) NOT NULL",
		"PRIMARY KEY (id)",
		// 同一个摘要只能对应一张票据，重放/冲突在写入时即被数据库拒绝。
		"UNIQUE KEY uk_audit_export_download_ticket_hash (token_hash)",
		// 兑换与清理都按（租户、任务）定位，需要独立索引而不是全表扫描。
		"KEY idx_audit_export_download_ticket_job (tenant_id, job_id, created_at)",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing %q", required)
		}
	}
	// 只检查可执行 DDL：注释里说明"不保存明文"不构成违规。
	ddl := strings.Join(commentFreeLines(text), "\n")
	lower := strings.ToLower(ddl)
	for _, forbidden := range []string{"token ", "raw_token", "plaintext", "token_value", "cookie", "password"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("download ticket table must not persist %s material", forbidden)
		}
	}
	// 迁移必须保持可重试、只新增对象：不得改写或删除既有表的数据。
	for _, forbidden := range []string{"DROP TABLE", "ALTER TABLE", "DELETE FROM", "UPDATE ", "INSERT INTO"} {
		if strings.Contains(ddl, forbidden) {
			t.Fatalf("migration must stay additive, found %s", forbidden)
		}
	}
	// 历史票据没有明文可回填，任何回填语句都意味着凭空造出可兑换的凭证。
	if strings.Contains(lower, "select") {
		t.Fatalf("migration must not backfill tickets, found SELECT in executable DDL")
	}
}
