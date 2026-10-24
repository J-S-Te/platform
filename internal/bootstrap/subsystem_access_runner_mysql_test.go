package bootstrap

import (
	"context"
	"errors"
	applicationregistryinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 接线链集成测试：真实 MySQL + 无部署 Agent。
// 本地模式清单不含任何目标 → 条件检查确定性地判定 OUT_OF_SCOPE（不触外部系统），
// 因此可以在隔离库上验证"装配 → 领取 → 推进 → 落库"的完整链路。
func TestSubsystemAccessRunnerWiringOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	// 迁移文件含多条语句：显式开启 multiStatements，避免按分号切分带来的歧义。
	if !strings.Contains(dsn, "multiStatements=") {
		if strings.Contains(dsn, "?") {
			dsn += "&multiStatements=true"
		} else {
			dsn += "?multiStatements=true"
		}
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	// 容器内运行时通过环境变量指向挂载的迁移文件；本地直接读取仓库相对路径。
	migrationPath := strings.TrimSpace(os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_MIGRATION_FILE"))
	if migrationPath == "" {
		migrationPath = filepath.Join("..", "..", "migrations", "000122_subsystem_access_operations.sql")
	}
	migration, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	cleanup := []string{
		"DROP TABLE IF EXISTS subsystem_access_operation_stage",
		"DROP TABLE IF EXISTS subsystem_access_operation",
		"DROP TABLE IF EXISTS subsystem_deployment_state",
		"DROP TABLE IF EXISTS platform_application_environment",
		"DROP TABLE IF EXISTS platform_application",
	}
	for _, statement := range cleanup {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("clean disposable schema: %v", err)
		}
	}
	if err := db.Exec(string(migration)).Error; err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	supporting := []string{
		"CREATE TABLE platform_application (id CHAR(26) NOT NULL, tenant_id CHAR(26) NOT NULL, code VARCHAR(64) NOT NULL, status VARCHAR(16) NOT NULL, PRIMARY KEY (id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE platform_application_environment (id CHAR(26) NOT NULL, tenant_id CHAR(26) NOT NULL, application_id CHAR(26) NOT NULL, environment VARCHAR(16) NOT NULL, status VARCHAR(16) NOT NULL, PRIMARY KEY (id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		"CREATE TABLE subsystem_deployment_state (tenant_id CHAR(26) NOT NULL, application_id CHAR(26) NOT NULL, environment_id CHAR(26) NOT NULL, application_code VARCHAR(64) NOT NULL, environment_code VARCHAR(16) NOT NULL, status VARCHAR(32) NOT NULL, operation VARCHAR(32) NOT NULL, generation BIGINT UNSIGNED NOT NULL DEFAULT 1, attempt_count INT UNSIGNED NOT NULL DEFAULT 0, created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL, PRIMARY KEY (tenant_id, environment_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
	}
	for _, statement := range supporting {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare supporting schema: %v", err)
		}
	}

	cfg := config.Config{}
	cfg.SubsystemOnboarding.Enabled = true
	cfg.SubsystemOnboarding.Mode = "local"
	cfg.SubsystemOnboarding.SocketPath = filepath.Join(t.TempDir(), "agent.sock")
	cfg.SubsystemOnboarding.Timeout = 5 * time.Second
	cfg.Auth.OIDCIssuer = "https://sso.example.test/realms/basic"
	cfg.Worker.ID = "test-worker"
	cfg.Worker.PollInterval = time.Second

	loggerInstance := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner, err := newSubsystemAccessRunnerFromConfig(cfg, db, loggerInstance, applicationregistryinfrastructure.SubsystemAccessKeycloakBrokerReconciliation{}, nil)
	if err != nil {
		t.Fatalf("wire subsystem access runner: %v", err)
	}
	if runner == nil || runner.worker == nil {
		t.Fatal("runner wiring produced no worker")
	}

	// 空表：领取不到操作，且不报错。
	handled, err := runner.worker.DriveClaimable(context.Background(), 10)
	if err != nil || handled != 0 {
		t.Fatalf("empty drive: handled=%d err=%v", handled, err)
	}

	// 放入一条待处理操作：接线链应当领取并推进（本地清单无目标 → 条件检查 OUT_OF_SCOPE 失败）。
	now := time.Now().UTC()
	insert := "INSERT INTO subsystem_access_operation (id, tenant_id, application_code, environment_code, action, stage, status, preview_version, request_digest, public_origin, auth_provider, initial_admin_id, idempotency_key, operator_id, attempt_count, active_slot, version, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	if err := db.Exec(insert, "01SAOP00000000000000000001", "tenant-1", "settlement", "dev", "ONBOARD", "CHECK", "PENDING",
		"pv-1", strings.Repeat("a", 64), "", "platform", nil, "idem-wiring-1", "01USR00000000000000000001",
		0, "tenant-1:settlement:dev", 1, now, now).Error; err != nil {
		t.Fatalf("seed pending operation: %v", err)
	}
	handled, err = runner.worker.DriveClaimable(context.Background(), 10)
	if err != nil {
		t.Fatalf("drive seeded operation: %v", err)
	}
	if handled != 1 {
		t.Fatalf("handled = %d, want 1", handled)
	}

	const row_id = "01SAOP00000000000000000001"

	var row struct {
		Status       string
		AttemptCount uint32
		ErrorCode    string
		Stage        string
	}
	if err := db.Table("subsystem_access_operation").
		Select("status, attempt_count, error_code, stage").Where("id = ?", row_id).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 (operation was not claimed)", row.AttemptCount)
	}
	// 无部署 Agent：阶段结果属于"未知"，Worker 必须保留租约等待下一轮核对，而不是假装成功或失败。
	if row.Status == "PENDING" || row.Status == "SUCCEEDED" {
		t.Fatalf("operation = %+v, want a claimed state (RUNNING/WAITING/FAILED)", row)
	}
	if row.Stage != "CHECK" {
		t.Fatalf("stage = %s, want CHECK", row.Stage)
	}
	var stageCount int64
	if err := db.Table("subsystem_access_operation_stage").
		Where("tenant_id = ? AND operation_id = ?", "tenant-1", "01SAOP00000000000000000001").Count(&stageCount).Error; err != nil {
		t.Fatal(err)
	}
	if stageCount != 1 {
		t.Fatalf("stage rows = %d, want 1", stageCount)
	}
	var stageRow struct {
		Stage  string
		Status string
	}
	if err := db.Table("subsystem_access_operation_stage").
		Select("stage, status").Where("tenant_id = ? AND operation_id = ?", "tenant-1", row_id).Scan(&stageRow).Error; err != nil {
		t.Fatal(err)
	}
	if stageRow.Stage != "CHECK" {
		t.Fatalf("stage row = %+v, want CHECK", stageRow)
	}
	var activeSlot *string
	if err := db.Table("subsystem_access_operation").Select("active_slot").
		Where("id = ?", row_id).Scan(&activeSlot).Error; err != nil {
		t.Fatal(err)
	}
	if row.Status != "RUNNING" && activeSlot != nil && strings.TrimSpace(*activeSlot) != "" {
		t.Fatalf("terminal operation must release the active slot, got %q", *activeSlot)
	}
}

