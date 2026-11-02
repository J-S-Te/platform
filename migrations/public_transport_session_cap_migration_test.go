package migrations

import (
	"strings"
	"testing"
)

// 降级截断的原值记录表契约（BIZ-RUNTIME-06）：只保存恢复所需的非秘密元数据，
// 唯一键必须能保证"同一切换、同一表、同一会话"只记录一次（重试幂等）。
func TestPublicTransportSessionCapMigrationKeepsRecoverableMetadata(t *testing.T) {
	body, err := Files.ReadFile("000134_create_public_transport_session_cap.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	text := string(body)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS platform_public_transport_session_cap",
		"transition_id",
		"session_table ENUM('iam_session', 'oauth_token_family', 'oauth_refresh_token')",
		"session_id",
		"original_expires_at DATETIME(3) NOT NULL",
		"capped_expires_at DATETIME(3) NOT NULL",
		"created_at DATETIME(3) NOT NULL",
		"PRIMARY KEY (transition_id, session_table, session_id)",
		// 恢复语句按 (session_table, session_id) 反查，需要独立索引。
		"idx_public_transport_session_cap_session (session_table, session_id)",
		// 原值不可能早于截断值：写入方向反了会让"恢复"变成延长会话。
		"chk_public_transport_session_cap_order CHECK (original_expires_at >= capped_expires_at)",
		// 切换删除时的引用完整性（历史切换不删除，保持 RESTRICT 语义）。
		"fk_public_transport_session_cap_transition",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("migration missing %q", required)
		}
	}
	// 该表只记录到期时间元数据，不得持久化任何会话内容或凭据。
	// 只检查可执行 DDL，注释里说明"不保存 Cookie"不算违规。
	ddl := strings.Join(commentFreeLines(text), "\n")
	lower := strings.ToLower(ddl)
	for _, forbidden := range []string{"token_hash", "refresh_token_hash", "client_secret", "password", "cookie"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("session cap table must not persist %s material", forbidden)
		}
	}
	// 迁移必须保持可重试：不得删除既有表，也不得改写历史切换记录。
	for _, forbidden := range []string{"DROP TABLE", "ALTER TABLE platform_public_transport_transition", "DELETE FROM"} {
		if strings.Contains(ddl, forbidden) {
			t.Fatalf("migration must stay additive, found %s", forbidden)
		}
	}
}

// 历史切换没有原始到期时间可用，因此本迁移不做回填；回填语句一旦出现就意味着
// 生产升级会凭空改写会话到期时间。
func TestPublicTransportSessionCapMigrationDoesNotBackfillSessions(t *testing.T) {
	body, err := Files.ReadFile("000134_create_public_transport_session_cap.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	ddl := strings.Join(commentFreeLines(string(body)), "\n")
	for _, forbidden := range []string{"UPDATE iam_session", "UPDATE oauth_token_family", "UPDATE oauth_refresh_token", "INSERT INTO platform_public_transport_session_cap SELECT"} {
		if strings.Contains(ddl, forbidden) {
			t.Fatalf("migration must not backfill or rewrite session lifetimes, found %q", forbidden)
		}
	}
}

// commentFreeLines 去掉整行 SQL 注释，用于只检查可执行语句。
func commentFreeLines(script string) []string {
	lines := make([]string, 0, 32)
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}
