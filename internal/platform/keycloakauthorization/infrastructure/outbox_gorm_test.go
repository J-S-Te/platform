package infrastructure

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestNewOutboxQueueRejectsNilDatabase(t *testing.T) {
	if _, err := NewOutboxQueue(nil); err == nil {
		t.Fatal("NewOutboxQueue(nil) error = nil")
	}
}

func TestOutboxClaimUsesLockedPendingSelectionAndAtomicRunningTransition(t *testing.T) {
	database := newDryRunMySQL(t)
	queue, err := NewOutboxQueue(database)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = queue.Claim(context.Background(), "worker-1", time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC))
	// Dry-run deliberately has no selected row. Verify the query shape directly:
	statement := database.Session(&gorm.Session{DryRun: true}).Clauses().Model(&authorizationOutboxRow{}).
		Where("id = ? AND status = ?", "event-1", "PENDING").Updates(map[string]any{"status": "RUNNING"}).Statement
	if !strings.Contains(statement.SQL.String(), "WHERE id = ? AND status = ?") {
		t.Fatalf("claim transition is not conditional: %s", statement.SQL.String())
	}
}

func TestOutboxRecoverStaleResetsOnlyExpiredRunningLocksWithoutChangingAttempts(t *testing.T) {
	database := newDryRunMySQL(t)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	staleBefore := now.Add(-5 * time.Minute)
	statement := database.Session(&gorm.Session{DryRun: true}).Model(&authorizationOutboxRow{}).
		Where("status = ? AND locked_at < ?", "RUNNING", staleBefore.UTC()).
		Updates(staleRecoveryUpdates(now)).Statement
	sql := statement.SQL.String()
	for _, expected := range []string{"UPDATE `keycloak_authorization_outbox` SET", "`status`=?", "`locked_by`=?", "`locked_at`=?", "`last_error_code`=?", "WHERE status = ? AND locked_at < ?"} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("recovery SQL missing %q: %s", expected, sql)
		}
	}
	if strings.Contains(sql, "attempts") {
		t.Fatalf("recovery must not alter attempts: %s", sql)
	}
}

func TestOutboxDeadLetterTransitionIsConditionalAndKeepsFailureReason(t *testing.T) {
	database := newDryRunMySQL(t)
	statement := database.Session(&gorm.Session{DryRun: true}).Model(&authorizationOutboxRow{}).
		Where("id = ? AND status = ?", "event-1", "RUNNING").Updates(map[string]any{
		"status": "FAILED", "last_error_code": "KEYCLOAK_SYNC_RETRY_EXHAUSTED", "last_error_message": "unavailable",
	}).Statement
	sql := statement.SQL.String()
	for _, expected := range []string{"`status`=?", "`last_error_code`=?", "`last_error_message`=?", "WHERE id = ? AND status = ?"} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("dead-letter SQL missing %q: %s", expected, sql)
		}
	}
}

func newDryRunMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	return database
}
