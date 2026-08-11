package infrastructure

import (
	"strings"
	"testing"
	"time"

	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
)

func TestNewProjectionSourceRejectsNilDependencies(t *testing.T) {
	if _, err := NewProjectionSource(nil, nil); err == nil {
		t.Fatal("NewProjectionSource(nil, nil) error = nil")
	}
}

func TestProjectionSourceReadsSynchronizedClientMappingInsteadOfProjectionRow(t *testing.T) {
	database := newDryRunMySQL(t)
	statement := keycloakClientMappingQuery(database, projectionworker.Event{TenantID: "tenant", ApplicationID: "application", EnvironmentID: "environment"}).Find(&[]projectionSourceRow{}).Statement
	sql := statement.SQL.String()
	for _, expected := range []string{"keycloak_application_client_mapping", "platform_application", "application_code", "environment_id", "keycloak_client_id", "status = ?", "ORDER BY environment_id ASC"} {
		if !strings.Contains(sql, expected) {
			t.Errorf("mapping query missing %q: %s", expected, sql)
		}
	}
	if strings.Contains(sql, "keycloak_authorization_projection") {
		t.Fatalf("source must not read projection output row: %s", sql)
	}
}

func TestProjectionUserQueryIncludesProfileAndDeletionState(t *testing.T) {
	database := newDryRunMySQL(t)
	statement := projectionUserQuery(database, projectionworker.Event{TenantID: "tenant", IdentityID: "identity"}).Find(&[]projectionUserRow{}).Statement
	sql := statement.SQL.String()
	for _, expected := range []string{"iam_user", "display_name", "email", "status", "deleted_at", "tenant_id = ?", "id = ?"} {
		if !strings.Contains(sql, expected) {
			t.Errorf("user query missing %q: %s", expected, sql)
		}
	}
	if !projectionUserEnabled(projectionUserRow{Status: "ACTIVE"}) {
		t.Error("active, undeleted platform user was not enabled")
	}
	deletedAt := time.Now()
	if projectionUserEnabled(projectionUserRow{Status: "ACTIVE", DeletedAt: &deletedAt}) || projectionUserEnabled(projectionUserRow{Status: "DISABLED"}) {
		t.Error("deleted or disabled platform user was enabled")
	}
}
