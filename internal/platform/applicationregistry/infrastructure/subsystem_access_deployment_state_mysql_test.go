package infrastructure

import (
	"context"
	"os"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 真实数据库验证就绪读取器：状态/代次/尝试次数来自 subsystem_deployment_state；不存在返回 found=false。
func TestSubsystemAccessDeploymentStateReaderOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	for _, statement := range []string{
		"DROP TABLE IF EXISTS subsystem_deployment_state",
		`CREATE TABLE subsystem_deployment_state (
			tenant_id CHAR(26) NOT NULL, application_id CHAR(26) NOT NULL, environment_id CHAR(26) NOT NULL,
			application_code VARCHAR(64) NOT NULL, environment_code VARCHAR(16) NOT NULL,
			status VARCHAR(32) NOT NULL, operation VARCHAR(32) NOT NULL, generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
			attempt_count INT UNSIGNED NOT NULL DEFAULT 0, created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
			PRIMARY KEY (tenant_id, environment_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	reader, err := NewSubsystemAccessDeploymentStateGORMReader(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Exec("INSERT INTO subsystem_deployment_state (tenant_id, application_id, environment_id, application_code, environment_code, status, operation, generation, attempt_count, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"tenant-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "READY", "ONBOARD", 3, 2, now, now).Error; err != nil {
		t.Fatal(err)
	}

	status, generation, attempts, found, err := reader.ReadSubsystemAccessDeploymentState(context.Background(), "tenant-1", "settlement", "DEV")
	if err != nil || !found || status != "READY" || generation != 3 || attempts != 2 {
		t.Fatalf("state = %s gen=%d attempts=%d found=%v err=%v", status, generation, attempts, found, err)
	}
	// 环境大小写归一化后仍能命中。
	if _, _, _, found, err := reader.ReadSubsystemAccessDeploymentState(context.Background(), "tenant-1", "settlement", "dev"); err != nil || !found {
		t.Fatalf("lowercase environment lookup failed: found=%v err=%v", found, err)
	}
	// 未登记目标：found=false，且不是错误。
	if _, _, _, found, err := reader.ReadSubsystemAccessDeploymentState(context.Background(), "tenant-1", "contract_management", "dev"); err != nil || found {
		t.Fatalf("unknown target must return found=false without error: found=%v err=%v", found, err)
	}
	// 跨租户不可见。
	if _, _, _, found, _ := reader.ReadSubsystemAccessDeploymentState(context.Background(), "tenant-2", "settlement", "dev"); found {
		t.Fatal("cross-tenant deployment state must not be visible")
	}
}

// 上下文读取器（InitialRuntimeProvisioning 派生前置）：清单漂移列的两态与未登记目标。
func TestSubsystemAccessDeploymentContextReaderOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	for _, statement := range []string{
		"DROP TABLE IF EXISTS subsystem_deployment_state",
		`CREATE TABLE subsystem_deployment_state (
			tenant_id CHAR(26) NOT NULL, application_id CHAR(26) NOT NULL, environment_id CHAR(26) NOT NULL,
			application_code VARCHAR(64) NOT NULL, environment_code VARCHAR(16) NOT NULL,
			status VARCHAR(32) NOT NULL, operation VARCHAR(32) NOT NULL, generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
			attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
			applied_manifest_checksum VARCHAR(128) NULL, manifest_last_applied_at DATETIME(3) NULL,
			created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
			PRIMARY KEY (tenant_id, environment_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	reader, err := NewSubsystemAccessDeploymentStateGORMReader(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Exec("INSERT INTO subsystem_deployment_state (tenant_id, application_id, environment_id, application_code, environment_code, status, operation, generation, attempt_count, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"tenant-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "FAILED", "UPDATE", 2, 1, now, now).Error; err != nil {
		t.Fatal(err)
	}

	// 尚未应用清单：Found=true、校验和为空、应用时间为 nil（派生会置真）。
	deploymentContext, err := reader.ReadSubsystemAccessDeploymentContext(context.Background(), "tenant-1", "settlement", "DEV")
	if err != nil || !deploymentContext.Found || deploymentContext.Status != "FAILED" ||
		deploymentContext.AppliedManifestChecksum != "" || deploymentContext.ManifestLastAppliedAt != nil {
		t.Fatalf("context = %+v err=%v", deploymentContext, err)
	}

	// 已应用清单：两个字段都必须读到（派生会置假）。
	appliedAt := now.Add(-time.Hour)
	if err := db.Exec("UPDATE subsystem_deployment_state SET applied_manifest_checksum = ?, manifest_last_applied_at = ? WHERE tenant_id = ? AND application_code = ?",
		"manifest-aaa", appliedAt, "tenant-1", "settlement").Error; err != nil {
		t.Fatal(err)
	}
	deploymentContext, err = reader.ReadSubsystemAccessDeploymentContext(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil || !deploymentContext.Found || deploymentContext.AppliedManifestChecksum != "manifest-aaa" || deploymentContext.ManifestLastAppliedAt == nil {
		t.Fatalf("applied context = %+v err=%v", deploymentContext, err)
	}

	// 未登记目标：Found=false，且不是错误。
	deploymentContext, err = reader.ReadSubsystemAccessDeploymentContext(context.Background(), "tenant-1", "contract_management", "dev")
	if err != nil || deploymentContext.Found {
		t.Fatalf("missing target context = %+v err=%v", deploymentContext, err)
	}
}
