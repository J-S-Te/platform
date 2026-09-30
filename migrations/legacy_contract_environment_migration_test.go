package migrations_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestLegacyContractDevelopmentEnvironmentIsRetiredWithoutRewritingHistory(t *testing.T) {
	content, err := migrations.Files.ReadFile("000111_retire_legacy_contract_development_environment.sql")
	if err != nil {
		t.Fatalf("read legacy contract environment migration: %v", err)
	}
	sql := string(content)
	required := []string{
		"application.code = 'contract_management'",
		"environment.id = '01J00000000000000000000301'",
		"environment.environment = 'dev'",
		"target.id = '01J00000000000000000000302'",
		"environment.status = 'DISABLED'",
		"'$.hidden_from_onboarding'",
		"'legacy_packaging_seed'",
		"deployment.operation = 'MIGRATION_BACKFILL'",
		"DELETE target",
		"DELETE environment",
		"NOT EXISTS (SELECT 1 FROM platform_oauth_client",
		"NOT EXISTS (SELECT 1 FROM cfg_namespace",
		"NOT EXISTS (SELECT 1 FROM audit_event",
		"NOT EXISTS (SELECT 1 FROM keycloak_application_client_mapping",
		"NOT EXISTS (SELECT 1 FROM platform_public_transport_resource",
	}
	for _, fragment := range required {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("legacy contract environment migration is missing %q", fragment)
		}
	}
}

// This test writes only to an explicitly supplied, empty disposable database. It proves the
// upgrade path: a legacy environment with durable OAuth evidence is retained but made invisible
// and inactive, while a fresh installation without evidence is physically removed by the same
// migration (the latter is covered by the complete-chain integration gate).
func TestLegacyContractDevelopmentEnvironmentWithEvidenceIsSafelyRetiredOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_LEGACY_CONTRACT_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_LEGACY_CONTRACT_MIGRATION_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database = database.WithContext(ctx)

	var tables int64
	if err := database.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("legacy contract migration integration test requires an empty disposable database")
	}

	historical := fstest.MapFS{}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "000111_retire_legacy_contract_development_environment.sql" {
			continue
		}
		data, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		historical[entry.Name()] = &fstest.MapFile{Data: data}
	}
	if _, err := migration.Run(ctx, database, historical); err != nil {
		t.Fatalf("run historical migrations: %v", err)
	}

	const insertOAuthClient = `INSERT INTO platform_oauth_client (
		id, tenant_id, application_id, environment_id, client_id, client_name,
		client_type, token_auth_method, access_token_ttl_seconds, refresh_token_ttl_seconds,
		require_pkce, status, version, created_at, updated_at
	) VALUES (
		'01JTESTLEGACYOAUTHCLIENT01', '01J00000000000000000000000',
		'01J00000000000000000000300', '01J00000000000000000000301',
		'contract-management-dev-evidence', 'Legacy contract dev evidence',
		'confidential', 'client_secret_basic', 900, 3600, 1, 'ACTIVE', 1,
		UTC_TIMESTAMP(3), UTC_TIMESTAMP(3)
	)`
	if err := database.Exec(insertOAuthClient).Error; err != nil {
		t.Fatalf("seed durable OAuth evidence: %v", err)
	}
	if applied, err := migration.Run(ctx, database, migrations.Files); err != nil {
		t.Fatalf("apply retirement migration: %v", err)
	} else if len(applied) != 1 || applied[0].Version != 111 {
		t.Fatalf("applied migrations = %+v, want only version 111", applied)
	}

	var environment struct {
		Status   string
		Metadata string
	}
	if err := database.Raw(`SELECT status, CAST(metadata AS CHAR) AS metadata
		FROM platform_application_environment WHERE id = '01J00000000000000000000301'`).Scan(&environment).Error; err != nil {
		t.Fatal(err)
	}
	if environment.Status != "DISABLED" || !strings.Contains(environment.Metadata, `"hidden_from_onboarding": true`) {
		t.Fatalf("legacy environment was not safely retired: %+v", environment)
	}
	var targetStatus string
	if err := database.Raw(`SELECT status FROM platform_application_login_target
		WHERE id = '01J00000000000000000000302'`).Scan(&targetStatus).Error; err != nil {
		t.Fatal(err)
	}
	if targetStatus != "DISABLED" {
		t.Fatalf("legacy login target status = %q, want DISABLED", targetStatus)
	}
	var clientCount int64
	if err := database.Table("platform_oauth_client").Where("client_id = ?", "contract-management-dev-evidence").Count(&clientCount).Error; err != nil {
		t.Fatal(err)
	}
	if clientCount != 1 {
		t.Fatalf("durable OAuth evidence count = %d, want 1", clientCount)
	}
}
