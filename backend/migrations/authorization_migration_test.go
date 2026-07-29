package migrations_test

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/migrations"
)

func TestAuthorizationBackfillMigrationIsIdempotentAndKeepsLegacyTablesReadOnly(t *testing.T) {
	migrationSQL, err := migrations.Files.ReadFile("000059_migrate_legacy_authorization.sql")
	if err != nil {
		t.Fatalf("read authorization backfill migration: %v", err)
	}

	sql := string(migrationSQL)
	for _, fragment := range []string{
		"authz_user_application_role",
		"authz_user_permission",
		"authz_role_binding",
		"authz_role_permission",
		"ON DUPLICATE KEY UPDATE id = id",
		"Legacy read-only compatibility table",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("authorization backfill migration is missing %q", fragment)
		}
	}
}

func TestAuthorizationCatalogMetadataMigrationDefinesApplicationScopedState(t *testing.T) {
	migrationSQL, err := migrations.Files.ReadFile("000058_create_authorization_catalog_metadata.sql")
	if err != nil {
		t.Fatalf("read authorization catalog migration: %v", err)
	}

	sql := string(migrationSQL)
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS authz_authorization_catalog",
		"tenant_id CHAR(26)",
		"application_id CHAR(26)",
		"catalog_version",
		"catalog_hash",
		"sync_status",
		"PRIMARY KEY (tenant_id, application_id)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("authorization catalog migration is missing %q", fragment)
		}
	}
}
