package migrations_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	auditapplication "github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	auditinfrastructure "github.com/J-S-Te/Basic-Platform/internal/platform/audit/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 本验收在隔离 MySQL 8.4 上端到端验证迁移 000135（审计去重键补环境维度）：
//   - 历史去重行一行不丢：能通过 audit_row_id 解析环境的行补 environment_id，
//     事件已被留存清理/行缺失的历史行保留在空环境，不再阻塞任何真实环境的摄取；
//   - 同一应用不同环境的同名 event_id 各自被接受（此前后到环境被判 DUPLICATE 丢弃），
//     同环境内重试仍返回 DUPLICATE；
//   - 迁移语句可整体重跑：DDL 中途失败后用同一脚本重试不会因列/主键已存在而失败。
//
// 运行方式：AUDIT_DEDUP_TEST_DSN 指向一个“可整体重建”的一次性数据库
// （测试会 DROP/CREATE 该库），未配置时跳过。
// TestAuditEventDedupEnvironmentMySQL 验证旧去重行迁移、环境隔离及重试幂等。
// 参数：t为显式一次性MySQL测试上下文，未提供隔离DSN时不连接数据库。
// 返回值：无；丢行、错误去重、迁移或重复执行失败时报告失败。
func TestAuditEventDedupEnvironmentMySQL(t *testing.T) {
	dsn := os.Getenv("AUDIT_DEDUP_TEST_DSN")
	if dsn == "" {
		t.Skip("AUDIT_DEDUP_TEST_DSN is not configured; no database connection attempted")
	}
	parsed, err := gomysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if parsed.DBName == "" {
		t.Fatal("DSN must name a database (it will be dropped and recreated)")
	}
	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec("DROP DATABASE IF EXISTS `" + parsed.DBName + "`"); err != nil {
		t.Fatalf("drop database: %v", err)
	}
	if _, err := raw.Exec("CREATE DATABASE `" + parsed.DBName + "`"); err != nil {
		t.Fatalf("create database: %v", err)
	}
	// 连接池里的旧连接默认库已被 DROP，重建后重开连接池，让 DSN 的默认库生效。
	_ = raw.Close()
	raw, err = sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	ddl := []string{
		"CREATE TABLE platform_application (id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, name VARCHAR(128), application_type VARCHAR(32), status VARCHAR(32) NOT NULL, version BIGINT UNSIGNED NOT NULL DEFAULT 1, created_at DATETIME(3) NOT NULL)",
		"CREATE TABLE platform_application_environment (id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, environment VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, status VARCHAR(32) NOT NULL)",
		"CREATE TABLE audit_event (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, occurred_month INT UNSIGNED NOT NULL, event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, event_category VARCHAR(32) NOT NULL, event_type VARCHAR(64) NOT NULL, occurred_at DATETIME(3) NOT NULL, received_at DATETIME(3) NOT NULL, actor_type VARCHAR(32) NOT NULL, actor_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, actor_name_snapshot VARCHAR(128) NULL, session_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, action VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, resource_name_snapshot VARCHAR(255) NULL, business_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, trace_id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL, correlation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, source_ip VARBINARY(16) NULL, login_ip VARBINARY(16) NULL, client_ip VARBINARY(16) NULL, ingest_peer_ip VARBINARY(16) NULL, ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'LEGACY_RECORD', login_ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL, user_agent VARCHAR(1000) NULL, result VARCHAR(32) NOT NULL, reason_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, risk_level VARCHAR(16) NOT NULL, classification VARCHAR(32) NOT NULL, summary VARCHAR(1000) NULL, metadata JSON NULL, changes JSON NULL, payload_hash BINARY(32) NOT NULL, PRIMARY KEY (id, occurred_month))",
		"CREATE TABLE audit_archive_item (archive_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, audit_row_id BIGINT UNSIGNED NOT NULL, occurred_month INT UNSIGNED NOT NULL, purged_at DATETIME(3) NULL, PRIMARY KEY (archive_id, audit_row_id, occurred_month))",
		// 旧形态去重表：主键只有（应用、事件），正是 RUNTIME-08 的根因。
		"CREATE TABLE audit_event_dedup (application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, audit_row_id BIGINT UNSIGNED NULL, occurred_month INT UNSIGNED NOT NULL, received_at DATETIME(3) NOT NULL, PRIMARY KEY (application_id, event_id), KEY idx_audit_dedup_row (audit_row_id))",
		"INSERT INTO platform_application (id, tenant_id, code, name, application_type, status, created_at) VALUES ('app-1', 'tenant-1', 'platform', '平台', 'CORE', 'ACTIVE', NOW(3))",
		"INSERT INTO platform_application_environment (id, application_id, environment, status) VALUES ('env-1', 'app-1', 'dev', 'ACTIVE')",
		"INSERT INTO platform_application_environment (id, application_id, environment, status) VALUES ('env-2', 'app-1', 'prod', 'ACTIVE')",
	}
	for _, statement := range ddl {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("apply ddl: %v (%s...)", err, statement[:60])
		}
	}

	// 逐版本应用迁移；闭包捕获 raw，避免包级 Exec(statement) 形态。
	applyMigrationVersion := func(version uint64) {
		t.Helper()
		items, err := migration.Load(migrations.Files)
		if err != nil {
			t.Fatalf("load migrations: %v", err)
		}
		for _, item := range items {
			if item.Version != version {
				continue
			}
			statements, err := migration.SplitStatements(item.SQL)
			if err != nil {
				t.Fatalf("split migration %06d: %v", version, err)
			}
			for index, statement := range statements {
				if _, err := raw.Exec(statement); err != nil {
					t.Fatalf("apply migration %06d statement %d: %v", version, index+1, err)
				}
			}
			return
		}
		t.Fatalf("migration %06d not found in embedded set", version)
	}
	// 129/130/131 是当前审计表的既有形态（payload_hash_alg、只追加触发器、哈希链）；
	// 135 之前的历史去重行必须在旧形态下才能写入。
	for _, version := range []uint64{129, 130, 131} {
		applyMigrationVersion(version)
	}

	insertLegacyEvent := func(eventID, environmentID string) uint64 {
		t.Helper()
		result, err := raw.Exec(
			"INSERT INTO audit_event (occurred_month, event_id, tenant_id, application_id, environment_id, event_category, event_type, occurred_at, received_at, actor_type, action, resource_type, result, risk_level, classification, payload_hash) VALUES (202610, ?, 'tenant-1', 'app-1', ?, 'EXTERNAL', 'POST /api/v1/x', ?, ?, 'USER', 'POST /api/v1/x', 'PLATFORM_API', 'SUCCESS', 'LOW', 'INTERNAL', UNHEX(REPEAT('01', 32)))",
			eventID, environmentID, time.Now().UTC(), time.Now().UTC(),
		)
		if err != nil {
			t.Fatalf("insert legacy event %s/%s: %v", eventID, environmentID, err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("legacy event id: %v", err)
		}
		return uint64(id)
	}
	crossEnvironmentRow := insertLegacyEvent("evt-shared", "env-1")
	insertLegacyEvent("evt-shared", "env-2")
	insertLegacyDedup := func(eventID string, auditRowID *uint64) {
		t.Helper()
		var err error
		if auditRowID == nil {
			_, err = raw.Exec("INSERT INTO audit_event_dedup (application_id, event_id, audit_row_id, occurred_month, received_at) VALUES ('app-1', ?, NULL, 202610, NOW(3))", eventID)
		} else {
			_, err = raw.Exec("INSERT INTO audit_event_dedup (application_id, event_id, audit_row_id, occurred_month, received_at) VALUES ('app-1', ?, ?, 202610, NOW(3))", eventID, *auditRowID)
		}
		if err != nil {
			t.Fatalf("insert legacy dedup %s: %v", eventID, err)
		}
	}
	// 三类历史行：可解析环境、指向已被留存清理的事件、无关联事件行。
	insertLegacyDedup("evt-shared", &crossEnvironmentRow)
	orphanRow := uint64(987654)
	insertLegacyDedup("evt-purged", &orphanRow)
	insertLegacyDedup("evt-unlinked", nil)

	const migrationVersion = 135
	applyMigrationVersion(migrationVersion)

	// 断言一：历史去重行一行不丢，可解析的补齐环境，不可解析的保留在空环境。
	type dedupRow struct {
		EventID       string
		EnvironmentID string
	}
	rows, err := raw.Query("SELECT event_id, environment_id FROM audit_event_dedup WHERE application_id = 'app-1' ORDER BY event_id")
	if err != nil {
		t.Fatalf("read migrated dedup rows: %v", err)
	}
	got := map[string]string{}
	for rows.Next() {
		var row dedupRow
		if err := rows.Scan(&row.EventID, &row.EnvironmentID); err != nil {
			t.Fatalf("scan migrated dedup row: %v", err)
		}
		got[row.EventID] = row.EnvironmentID
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrated dedup rows: %v", err)
	}
	_ = rows.Close()
	if len(got) != 3 {
		t.Fatalf("migrated dedup rows = %v, want all 3 legacy rows preserved", got)
	}
	if got["evt-shared"] != "env-1" {
		t.Fatalf("resolvable legacy dedup row environment = %q, want env-1", got["evt-shared"])
	}
	for _, eventID := range []string{"evt-purged", "evt-unlinked"} {
		if got[eventID] != "" {
			t.Fatalf("unresolvable legacy dedup row %s environment = %q, want \"\" (kept, not dropped)", eventID, got[eventID])
		}
	}

	// 断言二：新主键含环境维度——不同环境可持有同名 event_id，同环境内仍唯一。
	if _, err := raw.Exec("INSERT INTO audit_event_dedup (application_id, environment_id, event_id, occurred_month, received_at) VALUES ('app-1', 'env-2', 'evt-shared', 202610, NOW(3))"); err != nil {
		t.Fatalf("insert same event_id for another environment: %v", err)
	}
	if _, err := raw.Exec("INSERT INTO audit_event_dedup (application_id, environment_id, event_id, occurred_month, received_at) VALUES ('app-1', 'env-1', 'evt-shared', 202610, NOW(3))"); err == nil {
		t.Fatal("duplicate (application, environment, event_id) was accepted")
	}

	// 断言三：迁移脚本可整体重跑（DDL/回填都带守卫），不因列或主键已存在而失败。
	applyMigrationVersion(migrationVersion)
	var count int
	if err := raw.QueryRow("SELECT COUNT(*) FROM audit_event_dedup WHERE application_id = 'app-1'").Scan(&count); err != nil {
		t.Fatalf("count after re-run: %v", err)
	}
	if count != 4 {
		t.Fatalf("dedup rows after re-run = %d, want 4 (no rows lost)", count)
	}

	// 断言四：摄取路径端到端——跨环境同名 event_id 均被接受，同环境重试仍为 DUPLICATE。
	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: raw, SkipInitializeWithVersion: true}), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	repository, err := auditinfrastructure.NewRepository(gormDB)
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	service, err := auditapplication.NewService(repository, fixedGenerator{}, auditClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	eventInput := func(environmentCode string) auditapplication.EventInput {
		return auditapplication.EventInput{
			EventID: "evt-cross-environment", ApplicationCode: "platform", EnvironmentCode: environmentCode,
			ActorType: "USER", ActorID: "user-1", Action: "POST /api/v1/x",
			ResourceType: "PLATFORM_API", Result: "SUCCESS", RiskLevel: "LOW", Classification: "INTERNAL",
			EventCategory: "EXTERNAL", EventType: "POST /api/v1/x",
			OccurredAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
		}
	}
	for _, environmentCode := range []string{"dev", "prod"} {
		receipt, ingestErr := service.Ingest(context.Background(), "tenant-1", eventInput(environmentCode))
		if ingestErr != nil || receipt.Status != "ACCEPTED" {
			t.Fatalf("ingest %s: receipt=%+v err=%v", environmentCode, receipt, ingestErr)
		}
	}
	receipt, err := service.Ingest(context.Background(), "tenant-1", eventInput("dev"))
	if err != nil || receipt.Status != "DUPLICATE" {
		t.Fatalf("same-environment retry: receipt=%+v err=%v", receipt, err)
	}
	var accepted int
	if err := raw.QueryRow("SELECT COUNT(*) FROM audit_event WHERE event_id = 'evt-cross-environment'").Scan(&accepted); err != nil {
		t.Fatalf("count cross-environment events: %v", err)
	}
	if accepted != 2 {
		t.Fatalf("cross-environment persisted events = %d, want 2", accepted)
	}
}
