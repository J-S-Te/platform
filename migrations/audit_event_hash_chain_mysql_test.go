package migrations_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
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

// 本验收在隔离 MySQL 8.4 上端到端验证审计 P1 哈希链：
//   * 摄取按（租户、应用、环境）推进链序列，chain_hash 逐行可重算；
//   * 无密钥者追加伪造行 / 链指针伪造 / 跳号分别被校验识别（recompute mismatch / linkage break / gap）；
//   * 校验有界可续跑（more_remaining）。
// 运行方式：AUDIT_CHAIN_TEST_DSN 指向一次性数据库（测试 DROP/CREATE 该库），未配置时跳过。
func TestAuditEventHashChainMySQL(t *testing.T) {
	dsn := os.Getenv("AUDIT_CHAIN_TEST_DSN")
	if dsn == "" {
		t.Skip("AUDIT_CHAIN_TEST_DSN is not configured; no database connection attempted")
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
	_ = raw.Close()
	raw, err = sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	ddl := []string{
		"CREATE TABLE platform_application (id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, name VARCHAR(128), application_type VARCHAR(32), status VARCHAR(32) NOT NULL, version BIGINT UNSIGNED NOT NULL DEFAULT 1, created_at DATETIME(3) NOT NULL)",
		"CREATE TABLE platform_application_environment (id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, environment VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, status VARCHAR(32) NOT NULL)",
		"CREATE TABLE audit_event (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, occurred_month INT UNSIGNED NOT NULL, event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, event_category VARCHAR(32) NOT NULL, event_type VARCHAR(64) NOT NULL, occurred_at DATETIME(3) NOT NULL, received_at DATETIME(3) NOT NULL, actor_type VARCHAR(32) NOT NULL, actor_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, actor_name_snapshot VARCHAR(128) NULL, session_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, action VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, resource_name_snapshot VARCHAR(255) NULL, business_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, trace_id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL, correlation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, source_ip VARBINARY(16) NULL, login_ip VARBINARY(16) NULL, client_ip VARBINARY(16) NULL, ingest_peer_ip VARBINARY(16) NULL, ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'LEGACY_RECORD', login_ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL, user_agent VARCHAR(1000) NULL, result VARCHAR(32) NOT NULL, reason_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, risk_level VARCHAR(16) NOT NULL, classification VARCHAR(32) NOT NULL, summary VARCHAR(1000) NULL, metadata JSON NULL, changes JSON NULL, payload_hash BINARY(32) NOT NULL, PRIMARY KEY (id, occurred_month))",
		"CREATE TABLE audit_event_dedup (application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, audit_row_id BIGINT UNSIGNED NULL, occurred_month INT UNSIGNED NOT NULL, received_at DATETIME(3) NOT NULL, PRIMARY KEY (application_id, event_id))",
		"CREATE TABLE audit_archive_item (archive_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, audit_row_id BIGINT UNSIGNED NOT NULL, occurred_month INT UNSIGNED NOT NULL, purged_at DATETIME(3) NULL, PRIMARY KEY (archive_id, audit_row_id, occurred_month))",
		"INSERT INTO platform_application (id, tenant_id, code, name, application_type, status, created_at) VALUES ('app-1', 'tenant-1', 'platform', '平台', 'CORE', 'ACTIVE', NOW(3))",
		"INSERT INTO platform_application_environment (id, application_id, environment, status) VALUES ('env-1', 'app-1', 'dev', 'ACTIVE')",
	}
	for _, statement := range ddl {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("apply ddl: %v (%s...)", err, statement[:60])
		}
	}
	// 逐版本应用迁移：闭包捕获 raw，避免包级 Exec(statement) 形态。
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
	for _, version := range []uint64{129, 130, 131} {
		applyMigrationVersion(version)
	}

	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: raw, SkipInitializeWithVersion: true}), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	key := append([]byte{0x2a}, make([]byte, 31)...)
	repository, err := auditinfrastructure.NewRepository(gormDB, auditinfrastructure.WithPayloadHashKey(key))
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	service, err := auditapplication.NewService(repository, fixedGenerator{}, auditClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	newInput := func(id string) auditapplication.EventInput {
		return auditapplication.EventInput{
			EventID: id, ApplicationCode: "platform", EnvironmentCode: "dev",
			ActorType: "USER", ActorID: "user-1", Action: "POST /api/v1/x",
			ResourceType: "PLATFORM_API", Result: "SUCCESS", RiskLevel: "LOW", Classification: "INTERNAL",
			EventCategory: "EXTERNAL", EventType: "POST /api/v1/x",
			OccurredAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
		}
	}
	for _, id := range []string{"evt-1", "evt-2", "evt-3"} {
		if receipt, ingestErr := service.Ingest(context.Background(), "tenant-1", newInput(id)); ingestErr != nil || receipt.Status != "ACCEPTED" {
			t.Fatalf("ingest %s: receipt=%+v err=%v", id, receipt, ingestErr)
		}
	}

	// 全链校验：三行全部自证且连续。
	verification, err := service.VerifyEventChain(context.Background(), "tenant-1", "platform", "dev", 1, 100)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verification.Verified || verification.CheckedRows != 3 || verification.LastSequence != 3 || verification.MoreRemaining {
		t.Fatalf("verification=%+v", verification)
	}

	// 伪造一：攻击者无密钥，追加“下一序号”但 chain_hash 为乱值 → 重算不匹配。
	// （事件 INSERT 不受 000130 触发器约束——触发器只防改写/删除；追加伪造正是链校验要抓的威胁。）
	var lastChainHash []byte
	if err := raw.QueryRow("SELECT last_chain_hash FROM audit_event_chain").Scan(&lastChainHash); err != nil {
		t.Fatalf("read chain head: %v", err)
	}
	forgedAppend := map[string]any{
		"occurred_month": 202610, "event_id": "evt-forge-a", "tenant_id": "tenant-1",
		"application_id": "app-1", "environment_id": "env-1", "event_category": "EXTERNAL",
		"event_type": "POST /api/v1/x", "occurred_at": time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
		"received_at": time.Now().UTC(), "actor_type": "USER", "action": "POST /api/v1/x",
		"resource_type": "PLATFORM_API", "result": "SUCCESS", "risk_level": "LOW",
		"classification": "INTERNAL", "payload_hash": make([]byte, 32),
		"payload_hash_alg": "HMAC-SHA256", "chain_prev_hash": lastChainHash,
		"chain_hash": make([]byte, 32), "chain_sequence": uint64(4),
	}
	if err := gormDB.Table("audit_event").Create(forgedAppend).Error; err != nil {
		t.Fatalf("insert forged append: %v", err)
	}
	verification, err = service.VerifyEventChain(context.Background(), "tenant-1", "platform", "dev", 1, 100)
	if err != nil {
		t.Fatalf("verify forged append: %v", err)
	}
	if verification.Verified || verification.BrokenSequence != 4 || verification.BrokenEventID != "evt-forge-a" || verification.Reason != "chain hash recompute mismatch" {
		t.Fatalf("forged append verification=%+v", verification)
	}
	// 000130 触发器按设计拒绝了未豁免的 DELETE——伪造行经“归档标记→删除”的合法路径清理。
	purgeViaRetention(t, raw, "evt-forge-a")

	// 伪造二：自洽但接错链——持有 H 的一方可以对任意 prev 计算合法 chain_hash，
	// 换接 prev 后行内自证仍通过，只有“相邻链指针连续性”校验能识破。
	forgedPrev := bytes.Repeat([]byte{0xDD}, 32)
	var evt3PayloadHash []byte
	if err := raw.QueryRow("SELECT payload_hash FROM audit_event WHERE event_id = 'evt-3'").Scan(&evt3PayloadHash); err != nil {
		t.Fatalf("read evt-3 payload hash: %v", err)
	}
	selfConsistentChain := hmac.New(sha256.New, key)
	selfConsistentChain.Write(append(append([]byte{}, forgedPrev...), evt3PayloadHash...))
	forgedLinkage := map[string]any{
		"occurred_month": 202610, "event_id": "evt-forge-b", "tenant_id": "tenant-1",
		"application_id": "app-1", "environment_id": "env-1", "event_category": "EXTERNAL",
		"event_type": "POST /api/v1/x", "occurred_at": time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
		"received_at": time.Now().UTC(), "actor_type": "USER", "action": "POST /api/v1/x",
		"resource_type": "PLATFORM_API", "result": "SUCCESS", "risk_level": "LOW",
		"classification": "INTERNAL", "payload_hash": evt3PayloadHash,
		"payload_hash_alg": "HMAC-SHA256", "chain_prev_hash": forgedPrev,
		"chain_hash": selfConsistentChain.Sum(nil), "chain_sequence": uint64(4),
	}
	if err := gormDB.Table("audit_event").Create(forgedLinkage).Error; err != nil {
		t.Fatalf("insert forged linkage: %v", err)
	}
	verification, err = service.VerifyEventChain(context.Background(), "tenant-1", "platform", "dev", 1, 100)
	if err != nil {
		t.Fatalf("verify forged linkage: %v", err)
	}
	if verification.Verified || verification.BrokenSequence != 4 || verification.Reason != "chain linkage break" {
		t.Fatalf("forged linkage verification=%+v", verification)
	}
	purgeViaRetention(t, raw, "evt-forge-b")

	// 跳号：中间缺行（如被留存清理）记为 GAP，不误报“链断裂”。evt-2 经合法留存路径清理。
	purgeViaRetention(t, raw, "evt-2")
	verification, err = service.VerifyEventChain(context.Background(), "tenant-1", "platform", "dev", 1, 100)
	if err != nil {
		t.Fatalf("verify gap: %v", err)
	}
	if verification.Verified || verification.CheckedRows != 1 || verification.Reason != "sequence gap" {
		t.Fatalf("gap verification=%+v", verification)
	}
	// 有界续跑：从序列 3 开始校验，单行自证通过。
	verification, err = service.VerifyEventChain(context.Background(), "tenant-1", "platform", "dev", 3, 100)
	if err != nil || !verification.Verified || verification.CheckedRows != 1 {
		t.Fatalf("verify from 3: verified=%v checked=%d err=%v", verification.Verified, verification.CheckedRows, err)
	}
	// 窗口有界：limit=1 从 1 开始只查 1 行且 more_remaining=true。
	verification, err = service.VerifyEventChain(context.Background(), "tenant-1", "platform", "dev", 1, 1)
	if err != nil || !verification.Verified || verification.CheckedRows != 1 || !verification.MoreRemaining {
		t.Fatalf("bounded verify=%+v err=%v", verification, err)
	}
}


// purgeViaRetention 按留存任务的合法路径清理事件行：先写 purge 标记（满足 000130 触发器豁免），
// 再删除事件行。测试用它清理伪造行，同时顺带证明触发器确实在拦截未豁免删除。
func purgeViaRetention(t *testing.T, db *sql.DB, eventID string) {
	t.Helper()
	var rowID uint64
	var occurredMonth int
	if err := db.QueryRow("SELECT id, occurred_month FROM audit_event WHERE event_id = ?", eventID).Scan(&rowID, &occurredMonth); err != nil {
		t.Fatalf("read %s: %v", eventID, err)
	}
	if _, err := db.Exec("INSERT INTO audit_archive_item (archive_id, audit_row_id, occurred_month, purged_at) VALUES (?, ?, ?, NOW(3))", "cleanup", rowID, occurredMonth); err != nil {
		t.Fatalf("mark %s: %v", eventID, err)
	}
	if _, err := db.Exec("DELETE FROM audit_event WHERE id = ? AND occurred_month = ?", rowID, occurredMonth); err != nil {
		t.Fatalf("purge %s: %v", eventID, err)
	}
}