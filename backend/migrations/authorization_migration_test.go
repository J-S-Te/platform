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

func TestApplicationAuthorizationCatalogPolicyMigrationDefinesGenericRoleLimit(t *testing.T) {
	migrationSQL, err := migrations.Files.ReadFile("000062_create_application_authorization_catalog_policy.sql")
	if err != nil {
		t.Fatalf("read application authorization policy migration: %v", err)
	}

	sql := string(migrationSQL)
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS authz_application_authorization_policy",
		"tenant_id CHAR(26)",
		"application_id CHAR(26)",
		"max_effective_roles SMALLINT UNSIGNED NOT NULL DEFAULT 0",
		"source_type",
		"source_identifier",
		"catalog_version",
		"catalog_hash",
		"PRIMARY KEY (tenant_id, application_id)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("application authorization policy migration is missing %q", fragment)
		}
	}
}

func TestAuthorizationCatalogClaimsRoleConfigHashMigrationIsAdditive(t *testing.T) {
	migrationSQL, err := migrations.Files.ReadFile("000063_add_catalog_claims_role_config_hash.sql")
	if err != nil {
		t.Fatalf("read catalog claims role config hash migration: %v", err)
	}
	for _, fragment := range []string{
		"ALTER TABLE authz_authorization_catalog",
		"ADD COLUMN claims_role_config_hash",
		"VARCHAR(128)",
	} {
		if !strings.Contains(string(migrationSQL), fragment) {
			t.Fatalf("catalog claims role config hash migration is missing %q", fragment)
		}
	}
}
