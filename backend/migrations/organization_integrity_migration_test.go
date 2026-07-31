package migrations_test

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/migrations"
)

func TestOrganizationTenantIntegrityMigrationsUseCompositeForeignKeys(t *testing.T) {
	baseSQL := readMigration(t, "000055_enforce_organization_tenant_integrity.sql")
	for _, fragment := range []string{
		"ADD UNIQUE KEY uq_org_tenant_id (tenant_id, id)",
		"FOREIGN KEY (tenant_id, parent_id)",
		"REFERENCES iam_org_unit (tenant_id, id)",
		"ADD UNIQUE KEY uq_user_tenant_id (tenant_id, id)",
		"FOREIGN KEY (tenant_id, primary_org_id)",
		"ADD UNIQUE KEY uq_position_tenant_org_id (tenant_id, org_unit_id, id)",
		"FOREIGN KEY (tenant_id, org_unit_id, position_id)",
		"REFERENCES iam_position (tenant_id, org_unit_id, id)",
		"FOREIGN KEY (tenant_id, user_id)",
		"FOREIGN KEY (tenant_id, org_unit_id)",
	} {
		if !strings.Contains(baseSQL, fragment) {
			t.Fatalf("base organization integrity migration is missing %q", fragment)
		}
	}

	assertAppearsBefore(t, baseSQL,
		"ADD UNIQUE KEY uq_org_tenant_id (tenant_id, id)",
		"ADD CONSTRAINT fk_org_parent_tenant",
	)
	assertAppearsBefore(t, baseSQL,
		"ADD UNIQUE KEY uq_position_tenant_org_id (tenant_id, org_unit_id, id)",
		"ADD CONSTRAINT fk_membership_position_org_tenant",
	)

	positionSQL := readMigration(t, "000061_create_position_authorization_templates.sql")
	if !strings.Contains(positionSQL, "ADD UNIQUE KEY uq_position_tenant_id (tenant_id, id)") {
		t.Fatal("position migration must expose a tenant-safe (tenant_id, id) candidate key")
	}

	completionSQL := readMigration(t, "000065_complete_organization_tenant_integrity.sql")
	for _, fragment := range []string{
		"ADD UNIQUE KEY uq_membership_tenant_id (tenant_id, id)",
		"ADD CONSTRAINT fk_user_manager_tenant",
		"FOREIGN KEY (tenant_id, manager_user_id)",
		"ADD CONSTRAINT fk_org_leader_tenant",
		"FOREIGN KEY (tenant_id, leader_user_id)",
		"ADD UNIQUE KEY uq_account_tenant_id (tenant_id, id)",
		"ADD CONSTRAINT fk_account_user_tenant",
		"FOREIGN KEY (tenant_id, user_id)",
		"ADD CONSTRAINT fk_session_account_tenant",
		"FOREIGN KEY (tenant_id, account_id)",
		"REFERENCES iam_account (tenant_id, id)",
	} {
		if !strings.Contains(completionSQL, fragment) {
			t.Fatalf("organization integrity completion migration is missing %q", fragment)
		}
	}
	for _, rollbackFragment := range []string{
		"This project uses forward-only migrations",
		"DROP FOREIGN KEY fk_session_account_tenant",
		"DROP FOREIGN KEY fk_account_user_tenant",
		"DROP FOREIGN KEY fk_org_leader_tenant",
		"DROP FOREIGN KEY fk_user_manager_tenant",
	} {
		if !strings.Contains(completionSQL, rollbackFragment) {
			t.Fatalf("organization integrity completion migration is missing rollback guidance %q", rollbackFragment)
		}
	}
	if strings.Contains(strings.ToUpper(completionSQL), "CREATE TRIGGER") {
		t.Fatal("organization integrity must use declarative composite keys, not triggers")
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	migrationSQL, err := migrations.Files.ReadFile(name)
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	return string(migrationSQL)
}

func assertAppearsBefore(t *testing.T, text, first, second string) {
	t.Helper()
	firstIndex := strings.Index(text, first)
	secondIndex := strings.Index(text, second)
	if firstIndex < 0 || secondIndex < 0 {
		t.Fatalf("cannot compare migration ordering: first=%q second=%q", first, second)
	}
	if firstIndex >= secondIndex {
		t.Fatalf("migration fragment %q must appear before %q", first, second)
	}
}
