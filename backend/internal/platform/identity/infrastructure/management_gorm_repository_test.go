package infrastructure

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestApplyManagementAuthorizationScopeRestrictsOrganizationAndResourceIDs(t *testing.T) {
	database := newDryRunManagementDatabase(t)
	query := application.PageRequest{
		ScopeRestricted:    true,
		AllowedOrgUnitIDs:  []string{"org-1", "org-2"},
		AllowedResourceIDs: []string{"membership-1"},
	}

	result := applyManagementAuthorizationScope(
		database.Table("iam_membership AS m"),
		query,
		"m.id",
		"m.org_unit_id",
	).Find(&[]membershipProjection{})
	if result.Error != nil {
		t.Fatalf("build scoped management query: %v", result.Error)
	}
	statement := result.Statement.SQL.String()
	for _, fragment := range []string{"m.org_unit_id IN", "m.id IN", " OR "} {
		if !strings.Contains(statement, fragment) {
			t.Fatalf("scoped management SQL is missing %q; SQL=%s", fragment, statement)
		}
	}
}

func TestApplyManagementAuthorizationScopeFailsClosedWhenNoIDsAreAllowed(t *testing.T) {
	database := newDryRunManagementDatabase(t)
	result := applyManagementAuthorizationScope(
		database.Table("iam_position"),
		application.PageRequest{ScopeRestricted: true},
		"id",
		"org_unit_id",
	).Find(&[]positionModel{})
	if result.Error != nil {
		t.Fatalf("build empty scoped management query: %v", result.Error)
	}
	if statement := result.Statement.SQL.String(); !strings.Contains(statement, "1 = 0") {
		t.Fatalf("empty scoped query must fail closed; SQL=%s", statement)
	}
}

func TestMembershipAuthorizationRevisionUpdateTargetsInheritedBindingApplications(t *testing.T) {
	database := newDryRunManagementDatabase(t)

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

func TestDisablePositionAuthorizationArtifactsClosesAssignmentsAndBindings(t *testing.T) {
	database := newDryRunManagementDatabase(t)
	positionIDs := database.Table("iam_position").Select("id").Where("tenant_id = ? AND id = ?", "tenant-1", "position-1")
	now := time.Date(2026, time.August, 1, 9, 0, 0, 0, time.UTC)

	assignmentResult := buildDisablePositionAuthorizationTemplateAssignments(database.Session(&gorm.Session{}), "tenant-1", positionIDs, "operator-1", now)
	for _, fragment := range []string{
		"UPDATE `authz_position_grant_template_assignment`",
		"position_id IN",
		"status <> ?",
		"`status`=?",
		"`version`=version + 1",
	} {
		if !strings.Contains(assignmentResult.Statement.SQL.String(), fragment) {
			t.Errorf("disable position template assignment SQL is missing %q; SQL=%s", fragment, assignmentResult.Statement.SQL.String())
		}
	}

	bindingResult := buildDisablePositionRoleBindings(database.Session(&gorm.Session{}), "tenant-1", positionIDs, "operator-1", now)
	statement := bindingResult.Statement.SQL.String()
	for _, fragment := range []string{
		"UPDATE `authz_role_binding`",
		"subject_type = ?",
		"subject_id IN",
		"status <> ?",
		"`status`=?",
		"`version`=version + 1",
	} {
		if !strings.Contains(statement, fragment) {
			t.Errorf("disable position authorization SQL is missing %q; SQL=%s", fragment, statement)
		}
	}
	for _, value := range []any{"tenant-1", "POSITION", "DISABLED", "operator-1", now} {
		if !containsStatementVariable(bindingResult.Statement.Vars, value) {
			t.Errorf("disable position authorization variables do not contain %#v: %#v", value, bindingResult.Statement.Vars)
		}
	}
}

func TestDisableOrganizationRoleBindingsOnlyClosesActiveOrganizationBindings(t *testing.T) {
	database := newDryRunManagementDatabase(t)
	organizationIDs := database.Table("iam_org_unit").Select("id").
		Where("tenant_id = ? AND path LIKE ?", "tenant-1", "/root/%")
	now := time.Date(2026, time.August, 1, 10, 0, 0, 0, time.UTC)

	result := buildDisableOrganizationRoleBindings(
		database.Session(&gorm.Session{}),
		"tenant-1",
		organizationIDs,
		"operator-1",
		now,
	)
	if result.Error != nil {
		t.Fatalf("build organization role binding cleanup: %v", result.Error)
	}

	statement := result.Statement.SQL.String()
	for _, fragment := range []string{
		"UPDATE `authz_role_binding`",
		"subject_type = ?",
		"subject_id IN",
		"status = ?",
		"`status`=?",
		"`updated_at`=?",
		"`updated_by`=?",
		"`version`=version + 1",
		"FROM `iam_org_unit`",
		"path LIKE ?",
	} {
		if !strings.Contains(statement, fragment) {
			t.Errorf("disable organization authorization SQL is missing %q; SQL=%s", fragment, statement)
		}
	}
	for _, value := range []any{"tenant-1", "ORG_UNIT", "ACTIVE", "DISABLED", "operator-1", "/root/%", now} {
		if !containsStatementVariable(result.Statement.Vars, value) {
			t.Errorf("disable organization authorization variables do not contain %#v: %#v", value, result.Statement.Vars)
		}
	}
	for _, forbidden := range []string{"subject_type IN", "POSITION", "USER", "grant_origin"} {
		if strings.Contains(statement, forbidden) {
			t.Errorf("organization cleanup must not broaden to %q; SQL=%s", forbidden, statement)
		}
	}
}

func newDryRunManagementDatabase(t *testing.T) *gorm.DB {
	t.Helper()
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
	return database
}

func containsStatementVariable(values []any, expected any) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
