package migrations_test

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/migrations"
)

func TestExternalIdentityLoginAccountIntegrityMigrationBackfillsAndConstrainsAccount(t *testing.T) {
	migrationSQL, err := migrations.Files.ReadFile("000071_enforce_external_customer_login_account_integrity.sql")
	if err != nil {
		t.Fatalf("read external login account integrity migration: %v", err)
	}
	sql := string(migrationSQL)
	for _, fragment := range []string{
		"INSERT INTO iam_account",
		"NOT EXISTS (",
		"ADD COLUMN login_account_id",
		"uk_external_identity_login_account",
		"fk_external_identity_login_account",
		"REFERENCES iam_account (tenant_id, id)",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("external identity login-account integrity migration is missing %q", fragment)
		}
	}
	if strings.Contains(sql, "iam_password_credential") {
		t.Fatal("external identity repair must not initialize a password credential")
	}
}
