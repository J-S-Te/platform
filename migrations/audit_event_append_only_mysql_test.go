package migrations_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
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

// 本验收在隔离 MySQL 8.4 上端到端验证审计 P0 防篡改改造：
//   * 摄取持久化 payload_hash_alg，配置密钥时为 HMAC-SHA256 且可用 VerifyPayloadHash 重算比对；
//   * 迁移 000130 的触发器拒绝 UPDATE 与未豁免 DELETE，留存标记（purged_at）后放行删除；
//   * 无密钥（非必开）仓储退回 SHA-256 并如实写入算法标签。
// 运行方式：AUDIT_APPEND_ONLY_TEST_DSN 指向一个“可整体重建”的一次性数据库
// （测试会 DROP/CREATE 该库），未配置时跳过。
func TestAuditEventAppendOnlyAndPayloadHashMySQL(t *testing.T) {
	dsn := os.Getenv("AUDIT_APPEND_ONLY_TEST_DSN")
	if dsn == "" {
		t.Skip("AUDIT_APPEND_ONLY_TEST_DSN is not configured; no database connection attempted")
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
		"CREATE TABLE audit_event (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, occurred_month INT UNSIGNED NOT NULL, event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, tenant_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, environment_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, source_instance VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, event_category VARCHAR(32) NOT NULL, event_type VARCHAR(64) NOT NULL, occurred_at DATETIME(3) NOT NULL, received_at DATETIME(3) NOT NULL, actor_type VARCHAR(32) NOT NULL, actor_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, actor_name_snapshot VARCHAR(128) NULL, session_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, client_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, action VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, resource_type VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, resource_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, resource_name_snapshot VARCHAR(255) NULL, business_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, request_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, trace_id CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL, source_ip VARBINARY(16) NULL, user_agent VARCHAR(1000) NULL, result VARCHAR(32) NOT NULL, reason_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, risk_level VARCHAR(16) NOT NULL, classification VARCHAR(32) NOT NULL, summary VARCHAR(1000) NULL, metadata JSON NULL, changes JSON NULL, payload_hash BINARY(32) NOT NULL, correlation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL, login_ip VARBINARY(16) NULL, client_ip VARBINARY(16) NULL, ingest_peer_ip VARBINARY(16) NULL, ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'LEGACY_RECORD', login_ip_source VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL, PRIMARY KEY (id, occurred_month))",
		"CREATE TABLE audit_archive_item (archive_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, audit_row_id BIGINT UNSIGNED NOT NULL, occurred_month INT UNSIGNED NOT NULL, purged_at DATETIME(3) NULL, PRIMARY KEY (archive_id, audit_row_id, occurred_month))",
		"CREATE TABLE audit_event_dedup (application_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, event_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL, audit_row_id BIGINT UNSIGNED NULL, occurred_month INT UNSIGNED NOT NULL, received_at DATETIME(3) NOT NULL, PRIMARY KEY (application_id, event_id))",
		"INSERT INTO platform_application (id, tenant_id, code, name, application_type, status, created_at) VALUES ('app-1', 'tenant-1', 'platform', '平台', 'CORE', 'ACTIVE', NOW(3))",
		"INSERT INTO platform_application_environment (id, application_id, environment, status) VALUES ('env-1', 'app-1', 'dev', 'ACTIVE')",
	}
	for _, statement := range ddl {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("apply ddl: %v (%s...)", err, statement[:60])
		}
	}
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
		}
	}
	applyMigrationVersion(129)
	applyMigrationVersion(130)

	gormDB, err := gorm.Open(mysql.New(mysql.Config{Conn: raw, SkipInitializeWithVersion: true}), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}

	key := bytes.Repeat([]byte{0x2a}, 32)
	repository, err := auditinfrastructure.NewRepository(gormDB, auditinfrastructure.WithPayloadHashKey(key))
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	service, err := auditapplication.NewService(repository, fixedGenerator{}, auditClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	input := auditapplication.EventInput{
		EventID: "evt-hmac-1", ApplicationCode: "platform", EnvironmentCode: "dev",
		ActorType: "USER", ActorID: "user-1", Action: "POST /api/v1/dictionaries",
		ResourceType: "PLATFORM_API", Result: "SUCCESS", RiskLevel: "LOW", Classification: "INTERNAL",
		EventCategory: "EXTERNAL", EventType: "POST /api/v1/dictionaries",
		OccurredAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	}
	receipt, err := service.Ingest(context.Background(), "tenant-1", input)
	if err != nil || receipt.Status != "ACCEPTED" {
		t.Fatalf("ingest receipt=%+v err=%v", receipt, err)
	}

	var storedAlg string
	var storedHash []byte
	if err := raw.QueryRow("SELECT payload_hash, payload_hash_alg FROM audit_event WHERE event_id = ?", "evt-hmac-1").Scan(&storedHash, &storedAlg); err != nil {
		t.Fatalf("read stored hash: %v", err)
	}
	if storedAlg != auditinfrastructure.PayloadHashAlgorithmHMACSHA256 {
		t.Fatalf("stored algorithm = %q, want HMAC-SHA256", storedAlg)
	}
	// 摘要契约：存储摘要 = HMAC-SHA256(密钥, 规范化后 EventInput 的 JSON)。
	// 本 fixture 字段值已规范，规范化后的 JSON 与原始输入逐字节一致；
	// 随后用一条带首尾空白的输入证明摘要发生在规范化之后（规范化前的摘要不匹配）。
	canonical, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	unnormalized, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)
	if !hmac.Equal(storedHash, mac.Sum(nil)) {
		t.Fatalf("stored digest = %x, want HMAC-SHA256(key, normalized canonical payload)", storedHash)
	}
	if !bytes.Equal(canonical, unnormalized) {
		t.Fatal("canonical payload must equal the raw input marshaling for normalized fixtures")
	}

	spaced := input
	spaced.EventID = "evt-hmac-2"
	spaced.Action = "  POST /api/v1/dictionaries  "
	if receipt, err = service.Ingest(context.Background(), "tenant-1", spaced); err != nil || receipt.Status != "ACCEPTED" {
		t.Fatalf("spaced ingest receipt=%+v err=%v", receipt, err)
	}
	var stored2 []byte
	if err := raw.QueryRow("SELECT payload_hash FROM audit_event WHERE event_id = ?", "evt-hmac-2").Scan(&stored2); err != nil {
		t.Fatalf("read spaced hash: %v", err)
	}
	trimmed := spaced
	trimmed.Action = strings.TrimSpace(trimmed.Action)
	trimmedCanonical, err := json.Marshal(trimmed)
	if err != nil {
		t.Fatalf("marshal trimmed: %v", err)
	}
	trimmedMac := hmac.New(sha256.New, key)
	trimmedMac.Write(trimmedCanonical)
	if !hmac.Equal(stored2, trimmedMac.Sum(nil)) {
		t.Fatal("stored digest must equal HMAC over the normalized input")
	}
	spacedMac := hmac.New(sha256.New, key)
	spacedCanonical, err := json.Marshal(spaced)
	if err != nil {
		t.Fatalf("marshal spaced: %v", err)
	}
	spacedMac.Write(spacedCanonical)
	if hmac.Equal(stored2, spacedMac.Sum(nil)) {
		t.Fatal("digest must be computed after normalization, not over raw input")
	}

	verification, err := service.VerifyPayloadHash(context.Background(), "tenant-1", "evt-hmac-1", input)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verification.Matched || verification.Algorithm != auditinfrastructure.PayloadHashAlgorithmHMACSHA256 {
		t.Fatalf("verification=%+v", verification)
	}
	tampered := input
	tampered.Action = "DELETE /api/v1/users/1"
	if verification, err = service.VerifyPayloadHash(context.Background(), "tenant-1", "evt-hmac-1", tampered); err != nil || verification.Matched {
		t.Fatalf("tampered payload must not match: matched=%v err=%v", verification.Matched, err)
	}
	otherKey := bytes.Repeat([]byte{0x2b}, 32)
	otherRepo, err := auditinfrastructure.NewRepository(gormDB, auditinfrastructure.WithPayloadHashKey(otherKey))
	if err != nil {
		t.Fatalf("new other-key repository: %v", err)
	}
	otherService, err := auditapplication.NewService(otherRepo, fixedGenerator{}, auditClock{})
	if err != nil {
		t.Fatalf("new other-key service: %v", err)
	}
	if verification, err = otherService.VerifyPayloadHash(context.Background(), "tenant-1", "evt-hmac-1", input); err != nil || verification.Matched {
		t.Fatalf("a different key must not match: matched=%v err=%v", verification.Matched, err)
	}

	// 只追加约束：UPDATE 无条件拒绝；未豁免 DELETE 拒绝；留存标记后放行删除。
	if _, err := raw.Exec("UPDATE audit_event SET result = 'FAILURE' WHERE event_id = ?", "evt-hmac-1"); err == nil {
		t.Fatal("UPDATE on audit_event must be rejected by trigger")
	}
	if _, err := raw.Exec("DELETE FROM audit_event WHERE event_id = ?", "evt-hmac-1"); err == nil {
		t.Fatal("DELETE on audit_event without purge mark must be rejected by trigger")
	}
	var rowID uint64
	var occurredMonth int
	if err := raw.QueryRow("SELECT id, occurred_month FROM audit_event WHERE event_id = ?", "evt-hmac-1").Scan(&rowID, &occurredMonth); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if _, err := raw.Exec("INSERT INTO audit_archive_item (archive_id, audit_row_id, occurred_month, purged_at) VALUES ('arch-1', ?, ?, NOW(3))", rowID, occurredMonth); err != nil {
		t.Fatalf("seed purge mark: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM audit_event WHERE id = ? AND occurred_month = ?", rowID, occurredMonth); err != nil {
		t.Fatalf("marked DELETE must be allowed: %v", err)
	}

	// 无密钥仓储（非必开）退回 SHA-256 并写入算法标签。
	t.Setenv("PLATFORM_AUDIT_HASH_KEY", "")
	plainRepository, err := auditinfrastructure.NewRepository(gormDB)
	if err != nil {
		t.Fatalf("new plain repository: %v", err)
	}
	plainService, err := auditapplication.NewService(plainRepository, fixedGenerator{}, auditClock{})
	if err != nil {
		t.Fatalf("new plain service: %v", err)
	}
	plainInput := input
	plainInput.EventID = "evt-sha-1"
	if receipt, err = plainService.Ingest(context.Background(), "tenant-1", plainInput); err != nil || receipt.Status != "ACCEPTED" {
		t.Fatalf("plain ingest receipt=%+v err=%v", receipt, err)
	}
	var plainAlg string
	if err := raw.QueryRow("SELECT payload_hash_alg FROM audit_event WHERE event_id = ?", "evt-sha-1").Scan(&plainAlg); err != nil {
		t.Fatalf("read plain algorithm: %v", err)
	}
	if plainAlg != auditinfrastructure.PayloadHashAlgorithmSHA256 {
		t.Fatalf("plain algorithm = %q, want SHA-256", plainAlg)
	}
	if _, err := plainService.VerifyPayloadHash(context.Background(), "tenant-1", "evt-sha-1", plainInput); err != nil {
		t.Fatalf("plain verify: %v", err)
	}
}

type fixedGenerator struct{}

func (fixedGenerator) New(time.Time) (string, error) { return "generated", nil }

type auditClock struct{}

func (auditClock) Now() time.Time { return time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC) }
