package migration_test

import (
	"context"
	"os"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
)

// TestHandoverOrchestrationMigrationApplies 在专用测试库上执行完整迁移链，
// 验证人员交接编排表结构（批次/系统状态/回执）与交接项新列可用。
// 未配置 PLATFORM_MIGRATION_TEST_DSN 时跳过；该 DSN 必须指向专用空数据库。
func TestHandoverOrchestrationMigrationApplies(t *testing.T) {
	dsn := os.Getenv("PLATFORM_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_MIGRATION_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Run(context.Background(), database, migrations.Files); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	for _, table := range []string{"iam_personnel_handover_batch", "iam_personnel_handover_system", "iam_personnel_handover_receipt"} {
		if !database.Migrator().HasTable(table) {
			t.Fatalf("table %s is missing after migrations", table)
		}
	}
	for _, column := range []string{"batch_id", "expected_version", "applied_version", "idempotency_key", "lease_id", "lease_deadline", "attempts", "next_retry_at", "last_error_code", "evidence_ref", "version", "category", "resource_name"} {
		if !database.Migrator().HasColumn("iam_personnel_handover_item", column) {
			t.Fatalf("column %s is missing on iam_personnel_handover_item", column)
		}
	}
	// 编排需要 FAILED/SKIPPED；约束放宽后必须接受新取值。
	if err := database.Exec("INSERT INTO iam_personnel_handover_item (id, tenant_id, request_id, system_code, resource_type, resource_id, current_owner_id, status, created_at, updated_at) VALUES ('01HANDOVERITEM00000000001', 'tenant-1', 'request-1', 'contract', 'contract', 'c-1', 'user-1', 'FAILED', NOW(3), NOW(3))").Error; err != nil {
		t.Fatalf("status FAILED must be accepted after the constraint update: %v", err)
	}
}
