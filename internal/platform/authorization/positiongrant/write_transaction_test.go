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

var errTemplateLockFixture = errors.New("tenant configuration lock unavailable")

type configurationLockFixture struct {
	transaction bool
	first       string
	args        []driver.NamedValue
	writes      int
	rollback    bool
}
type configurationLockDriver struct{ f *configurationLockFixture }
type configurationLockConn struct{ f *configurationLockFixture }
type configurationLockTx struct{ f *configurationLockFixture }

var configurationLockCounter uint64

func (d configurationLockDriver) Open(string) (driver.Conn, error) {
	return &configurationLockConn{d.f}, nil
}
func (c *configurationLockConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("no prepare")
}
func (c *configurationLockConn) Close() error { return nil }
func (c *configurationLockConn) Begin() (driver.Tx, error) {
	c.f.transaction = true
	return &configurationLockTx{c.f}, nil
}
func (c *configurationLockTx) Commit() error   { return errors.New("lock failure must not commit") }
func (c *configurationLockTx) Rollback() error { c.f.rollback = true; return nil }
func (c *configurationLockConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.f.writes++
	return nil, errors.New("no write expected")
}
func (c *configurationLockConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if c.f.transaction {
		c.f.first = q
		c.f.args = args
		return nil, errTemplateLockFixture
	}
	if strings.Contains(q, "iam_position") {
		return regressionRows("count", []driver.Value{int64(1)}), nil
	}
	return nil, fmt.Errorf("unexpected pre-transaction read: %s", q)
}
func TestAllTemplateWritersTakeTenantLockBeforeTheirReadsOrWrites(t *testing.T) {
	for _, name := range []string{"create", "update", "delete", "replace assignments", "role inheritances"} {
		t.Run(name, func(t *testing.T) {
			f := &configurationLockFixture{}
			driverName := fmt.Sprintf("template-lock-%d", atomic.AddUint64(&configurationLockCounter, 1))
			sql.Register(driverName, configurationLockDriver{f})
			raw, _ := sql.Open(driverName, "")
			defer raw.Close()
			db, err := gorm.Open(mysql.New(mysql.Config{Conn: raw, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			s := &Service{db: db, clock: assignmentRegressionClock{time.Now().UTC()}, ids: assignmentRegressionIDs{}}
			switch name {
			case "create":
				_, err = s.Create(context.Background(), "tenant-A", "u", TemplateInput{Name: "name", Status: activeStatus})
			case "update":
				_, err = s.Update(context.Background(), "tenant-A", "u", "template", TemplateInput{Name: "name", Status: activeStatus, Version: 1})
			case "delete":
				err = s.Delete(context.Background(), "tenant-A", "u", "template", 1)
			case "replace assignments":
				_, err = s.ReplacePositionAssignments(context.Background(), "tenant-A", "u", "p", ReplaceAssignmentsInput{ExpectedRevision: assignmentSetRevision("tenant-A", "p", nil)})
			case "role inheritances":
				_, err = s.ReplaceRoleInheritances(context.Background(), "tenant-A", "u", RoleInheritanceReplaceInput{SourceRoleID: "role"})
			}
			if !errors.Is(err, errTemplateLockFixture) || !strings.Contains(f.first, "`iam_tenant`") || !strings.Contains(f.first, "FOR UPDATE") || len(f.args) < 1 || f.args[0].Value != "tenant-A" || f.writes != 0 || !f.rollback {
				t.Fatalf("lock hierarchy/rollback violated: err=%v fixture=%+v", err, f)
			}
		})
	}
}
