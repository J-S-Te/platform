package migrations_test

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/migrations"
)

func TestKeycloakReadinessConfigurationBindingMigrationFailsClosed(t *testing.T) {
	migrationSQL, err := migrations.Files.ReadFile("000090_bind_keycloak_readiness_to_client_configuration.sql")
	if err != nil {
		t.Fatalf("read Keycloak readiness migration: %v", err)
	}
	sql := string(migrationSQL)
	for _, required := range []string{
		"ADD COLUMN configuration_hash",
		"ADD COLUMN client_configuration_hash",
		"ADD COLUMN broker_verified_configuration_hash",
		"user_projection_completed = FALSE",
		"broker_login_verified = FALSE",
		"DELETE FROM keycloak_authorization_reconcile_backfill",
		"UPDATE keycloak_authorization_projection",
		"status = 'PENDING'",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration missing %q", required)
		}
	}
}
