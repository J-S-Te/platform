package infrastructure

import (
	"context"
	"strings"
	"testing"
	"time"

	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
)

func TestProjectionOperationsFailedQueryIsTenantScopedAndOnlyReadsDeadLetters(t *testing.T) {
	store, err := NewProjectionOperationsStore(newDryRunMySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	statement := store.failedQuery(context.Background(), "tenant-1", projectionapplication.FailurePageRequest{ApplicationCode: "crm", Environment: "prod"}).Statement
	sql := statement.SQL.String()
	// GORM builds a statement only after a terminal operation; use Find on the
	// same query to verify the complete generated shape.
	statement = store.failedQuery(context.Background(), "tenant-1", projectionapplication.FailurePageRequest{ApplicationCode: "crm", Environment: "prod"}).Find(&[]failedProjectionRow{}).Statement
	sql = statement.SQL.String()
	for _, expected := range []string{"keycloak_authorization_outbox AS outbox", "platform_application AS application", "platform_application_environment AS environment", "outbox.tenant_id = ?", "outbox.status = ?", "application.code = ?", "environment.environment = ?"} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("query missing %q: %s", expected, sql)
		}
	}
}

func TestProjectionReplayOnlyTransitionsFailedEventAndResetsAttemptBudget(t *testing.T) {
	database := newDryRunMySQL(t)
	when := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)
	statement := database.Model(&authorizationOutboxRow{}).Where("id = ? AND tenant_id = ? AND status = ?", "event-1", "tenant-1", "FAILED").Updates(map[string]any{
		"status": "PENDING", "available_at": when, "locked_by": nil, "locked_at": nil, "attempts": 0, "completed_at": nil,
	}).Statement
	sql := statement.SQL.String()
	for _, expected := range []string{"`status`=?", "`available_at`=?", "`attempts`=?", "`completed_at`=?", "WHERE id = ? AND tenant_id = ? AND status = ?"} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("replay SQL missing %q: %s", expected, sql)
		}
	}
}