// 真实迁移的约束必须生效：动作/状态/阶段的 CHECK、阶段唯一键、操作到阶段的级联删除。
// 这里同样直接执行发布用迁移文件（不用测试自写 DDL），避免"测试 DDL 掩盖迁移缺陷"。
func TestSubsystemAccessMigrationConstraintsOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	if !strings.Contains(dsn, "multiStatements=") {
		if strings.Contains(dsn, "?") {
			dsn += "&multiStatements=true"
		} else {
			dsn += "?multiStatements=true"
		}
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	migrationPath := strings.TrimSpace(os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_MIGRATION_FILE"))
	if migrationPath == "" {
		migrationPath = filepath.Join("..", "..", "migrations", "000122_subsystem_access_operations.sql")
	}
	migration, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	for _, statement := range []string{
		"DROP TABLE IF EXISTS subsystem_access_operation_stage",
		"DROP TABLE IF EXISTS subsystem_access_operation",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("clean disposable schema: %v", err)
		}
	}
	if err := db.Exec(string(migration)).Error; err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	now := time.Now().UTC()
	insert := "INSERT INTO subsystem_access_operation (id, tenant_id, application_code, environment_code, action, stage, status, preview_version, request_digest, idempotency_key, operator_id, version, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	if err := db.Exec(insert, "01SAOP00000000000000000002", "tenant-1", "settlement", "dev", "BOGUS", "CHECK", "PENDING",
		"pv-1", strings.Repeat("a", 64), "idem-bogus-action", "01USR00000000000000000001", 1, now, now).Error; err == nil {
		t.Fatal("invalid action must be rejected by the CHECK constraint")
	}
	if err := db.Exec(insert, "01SAOP00000000000000000003", "tenant-1", "settlement", "dev", "ONBOARD", "CHECK", "BOGUS",
		"pv-1", strings.Repeat("a", 64), "idem-bogus-status", "01USR00000000000000000001", 1, now, now).Error; err == nil {
		t.Fatal("invalid status must be rejected by the CHECK constraint")
	}
	if err := db.Exec(insert, "01SAOP00000000000000000004", "tenant-1", "settlement", "dev", "ONBOARD", "CHECK", "PENDING",
		"pv-1", strings.Repeat("a", 64), "idem-valid", "01USR00000000000000000001", 1, now, now).Error; err != nil {
		t.Fatalf("valid operation must be accepted: %v", err)
	}

	stageInsert := "INSERT INTO subsystem_access_operation_stage (id, tenant_id, operation_id, stage, status, created_at, updated_at) VALUES (?,?,?,?,?,?,?)"
	if err := db.Exec(stageInsert, "01STG00000000000000000001", "tenant-1", "01SAOP00000000000000000004", "CHECK", "SUCCEEDED", now, now).Error; err != nil {
		t.Fatalf("insert stage: %v", err)
	}
	if err := db.Exec(stageInsert, "01STG00000000000000000002", "tenant-1", "01SAOP00000000000000000004", "CHECK", "SUCCEEDED", now, now).Error; err == nil {
		t.Fatal("duplicate (tenant, operation, stage) must be rejected by the unique key")
	}
	if err := db.Exec(stageInsert, "01STG00000000000000000003", "tenant-1", "01SAOP00000000000000000004", "BOGUS", "SUCCEEDED", now, now).Error; err == nil {
		t.Fatal("invalid stage name must be rejected by the CHECK constraint")
	}
	if err := db.Exec("DELETE FROM subsystem_access_operation WHERE id = ?", "01SAOP00000000000000000004").Error; err != nil {
		t.Fatalf("delete operation: %v", err)
	}
	var remaining int64
	if err := db.Table("subsystem_access_operation_stage").Where("operation_id = ?", "01SAOP00000000000000000004").Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("stage rows = %d after cascade delete, want 0", remaining)
	}
}

