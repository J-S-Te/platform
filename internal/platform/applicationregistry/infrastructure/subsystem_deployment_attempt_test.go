package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// This driver executes the production repository's SQL against a single locked
// row. It rejects unguarded recovery/claim SQL rather than emulating the service.
type attemptDriver struct {
	mu         sync.Mutex
	generation int64
	status     string
	started    time.Time
}
type attemptConn struct {
	store   *attemptDriver
	queries int
}
type attemptTx struct{ store *attemptDriver }

var attemptDriverID uint64

func (d *attemptDriver) Open(string) (driver.Conn, error) { return &attemptConn{store: d}, nil }
func (c *attemptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *attemptConn) Close() error { return nil }
func (c *attemptConn) Begin() (driver.Tx, error) {
	c.store.mu.Lock()
	c.queries = 0
	return &attemptTx{c.store}, nil
}
func (t *attemptTx) Commit() error   { t.store.mu.Unlock(); return nil }
func (t *attemptTx) Rollback() error { t.store.mu.Unlock(); return nil }
func (c *attemptConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.HasPrefix(q, "SELECT") {
		return nil, fmt.Errorf("unexpected query %s", q)
	}
	if c.queries == 0 && !strings.Contains(q, "FOR UPDATE") {
		return nil, errors.New("attempt read is not locked")
	}
	c.queries++
	return &deleteEnvRows{columns: []string{"tenant_id", "application_code", "environment_code", "status", "generation", "started_at"}, values: [][]driver.Value{{"tenant", "app", "prod", c.store.status, c.store.generation, c.store.started}}}, nil
}
func (c *attemptConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if !strings.HasPrefix(q, "UPDATE") {
		return nil, fmt.Errorf("unexpected exec %s", q)
	}
	parts := strings.Split(q, " WHERE ")
	if len(parts) != 2 {
		return nil, errors.New("unguarded update")
	}
	set := strings.Split(strings.Split(parts[0], " SET ")[1], ",")
	param := 0
	status := ""
	started := c.store.started
	increment := false
	for _, field := range set {
		if strings.Contains(field, "generation + 1") {
			increment = true
			continue
		}
		if strings.Contains(field, "attempt_count + 1") {
			continue
		}
		if !strings.Contains(field, "?") {
			continue
		}
		if strings.HasPrefix(field, "`status`") {
			status = args[param].Value.(string)
		}
		if strings.HasPrefix(field, "`started_at`") {
			started = args[param].Value.(time.Time)
		}
		param++
	}
	where := parts[1]
	if strings.Contains(where, "started_at < ?") {
		// Recovery SQL must preserve the enumerated attempt, status, and timestamp.
		if !strings.Contains(where, "generation = ? AND status = ? AND started_at = ?") {
			return nil, errors.New("recovery lacks snapshot predicates")
		}
		generation := args[param+3].Value.(int64)
		previous := args[param+4].Value.(string)
		expectedTime := args[param+5].Value.(time.Time)
		cutoff := args[param+6].Value.(time.Time)
		if c.store.generation != generation || c.store.status != previous || !c.store.started.Equal(expectedTime) || !c.store.started.Before(cutoff) {
			return driver.RowsAffected(0), nil
		}
	} else {
		previous := args[param+3].Value.(string)
		generation := args[param+4].Value.(int64)
		if c.store.status != previous || c.store.generation != generation {
			return driver.RowsAffected(0), nil
		}
	}
	if increment {
		c.store.generation++
	}
	c.store.status = status
	c.store.started = started
	return driver.RowsAffected(1), nil
}
func newAttemptRepository(t *testing.T) (*SubsystemOnboardingGORMRepository, *attemptDriver) {
	t.Helper()
	store := &attemptDriver{generation: 4, status: application.SubsystemDeploymentStatusReady, started: time.Now().UTC().Add(-time.Hour)}
	name := fmt.Sprintf("attempt-driver-%d", atomic.AddUint64(&attemptDriverID, 1))
	sql.Register(name, store)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	gdb, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return &SubsystemOnboardingGORMRepository{database: gdb}, store
}
func TestDeploymentClaimsAreExclusiveAndOldJobsCannotCompleteNewAttempt(t *testing.T) {
	repo, store := newAttemptRepository(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := repo.ClaimSubsystemDeployment(context.Background(), "tenant", "app", "prod", "UPDATE", time.Now().UTC())
			results <- err
		}()
	}
	close(start)
	successes := 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, application.ErrSubsystemDeploymentTransition) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("claims succeeded %d times", successes)
	}
	if err := repo.CompleteSubsystemDeployment(context.Background(), "tenant", "app", "prod", 4, application.SubsystemDeploymentStatusFailed, "UPDATE", "OLD", "old", time.Now()); !errors.Is(err, application.ErrSubsystemDeploymentTransition) {
		t.Fatalf("old completion = %v", err)
	}
	if store.status != application.SubsystemDeploymentStatusUpdating || store.generation != 5 {
		t.Fatalf("new attempt overwritten: %#v", store)
	}
}
func TestStaleDeploymentSnapshotCannotRecoverNewRetry(t *testing.T) {
	repo, store := newAttemptRepository(t)
	stale := store.started
	store.status = application.SubsystemDeploymentStatusUpdating
	snapshot := application.SubsystemDeploymentState{TenantID: "tenant", ApplicationCode: "app", Environment: "prod", Generation: 4, Status: store.status, StartedAt: &stale}
	now := time.Now().UTC()
	if err := repo.CompleteSubsystemDeployment(context.Background(), "tenant", "app", "prod", 4, application.SubsystemDeploymentStatusFailed, "UPDATE", "FAILED", "failed", now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimSubsystemDeployment(context.Background(), "tenant", "app", "prod", "RETRY", now); err != nil {
		t.Fatal(err)
	}
	recovered, err := repo.RecoverStaleSubsystemDeployment(context.Background(), snapshot, now.Add(-20*time.Minute), now)
	if err != nil || recovered {
		t.Fatalf("stale recovery = %v, %v", recovered, err)
	}
	if store.status != application.SubsystemDeploymentStatusUpdating || store.generation != 5 {
		t.Fatal("new retry was interrupted")
	}
	store.started = stale
	snapshot.Generation = 5
	recovered, err = repo.RecoverStaleSubsystemDeployment(context.Background(), snapshot, now.Add(-20*time.Minute), now)
	if err != nil || !recovered || store.status != application.SubsystemDeploymentStatusFailed {
		t.Fatalf("matching stale snapshot was not recovered: %v %v", recovered, err)
	}
}
