package positiongrant

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

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type materializationWrite struct {
	query string
	args  []driver.NamedValue
}
type materializationFixture struct {
	rows                  []bindingProjection
	writes                []materializationWrite
	active                bool
	fail                  string
	committed, rolledBack bool
}
type materializationDriver struct{ f *materializationFixture }
type materializationConn struct{ f *materializationFixture }
type materializationTx struct{ f *materializationFixture }

var materializationCounter uint64

func (d materializationDriver) Open(string) (driver.Conn, error) {
	return &materializationConn{d.f}, nil
}
func (c *materializationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("no prepare")
}
func (c *materializationConn) Close() error              { return nil }
func (c *materializationConn) Begin() (driver.Tx, error) { return &materializationTx{c.f}, nil }
func (c *materializationTx) Commit() error               { c.f.committed = true; return nil }
func (c *materializationTx) Rollback() error             { c.f.rolledBack = true; return nil }
func (c *materializationConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.f.writes = append(c.f.writes, materializationWrite{q, append([]driver.NamedValue(nil), args...)})
	if c.f.fail != "" && strings.HasPrefix(q, c.f.fail) {
		return nil, errors.New("fixture dependent write failure")
	}
	return materializationResult(1), nil
}
func (c *materializationConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "authz_position_grant_template_assignment"):
		status := activeStatus
		if !c.f.active {
			status = disabledStatus
		}
		return regressionRows("id,tenant_id,position_id,template_id,status", []driver.Value{"a", "t", "p", "template", status}), nil
	case strings.Contains(q, "authz_position_grant_template_role"):
		return regressionRows("id,application_id,role_id,scope_type,scope_id,status", []driver.Value{"item", "app", "role", scopeTenant, "", activeStatus}), nil
	case strings.Contains(q, "authz_position_grant_template"):
		return regressionRows("id,status", []driver.Value{"template", activeStatus}), nil
	case strings.Contains(q, "authz_role_binding"):
		var values [][]driver.Value
		for _, r := range c.f.rows {
			if strings.Contains(q, "subject_type=?") && (r.SubjectType != subjectPosition || r.SubjectID != "p") {
				continue
			}
			values = append(values, []driver.Value{r.ID, r.ApplicationID, r.RoleID, r.SubjectType, r.SubjectID, r.ScopeType, r.ScopeID, r.Status, r.OriginItemID, int64(r.Version)})
		}
		return regressionRows("id,application_id,role_id,subject_type,subject_id,scope_type,scope_id,status,origin_item_id,version", values...), nil
	case strings.Contains(q, "keycloak_application_client_mapping"):
		return regressionRows("environment_id", []driver.Value{"prod"}), nil
	case strings.Contains(q, "iam_account"), strings.Contains(q, "iam_membership"):
		return regressionRows("user_id", []driver.Value{"former-user"}), nil
	default:
		return nil, fmt.Errorf("unexpected materialization SQL: %s", q)
	}
}

type materializationIDs struct{ n int }