// 观察期门禁：只读查询既有切换生命周期仓储；观察期相关原因阻塞（等待），其它原因不阻塞。
func TestSubsystemAccessObservationGateMapping(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantAllowed bool
		wantReason  string
	}{
		{"无仓储（未配置切换）→ 放行", nil, true, ""},
	}
	for _, item := range cases {
		gate := subsystemAccessObservationGate{}
		allowed, reason, err := gate.CanProceedWithUpdate(context.Background(), "tenant-1", "settlement", "dev")
		if err != nil || allowed != item.wantAllowed || reason != item.wantReason {
			t.Fatalf("%s: allowed=%v reason=%s err=%v", item.name, allowed, reason, err)
		}
	}

	mapping := []struct {
		message     string
		wantAllowed bool
		wantReason  string
	}{
		// 只有「观察期已开始但未结束」阻塞；从未切换过（无记录）或首次切换不应阻塞普通更新。
		{"Keycloak observation window has not completed", false, "OBSERVATION_WINDOW_NOT_COMPLETED"},
		{"Keycloak observation has not started", true, "OBSERVATION_NOT_STARTED"},
		{"Keycloak observation already exists", true, "OBSERVATION_ALREADY_EXISTS"},
		{"database is down", true, "OBSERVATION_STATE_UNAVAILABLE"},
	}
	for _, item := range mapping {
		gate := subsystemAccessObservationGate{store: observationGateStoreStub{err: errors.New(item.message)}}
		allowed, reason, err := gate.CanProceedWithUpdate(context.Background(), "tenant-1", "settlement", "dev")
		if err != nil || allowed != item.wantAllowed || reason != item.wantReason {
			t.Fatalf("%q: allowed=%v reason=%s err=%v", item.message, allowed, reason, err)
		}
	}
}

type observationGateStoreStub struct{ err error }

func (stub observationGateStoreStub) CanKeycloakCutover(context.Context, string, string, string) error {
	return stub.err
}
