package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Exercise GORM's real SQL generation and scanning without a database/network.
// Query-only connector fails all preparation/transaction paths rather than
// providing a fake successful write capability.
type policyQuery struct {
	tenant       string
	rows         [][]driver.Value
	err          error
	called       int
	contextKey   any
	contextValue any
}
type policyConnector struct{ query *policyQuery }
type policyDriver struct{ query *policyQuery }
type policyConnection struct{ query *policyQuery }
type policyRows struct {
	rows  [][]driver.Value
	index int
}

func (c policyConnector) Connect(context.Context) (driver.Conn, error) {
	return policyConnection{c.query}, nil
}
func (c policyConnector) Driver() driver.Driver         { return policyDriver{c.query} }
func (d policyDriver) Open(string) (driver.Conn, error) { return policyConnection{d.query}, nil }
func (c policyConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected policy prepare")
}
func (c policyConnection) Close() error { return nil }
func (c policyConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected policy transaction")
}
func (c policyConnection) QueryContext(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
	c.query.called++
	if !strings.Contains(statement, "FROM `notification_setting`") || strings.Contains(statement, "notification_setting_rows") || !strings.Contains(statement, "WHERE tenant_id = ?") || len(arguments) != 2 || arguments[0].Value != c.query.tenant || arguments[1].Value != int64(1) {
		return nil, fmt.Errorf("unexpected tenant settings SELECT %q", statement)
	}
	if c.query.contextKey != nil && ctx.Value(c.query.contextKey) != c.query.contextValue {
		return nil, errors.New("lost caller context")
	}
	if c.query.err != nil {
		return nil, c.query.err
	}
	return &policyRows{rows: c.query.rows}, nil
}
func (*policyRows) Columns() []string { return []string{"inbox_enabled", "reminder_frequency"} }
func (*policyRows) Close() error      { return nil }
func (r *policyRows) Next(values []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(values, r.rows[r.index])
	r.index++
	return nil
}

func policyDatabase(t *testing.T, query *policyQuery) *gorm.DB {
	t.Helper()
	connection := sql.OpenDB(policyConnector{query})
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func TestInboxPolicyQueriesActualMigrationTableAndTenant(t *testing.T) {
	type contextTag struct{}
	query := &policyQuery{tenant: "owned-tenant", rows: [][]driver.Value{{true, "IMMEDIATE"}}, contextKey: contextTag{}, contextValue: "owned-context"}
	policy, err := NewInboxPolicy(policyDatabase(t, query))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	enabled, visibleAt, err := policy.DeliveryVisibility(context.WithValue(context.Background(), contextTag{}, "owned-context"), query.tenant, now)
	if err != nil || !enabled || !visibleAt.Equal(now) || query.called != 1 {
		t.Fatalf("policy result enabled=%v visible=%v error=%v queries=%d", enabled, visibleAt, err, query.called)
	}
}

func TestInboxPolicyMissingSettingsUsesConfirmedSettingsDefault(t *testing.T) {
	query := &policyQuery{tenant: "unsaved-tenant"}
	policy, err := NewInboxPolicy(policyDatabase(t, query))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	enabled, visibleAt, err := policy.DeliveryVisibility(context.Background(), query.tenant, now)
	if err != nil || !enabled || !visibleAt.Equal(now) || query.called != 1 {
		t.Fatal("missing settings must match inbox=true/IMMEDIATE settings service default")
	}
}

func TestInboxPolicyExplicitSettingsRemainAuthoritative(t *testing.T) {
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		inbox     bool
		frequency string
		enabled   bool
		visibleAt time.Time
	}{
		{"disabled", false, "IMMEDIATE", false, now},
		{"never", true, "NEVER", false, now},
		{"daily", true, "DAILY", true, time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)},
		{"weekly", true, "WEEKLY", true, time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)},
		{"immediate", true, "IMMEDIATE", true, now},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := &policyQuery{tenant: "owned-tenant", rows: [][]driver.Value{{test.inbox, test.frequency}}}
			policy, err := NewInboxPolicy(policyDatabase(t, query))
			if err != nil {
				t.Fatal(err)
			}
			enabled, visibleAt, err := policy.DeliveryVisibility(context.Background(), query.tenant, now)
			if err != nil || enabled != test.enabled || !visibleAt.Equal(test.visibleAt) {
				t.Fatalf("explicit policy changed: enabled=%v visible=%v err=%v", enabled, visibleAt, err)
			}
		})
	}
}

func TestInboxPolicyDatabaseFailureIsNotMissingSettingsDefault(t *testing.T) {
	failure := errors.New("database query failure")
	query := &policyQuery{tenant: "owned-tenant", err: failure}
	policy, err := NewInboxPolicy(policyDatabase(t, query))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	enabled, visibleAt, err := policy.DeliveryVisibility(context.Background(), query.tenant, now)
	if !errors.Is(err, failure) || enabled || !visibleAt.Equal(now) || query.called != 1 {
		t.Fatal("database error was hidden as a default or delivery success")
	}
}
