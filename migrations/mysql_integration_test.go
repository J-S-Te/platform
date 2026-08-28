package migrations_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/tenantclone"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestMigrations95To99OnMySQL 在真实 MySQL 上执行完整迁移链，并验证 000095—000099 的关键结构。
// 测试默认跳过；CI 或发布前检查必须显式提供 PLATFORM_MIGRATION_TEST_DSN 指向一次性数据库。
func TestMigrations95To99OnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_MIGRATION_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err := migration.Run(ctx, database, migrations.Files); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	// 第二次运行必须为空，证明所有迁移的登记和 checksum 判断可安全重试。
	if applied, err := migration.Run(ctx, database, migrations.Files); err != nil {
		t.Fatalf("rerun migrations: %v", err)
	} else if len(applied) != 0 {
		t.Fatalf("rerun applied %d migrations, want 0", len(applied))
	}

	assertColumns(t, database, "subsystem_deployment_state", "desired_manifest_checksum", "applied_manifest_checksum", "manifest_drift_status", "manifest_last_applied_at", "manifest_last_verified_at")
	assertColumns(t, database, "file_version", "sha256")
	assertColumns(t, database, "authz_authorization_catalog", "previous_catalog_version", "previous_catalog_hash", "previous_claims_role_config_hash")
	assertColumns(t, database, "async_job", "parent_job_id", "application_idempotency_scope", "idempotency_key", "request_hash", "request_id", "trace_id", "correlation_id", "business_ref", "last_attempt_at", "retry_count", "last_succeeded_at")

	var versions int64
	if err := database.Raw("SELECT COUNT(*) FROM platform_schema_migration WHERE version BETWEEN 95 AND 99").Scan(&versions).Error; err != nil {
		t.Fatalf("query migration versions: %v", err)
	}
	if versions != 5 {
		t.Fatalf("recorded migrations 95-99 = %d, want 5", versions)
	}
	var permissionCount int64
	if err := database.Table("authz_permission").Where("code = ?", "platform:file:bind").Count(&permissionCount).Error; err != nil {
		t.Fatalf("query file bind permission: %v", err)
	}
	if permissionCount != 1 {
		t.Fatalf("platform:file:bind permission count = %d, want 1", permissionCount)
	}

	const targetTenantID = "01JTESTTENANT0000000000000"
	if err := database.Exec(`INSERT INTO iam_tenant (id,code,name,timezone,locale,status,version,created_at,updated_at) VALUES (?,?,?,'Asia/Shanghai','zh-CN','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, targetTenantID, "migration-clone-target", "迁移克隆测试租户").Error; err != nil {
		t.Fatalf("create clone target tenant: %v", err)
	}
	cloneService := &tenantclone.Service{DB: database}
	cloned, err := cloneService.Clone(ctx, tenantclone.Input{SourceTenantID: "01J00000000000000000000000", TargetTenantID: targetTenantID, IdempotencyKey: "mysql-integration-clone"})
	if err != nil {
		t.Fatalf("clone tenant authorization catalog: %v", err)
	}
	if cloned.Status != "COMPLETED" || cloned.Applications == 0 || cloned.Resources == 0 || cloned.Permissions == 0 || cloned.Roles == 0 || cloned.RolePermissions == 0 {
		t.Fatalf("clone result is incomplete: %+v", cloned)
	}
	replayed, err := cloneService.Clone(ctx, tenantclone.Input{SourceTenantID: "01J00000000000000000000000", TargetTenantID: targetTenantID, IdempotencyKey: "mysql-integration-clone"})
	if err != nil {
		t.Fatalf("replay tenant authorization clone: %v", err)
	}
	if replayed.OperationID != cloned.OperationID || replayed.Applications != cloned.Applications || replayed.RolePermissions != cloned.RolePermissions {
		t.Fatalf("clone replay changed result: first=%+v replay=%+v", cloned, replayed)
	}
}

func assertColumns(t *testing.T, database *gorm.DB, table string, columns ...string) {
	t.Helper()
	for _, column := range columns {
		var count int64
		if err := database.Raw(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`, table, column).Scan(&count).Error; err != nil {
			t.Fatalf("query %s.%s: %v", table, column, err)
		}
		if count != 1 {
			t.Fatalf("column %s.%s count = %d, want 1", table, column, count)
		}
	}
}
