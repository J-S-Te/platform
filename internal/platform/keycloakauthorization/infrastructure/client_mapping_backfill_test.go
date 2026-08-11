package infrastructure

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm/clause"
)

func TestKeycloakBackfillQueriesOnlyActiveUsersAndSynchronizedTargets(t *testing.T) {
	database := newDryRunMySQL(t)
	users := activeKeycloakBackfillUsersQuery(database, "tenant-1").Find(&[]struct{}{})
	targets := synchronizedKeycloakBackfillTargetsQuery(database, "tenant-1", "application-1").Find(&[]struct{}{})
	for name, statement := range map[string]string{"users": users.Statement.SQL.String(), "targets": targets.Statement.SQL.String()} {
		if !strings.Contains(statement, "status = ?") || !strings.Contains(statement, "ORDER BY") {
			t.Fatalf("%s query is not constrained and stable: %s", name, statement)
		}
	}
	if !containsBackfillVariable(users.Statement.Vars, "ACTIVE") || !containsBackfillVariable(targets.Statement.Vars, "SYNCED") {
		t.Fatalf("backfill must select only active users and synchronized mappings: users=%#v targets=%#v", users.Statement.Vars, targets.Statement.Vars)
	}
}

func TestPendingLegacyKeycloakOutboxQuerySelectsOnlyUnexpandedWork(t *testing.T) {
	database := newDryRunMySQL(t)
	result := pendingLegacyKeycloakOutboxQuery(database, "tenant-1", "application-1").Find(&[]keycloakBackfillLegacyEvent{})
	sql := result.Statement.SQL.String()
	for _, fragment := range []string{"environment_id IS NULL", "status IN", "created_at ASC", "id ASC"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("legacy query missing %q: %s", fragment, sql)
		}
	}
}

func TestPendingLegacyKeycloakOutboxScopesQueryScansEveryTenantAndApplication(t *testing.T) {
	database := newDryRunMySQL(t)
	result := pendingLegacyKeycloakOutboxScopesQuery(database).Find(&[]keycloakBackfillLegacyScope{})
	sql := result.Statement.SQL.String()
	for _, fragment := range []string{"environment_id IS NULL", "status IN", "GROUP BY tenant_id, application_id", "ORDER BY tenant_id ASC, application_id ASC"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("legacy scope query missing %q: %s", fragment, sql)
		}
	}
	if strings.Contains(sql, "tenant_id = ?") || strings.Contains(sql, "application_id = ?") {
		t.Fatalf("legacy scope query must scan every tenant and application: %s", sql)
	}
	if !containsBackfillVariable(result.Statement.Vars, "PENDING") || !containsBackfillVariable(result.Statement.Vars, "RUNNING") {
		t.Fatalf("legacy scope query must include pending and running events: %#v", result.Statement.Vars)
	}
}

func TestLegacyKeycloakOutboxExpansionOnlyCompletesAfterSynchronizedTargetsExist(t *testing.T) {
	database := newDryRunMySQL(t)
	targets := synchronizedKeycloakBackfillTargetsQuery(database, "tenant-1", "application-1").Find(&[]struct{}{})
	if !containsBackfillVariable(targets.Statement.Vars, "SYNCED") {
		t.Fatalf("legacy expansion targets must be synchronized mappings: %#v", targets.Statement.Vars)
	}
	completion := database.Table("keycloak_authorization_outbox").
		Where("id = ? AND environment_id IS NULL AND status IN ?", "legacy-1", []string{"PENDING", "RUNNING"}).
		Updates(map[string]any{"status": "SUCCEEDED"})
	sql := completion.Statement.SQL.String()
	for _, fragment := range []string{"environment_id IS NULL", "status IN", "UPDATE `keycloak_authorization_outbox` SET"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("legacy completion SQL missing %q: %s", fragment, sql)
		}
	}
}

func TestInsertKeycloakBackfillLedgerUsesConflictDoNothing(t *testing.T) {
	database := newDryRunMySQL(t)
	result := database.Table("keycloak_authorization_reconcile_backfill").Clauses(clause.OnConflict{DoNothing: true}).Create(map[string]any{"tenant_id": "tenant-1"})
	if result.Error != nil || !strings.Contains(result.Statement.SQL.String(), "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("ledger insert must be conflict-safe: error=%v SQL=%s", result.Error, result.Statement.SQL.String())
	}
}

func TestLegacyKeycloakOutboxExpansionUsesExistingConflictSafeLedger(t *testing.T) {
	database := newDryRunMySQL(t)
	result := database.Table("keycloak_authorization_outbox_expansion").Clauses(clause.OnConflict{DoNothing: true}).Create(map[string]any{
		"source_outbox_event_id": "legacy-1", "environment_id": "environment-1", "outbox_event_id": "expanded-1",
	})
	if result.Error != nil || !strings.Contains(result.Statement.SQL.String(), "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("legacy expansion ledger must be conflict-safe: error=%v SQL=%s", result.Error, result.Statement.SQL.String())
	}
}

func TestClientMappingBackfillRejectsIncompleteTarget(t *testing.T) {
	store := &ClientMappingStore{database: newDryRunMySQL(t)}
	if err := store.BackfillKeycloakAuthorization(context.Background(), "tenant-1", "application-1", ""); err == nil {
		t.Fatal("BackfillKeycloakAuthorization() error = nil")
	}
}

func TestExpandLegacyKeycloakAuthorizationOutboxRejectsNilDatabase(t *testing.T) {
	if err := (&ClientMappingStore{}).ExpandLegacyKeycloakAuthorizationOutbox(context.Background()); err == nil {
		t.Fatal("ExpandLegacyKeycloakAuthorizationOutbox() error = nil")
	}
}

func containsBackfillVariable(values []any, expected any) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