func (g *materializationIDs) New(time.Time) (string, error) {
	g.n++
	return fmt.Sprintf("generated-%d", g.n), nil
}
func materializationDB(t *testing.T, f *materializationFixture) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("materialization-%d", atomic.AddUint64(&materializationCounter, 1))
	sql.Register(name, materializationDriver{f})
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: raw, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return db
}
func insertedValue(w materializationWrite, column string) any {
	start := strings.Index(w.query, "(")
	end := strings.Index(w.query, ") VALUES")
	if start < 0 || end < start {
		return nil
	}
	cols := strings.Split(w.query[start+1:end], ",")
	for i, col := range cols {
		if strings.Trim(col, " `") == column {
			return w.args[i].Value
		}
	}
	return nil
}
func TestMaterializationDisablesDriftAndNotifiesFormerSubjects(t *testing.T) {
	canonical := bindingProjection{ID: "correct", ApplicationID: "app", RoleID: "role", SubjectType: subjectPosition, SubjectID: "p", ScopeType: scopeTenant, Status: activeStatus, OriginItemID: "item", Version: 1}
	for _, tc := range []struct {
		name                               string
		change                             func(*bindingProjection)
		deactivate, keepCanonical, restore bool
		fail                               string
	}{
		{name: "canonical unchanged"},
		{name: "role drift", change: func(b *bindingProjection) { b.RoleID = "wrong" }},
		{name: "application drift", change: func(b *bindingProjection) { b.ApplicationID = "old-app" }},
		{name: "scope drift", change: func(b *bindingProjection) { b.ScopeType = "RESOURCE"; b.ScopeID = "other" }},
		{name: "user displacement", change: func(b *bindingProjection) { b.SubjectType = "USER"; b.SubjectID = "former-user" }},
		{name: "account displacement", change: func(b *bindingProjection) { b.SubjectType = "ACCOUNT"; b.SubjectID = "old-account" }},
		{name: "organization displacement", change: func(b *bindingProjection) { b.SubjectType = "ORG_UNIT"; b.SubjectID = "old-org" }},
		{name: "position displacement", change: func(b *bindingProjection) { b.SubjectID = "old-position" }},
		{name: "disable moved user", change: func(b *bindingProjection) { b.SubjectType = "USER"; b.SubjectID = "former-user" }, deactivate: true},
		{name: "drift alongside canonical", change: func(b *bindingProjection) { b.RoleID = "wrong" }, keepCanonical: true},
		{name: "reactivate canonical history", restore: true},
		{name: "failed replacement rolls back cleanup", change: func(b *bindingProjection) { b.RoleID = "wrong" }, fail: "INSERT INTO `authz_role_binding`"},
		{name: "failed revocation enqueue rolls back cleanup", change: func(b *bindingProjection) { b.SubjectType = "USER"; b.SubjectID = "former-user" }, fail: "INSERT INTO `keycloak_authorization_outbox`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := canonical
			if tc.change != nil {
				b.ID = "drift"
				tc.change(&b)
			}
			if tc.restore {
				b.Status = disabledStatus
			}
			f := &materializationFixture{rows: []bindingProjection{b}, active: !tc.deactivate, fail: tc.fail}
			if tc.keepCanonical {
				f.rows = append(f.rows, canonical)
			}
			db := materializationDB(t, f)
			s := &Service{db: db, ids: &materializationIDs{}}
			apps := map[string]struct{}{}
			positions := map[string]map[string]struct{}{}
			err := db.Transaction(func(tx *gorm.DB) error {
				return s.syncAssignment(context.Background(), tx, "t", "a", "operator", time.Now().UTC(), apps, positions)
			})
			if tc.fail != "" {
				if err == nil || f.committed || !f.rolledBack {
					t.Fatalf("failure did not roll back: err=%v fixture=%+v", err, f)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.change == nil && !tc.restore {
				if len(f.writes) != 0 {
					t.Fatal("unchanged canonical mutated")
				}
				return
			}
			var bindingInserts, disables, reactivations int
			notified := false
			for _, w := range f.writes {
				if strings.HasPrefix(w.query, "UPDATE `authz_role_binding`") {
					if w.args[0].Value == disabledStatus {
						disables++
					} else if w.args[0].Value == activeStatus {
						reactivations++
					}
				}
				if strings.HasPrefix(w.query, "INSERT INTO `authz_role_binding`") {
					bindingInserts++
					if insertedValue(w, "role_id") != "role" || insertedValue(w, "subject_type") != subjectPosition || insertedValue(w, "subject_id") != "p" {
						t.Fatalf("invalid replacement: %+v", w)
					}
				}
				if strings.HasPrefix(w.query, "INSERT INTO `keycloak_authorization_outbox`") && insertedValue(w, "identity_id") == "former-user" {
					notified = true
				}
			}
			if tc.restore {
				if reactivations != 1 || bindingInserts != 0 {
					t.Fatal("canonical historical row not reused")
				}
				return
			}
			if disables != 1 {
				t.Fatalf("expected displaced binding disabled, got %d", disables)
			}
			wantInsert := 1
			if tc.deactivate || tc.keepCanonical {
				wantInsert = 0
			}
			if bindingInserts != wantInsert {
				t.Fatalf("insert count=%d want=%d", bindingInserts, wantInsert)
			}
			if b.SubjectType != subjectPosition || b.SubjectID != "p" {
				if !notified {
					t.Fatal("former subject projection not enqueued")
				}
			}
			if _, ok := apps[b.ApplicationID]; !ok {
				t.Fatal("former application revision missing")
			}
		})
	}
}

type materializationResult int64

func (r materializationResult) LastInsertId() (int64, error) { return int64(r), nil }
func (r materializationResult) RowsAffected() (int64, error) { return int64(r), nil }
