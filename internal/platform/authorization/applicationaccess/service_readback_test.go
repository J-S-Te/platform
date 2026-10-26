package applicationaccess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 本文件回归验收项「保存成功与响应读取失败必须分开」：
// UpdateAccess 写事务提交成功后、GetAccess 读回失败时——
//   1. 返回错误必须可识别为 ErrAccessReadback（保存事实已成立）；
//   2. 成功审计不得被跳过，且 Metadata 标注 access_view_error。
//
// 驱动按事务 Begin 次序毒化：第 1 个事务=前置 GetAccess 尾部快照，第 2 个=写事务，
// 第 3 个起（提交后的读回事务）所有查询失败。写路径全部走生产实现。

type readbackAuditRecorder struct{ events []AuditEvent }

func (r *readbackAuditRecorder) RecordApplicationAccessAudit(_ context.Context, event AuditEvent) error {
	r.events = append(r.events, event)
	return nil
}

var readbackDriverSeq uint64

type readbackDriver struct {
	poisonFromBegin int
	beginCount      int
}

func (d *readbackDriver) Open(string) (driver.Conn, error) { return &readbackConn{d}, nil }

type readbackConn struct{ driver *readbackDriver }

func (c *readbackConn) Prepare(query string) (driver.Stmt, error) {
	return &readbackStmt{conn: c, query: query}, nil
}
func (c *readbackConn) Close() error  { return nil }
func (c *readbackConn) Begin() (driver.Tx, error) {
	c.driver.beginCount++
	return readbackTx{}, nil
}

type readbackTx struct{}

func (readbackTx) Commit() error   { return nil }
func (readbackTx) Rollback() error { return nil }

type readbackStmt struct {
	conn   *readbackConn
	query  string
}

func (s *readbackStmt) Close() error  { return nil }
func (s *readbackStmt) NumInput() int { return -1 }

func (s *readbackStmt) Exec(args []driver.Value) (driver.Result, error) {
	q := strings.ToLower(s.query)
	switch {
	case strings.HasPrefix(q, "insert into `authz_role_binding`"),
		strings.HasPrefix(q, "update `authz_role_binding`"),
		strings.HasPrefix(q, "insert into `authz_policy_revision`"),
		strings.HasPrefix(q, "insert into `keycloak_authorization_outbox`"):
		return readbackResult{}, nil
	}
	return nil, fmt.Errorf("unexpected write: %s", s.query)
}

type readbackResult struct{}

func (readbackResult) LastInsertId() (int64, error) { return 1, nil }
func (readbackResult) RowsAffected() (int64, error) { return 1, nil }

