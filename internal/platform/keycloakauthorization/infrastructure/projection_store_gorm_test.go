package infrastructure

import (
	"context"
	"testing"
	"time"

	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
)

func TestNewProjectionStoreRejectsNilDatabase(t *testing.T) {
	if _, err := NewProjectionStore(nil); err == nil {
		t.Fatal("NewProjectionStore(nil) error = nil")
	}
}

func TestProjectionStorePersistsSynchronizationAndFailure(t *testing.T) {
	store, err := NewProjectionStore(newDryRunMySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	if err := store.MarkSynchronized(context.Background(), projectionapplication.Snapshot{TenantID: "tenant", IdentityID: "identity", ApplicationID: "app", EnvironmentID: "environment", ApplicationCode: "console", KeycloakClientID: "client", AuthorizationRevision: 7}, now); err != nil {
		t.Fatalf("MarkSynchronized() error = %v", err)
	}
	if err := store.MarkFailed(context.Background(), projectionworker.Event{TenantID: "tenant", IdentityID: "identity", ApplicationID: "app", EnvironmentID: "environment"}, "SYNC_FAILED", "unavailable", now); err != nil {
		t.Fatalf("MarkFailed() error = %v", err)
	}
}

func TestTrimProjectionError(t *testing.T) {
	if got := trimProjectionError("  error  "); got != "error" {
		t.Fatalf("trimProjectionError() = %q", got)
	}
}
