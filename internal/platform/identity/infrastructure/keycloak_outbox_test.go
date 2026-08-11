package infrastructure

import (
	"reflect"
	"strings"
	"testing"
)

func TestKeycloakProjectionTargetsQueryUsesSynchronizedApplicationMappings(t *testing.T) {
	database := newDryRunManagementDatabase(t)
	result := keycloakProjectionTargetsQuery(database, "tenant-1").
		Find(&[]keycloakProjectionTarget{})
	if result.Error != nil {
		t.Fatalf("build Keycloak projection target query: %v", result.Error)
	}
	statement := result.Statement.SQL.String()
	for _, fragment := range []string{
		"FROM `keycloak_application_client_mapping`",
		"application_id, environment_id",
		"tenant_id = ?",
		"status = ?",
	} {
		if !strings.Contains(statement, fragment) {
			t.Errorf("target query is missing %q; SQL=%s", fragment, statement)
		}
	}
	for _, value := range []any{"tenant-1", "SYNCED"} {
		if !containsStatementVariable(result.Statement.Vars, value) {
			t.Fatalf("target query must use %#v; variables=%#v", value, result.Statement.Vars)
		}
	}
}

func TestUniqueIdentityIDsDropsBlanksDuplicatesAndStabilizesOrder(t *testing.T) {
	got := uniqueIdentityIDs([]string{" identity-2 ", "", "identity-1", "identity-2", "  "})
	want := []string{"identity-1", "identity-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uniqueIdentityIDs() = %#v, want %#v", got, want)
	}
}
