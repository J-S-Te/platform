package infrastructure

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestMembershipAuthorizationRevisionUpdateTargetsInheritedBindingApplications(t *testing.T) {
	sqlDatabase, err := sql.Open("mysql", "")
	if err != nil {
		t.Fatalf("open dry-run SQL database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })

	database, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      sqlDatabase,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run GORM database: %v", err)
	}

	now := time.Date(2026, time.July, 29, 8, 30, 0, 0, time.UTC)
	result := buildMembershipAuthorizationRevisionUpdate(database, "tenant-1", now, "membership changed")
	if result.Error != nil {
		t.Fatalf("build revision update: %v", result.Error)
	}

	statement := result.Statement.SQL.String()
	for _, fragment := range []string{
		"UPDATE `authz_policy_revision`",
		"`revision`=revision + 1",
		"tenant_id = ?",
		"FROM authz_role_binding AS inherited_binding",
		"inherited_binding.tenant_id = authz_policy_revision.tenant_id",
		"inherited_binding.application_id = authz_policy_revision.application_id",
		"inherited_binding.subject_type IN ('ORG_UNIT', 'POSITION')",
	} {
		if !strings.Contains(statement, fragment) {
			t.Errorf("revision update SQL is missing %q; SQL=%s", fragment, statement)
		}
	}

	if !containsStatementVariable(result.Statement.Vars, "tenant-1") {
		t.Fatalf("revision update variables do not contain tenant ID: %#v", result.Statement.Vars)
	}
	if !containsStatementVariable(result.Statement.Vars, "membership changed") {
		t.Fatalf("revision update variables do not contain change reason: %#v", result.Statement.Vars)
	}
	if !containsStatementVariable(result.Statement.Vars, now) {
		t.Fatalf("revision update variables do not contain changed time: %#v", result.Statement.Vars)
	}
}

func containsStatementVariable(values []any, expected any) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
