package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type completionNativeScenario struct {
	committed       bool
	postCommitReads int
	queue           bool
	query           string
	args            []driver.NamedValue
	queryErr        error
}
type completionNativeDriver struct{ scenario *completionNativeScenario }
type completionNativeConn struct{ scenario *completionNativeScenario }
type completionNativeTx struct{ scenario *completionNativeScenario }

func (d completionNativeDriver) Open(string) (driver.Conn, error) {
	return &completionNativeConn{d.scenario}, nil
}
func (*completionNativeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*completionNativeConn) Close() error                { return nil }
func (c *completionNativeConn) Begin() (driver.Tx, error) { return completionNativeTx{c.scenario}, nil }
func (tx completionNativeTx) Commit() error               { tx.scenario.committed = true; return nil }
func (completionNativeTx) Rollback() error                { return nil }
func (*completionNativeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return membershipVersionResult(1), nil
}
func (c *completionNativeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.scenario
	if s.queue {
		s.query, s.args = query, args
		if s.queryErr != nil {
			return nil, s.queryErr
		}
		return &membershipVersionRows{columns: []string{"id", "tenant_id", "request_id", "user_id", "status"}, values: [][]driver.Value{{"batch-2", "tenant-1", "request-2", "leaver", "FROZEN"}}}, nil
	}
	if s.committed {
		s.postCommitReads++
		return nil, errors.New("connection lost after commit")
	}
	switch {
	case strings.Contains(query, "iam_personnel_change_request"):
		return &membershipVersionRows{columns: []string{"id", "tenant_id", "user_id", "change_type", "status", "version"}, values: [][]driver.Value{{"request-1", "tenant-1", "leaver", "TERMINATION", "SCHEDULED", int64(3)}}}, nil
	case strings.Contains(query, "iam_tenant"):
		return &membershipVersionRows{columns: []string{"code"}, values: [][]driver.Value{{"test-tenant"}}}, nil
	case strings.Contains(query, "iam_bootstrap_state"), strings.Contains(query, "iam_account"), strings.Contains(query, "keycloak_application_client_mapping"):
		return &membershipVersionRows{columns: []string{"id"}}, nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
}

var completionNativeCounter uint64

func completionNativeDB(t *testing.T, scenario *completionNativeScenario) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("personnel-completion-native-%d", atomic.AddUint64(&completionNativeCounter, 1))
	sql.Register(name, completionNativeDriver{scenario})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	g, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A failed read after COMMIT must never make an executed termination look rolled back.
func TestPersonnelTerminationCommitDoesNotDependOnFollowupRead(t *testing.T) {
	s := &completionNativeScenario{}
	repo := NewPersonnelChangeGORMRepository(completionNativeDB(t, s))
	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	result, err := repo.Execute(context.Background(), application.PersonnelChangeRequest{ID: "request-1", TenantID: "tenant-1", UserID: "leaver", Version: 3, Status: domain.PersonnelChangeScheduled}, "operator", now)
	if !s.committed {
		t.Fatalf("fixture did not reach commit: %v", err)
	}
	if err != nil || result.Status != domain.PersonnelChangeExecuted || result.Version != 4 || result.ExecutedAt == nil || !result.ExecutedAt.Equal(now) || !result.UpdatedAt.Equal(now) {
		t.Fatalf("committed result = %+v err=%v", result, err)
	}
	if s.postCommitReads != 0 {
		t.Fatal("successful execution depends on a fallible post-commit read")
	}
}

// Exercise the emitted SQL boundary with a native driver; no DSN or live database is used.
func TestPendingHandoverCompletionQueryIsBoundedAndTenantCorrelated(t *testing.T) {
	s := &completionNativeScenario{queue: true}
	repo, err := NewPersonnelHandoverGORMRepository(completionNativeDB(t, s))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListPendingCompletionBatches(context.Background(), "batch-1", 7)
	if err != nil || len(rows) != 1 || rows[0].TenantID != "tenant-1" || rows[0].ID != "batch-2" {
		t.Fatalf("queue result=%+v err=%v", rows, err)
	}
	for _, predicate := range []string{"r.tenant_id = b.tenant_id", "r.id = b.request_id", "r.user_id = b.user_id", "b.status IN", "b.cancel_requested_at IS NULL", "b.id >", "r.status =", "r.change_type =", "ORDER BY b.id ASC", "LIMIT"} {
		if !strings.Contains(s.query, predicate) {
			t.Errorf("missing queue boundary %q in %s", predicate, s.query)
		}
	}
	want := []any{"FROZEN", "TRANSFERRING", "batch-1", "EXECUTED", "TERMINATION", int64(7)}
	if len(s.args) != len(want) {
		t.Fatalf("query args=%v", s.args)
	}
	for i, v := range want {
		if s.args[i].Value != v {
			t.Errorf("arg %d=%v, want %v", i, s.args[i].Value, v)
		}
	}
	s.query = ""
	for _, limit := range []int{-1, 0, 101} {
		if _, err := repo.ListPendingCompletionBatches(context.Background(), "", limit); !errors.Is(err, application.ErrValidation) {
			t.Errorf("limit %d was accepted: %v", limit, err)
		}
	}
	if s.query != "" {
		t.Fatal("invalid bounds reached database")
	}
	s.queryErr = errors.New("queue database unavailable")
	if _, err := repo.ListPendingCompletionBatches(context.Background(), "", 1); !errors.Is(err, s.queryErr) {
		t.Fatal("query failure was hidden")
	}
}