func (s *readbackStmt) Query(args []driver.Value) (driver.Rows, error) {
	if s.conn.driver.beginCount >= s.conn.driver.poisonFromBegin {
		return nil, errors.New("simulated read failure after commit")
	}
	plain := strings.ReplaceAll(s.query, "`", "")
	lower := strings.ToLower(plain)
	switch {
	case strings.Contains(lower, "from iam_tenant"):
		return &rowsWithValues{columns: []string{"id"}, values: [][]driver.Value{{"tenant-1"}}}, nil
	case strings.Contains(lower, "from platform_application"):
		return &rowsWithValues{columns: []string{"id", "tenant_id", "code"}, values: [][]driver.Value{{"app-1", "tenant-1", "sales-app"}}}, nil
	case strings.Contains(lower, "from authz_application_authorization_policy"):
		return &rowsWithValues{columns: []string{"application_id"}, values: [][]driver.Value{{"app-1"}}}, nil
	case strings.Contains(lower, "from authz_authorization_catalog"):
		return &rowsWithValues{columns: []string{"claims_role_config_hash"}, values: [][]driver.Value{{"catalog-hash"}}}, nil
	case strings.Contains(lower, "from authz_policy_revision"):
		return &rowsWithValues{columns: []string{"revision"}, values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(lower, "count(") && strings.Contains(lower, "from iam_user"):
		return &rowsWithValues{columns: []string{"count"}, values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(lower, "count("):
		return &rowsWithValues{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	case strings.Contains(lower, "from keycloak_application_client_mapping"):
		return &rowsWithValues{columns: []string{"environment_id"}}, nil
	default:
		// authz_role_binding 的各类读（有效角色、超管判定、MANUAL 管理视图、
		// edit revision 绑定行）与 authz_user_permission：空集驱动合法业务路径。
		return &rowsWithValues{columns: []string{"value"}}, nil
	}
}

// rowsWithValues 用于需要返回具体行的分支。
type rowsWithValues struct {
	columns []string
	values  [][]driver.Value
	cursor  int
}

func (r *rowsWithValues) Columns() []string { return r.columns }
func (*rowsWithValues) Close() error        { return nil }
func (r *rowsWithValues) Next(dest []driver.Value) error {
	if r.cursor >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.cursor])
	r.cursor++
	return nil
}

type readbackIDs struct{}

func (readbackIDs) New(time.Time) (string, error) { return "binding-new", nil }

type readbackClock struct{ now time.Time }

func (c readbackClock) Now() time.Time { return c.now }

func TestUpdateAccessReportsCommittedWhenPostCommitReadFails(t *testing.T) {
	driverName := fmt.Sprintf("appaccess-readback-%d", atomic.AddUint64(&readbackDriverSeq, 1))
	sql.Register(driverName, &readbackDriver{poisonFromBegin: 3})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{db: db, ids: readbackIDs{}, clock: readbackClock{time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC)}}
	recorder := &readbackAuditRecorder{}
	service.audit = recorder

	_, err = service.UpdateAccess(context.Background(), UpdateAccessInput{
		TenantID: "tenant-1", UserID: "user-1", OperatorID: "operator-1",
		Roles: []RoleInput{}, RolesProvided: true,
	}, "sales-app")
	if err == nil {
		t.Fatal("post-commit read failure must surface an error")
	}
	if !errors.Is(err, ErrAccessReadback) {
		t.Fatalf("error must be identifiable as committed-but-unreadable, got: %v", err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("exactly one SUCCESS audit must be recorded after commit, got %d", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Result != "SUCCESS" || event.Action != "authorization.application_access.updated" {
		t.Fatalf("audit must record the committed update: result=%s action=%s", event.Result, event.Action)
	}
	if message, ok := event.Metadata["access_view_error"].(string); !ok || message == "" {
		t.Fatalf("audit metadata must annotate access_view_error, got %#v", event.Metadata)
	}
}

// 对照组：读回正常时必须返回完整 Access 且审计携带版本与变更（不毒化）。
func TestUpdateAccessReturnsAccessWhenReadbackSucceeds(t *testing.T) {
	driverName := fmt.Sprintf("appaccess-readback-ok-%d", atomic.AddUint64(&readbackDriverSeq, 1))
	sql.Register(driverName, &readbackDriver{poisonFromBegin: 1 << 30})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{db: db, ids: readbackIDs{}, clock: readbackClock{time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC)}}
	recorder := &readbackAuditRecorder{}
	service.audit = recorder

	access, err := service.UpdateAccess(context.Background(), UpdateAccessInput{
		TenantID: "tenant-1", UserID: "user-1", OperatorID: "operator-1",
		Roles: []RoleInput{}, RolesProvided: true,
	}, "sales-app")
	if err != nil {
		t.Fatalf("clean path must succeed: %v", err)
	}
	if access.ApplicationCode != "sales-app" {
		t.Fatalf("access view must be returned, got %#v", access)
	}
	if len(recorder.events) != 1 || recorder.events[0].Result != "SUCCESS" {
		t.Fatalf("clean path must keep the SUCCESS audit, got %#v", recorder.events)
	}
	if _, annotated := recorder.events[0].Metadata["access_view_error"]; annotated {
		t.Fatal("clean path audit must not annotate access_view_error")
	}
}
