package applicationaccess

import (
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

func TestBuildEditRevisionIsDeterministicAndOrderIndependent(t *testing.T) {
	validFrom := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	bindings := []editRevisionBinding{
		{ID: "binding-b", Version: 2, RoleID: "role-2", ScopeType: "ENVIRONMENT", ScopeID: "env-1", Status: "ACTIVE", ValidFrom: revisionTimeFormat(&validFrom), GrantOrigin: "MANUAL"},
		{ID: "binding-a", Version: 1, RoleID: "role-1", ScopeType: "TENANT", Status: "DISABLED", GrantOrigin: "MANUAL"},
	}
	permissions := []editRevisionPermission{
		{PermissionID: "perm-2", CreatedAt: revisionTimeFormat(&validFrom)},
		{PermissionID: "perm-1", CreatedAt: revisionTimeFormat(&validFrom)},
	}
	first := buildEditRevision("tenant-1", "user-1", "app-1", bindings, permissions)
	if !strings.HasPrefix(first, EditRevisionPrefix) {
		t.Fatalf("revision missing prefix: %s", first)
	}
	// 相同内容乱序输入必须得到同一版本；不同内容必须不同。
	reordered := []editRevisionBinding{bindings[1], bindings[0]}
	reorderedPerms := []editRevisionPermission{permissions[1], permissions[0]}
	if again := buildEditRevision("tenant-1", "user-1", "app-1", reordered, reorderedPerms); again != first {
		t.Fatalf("order changed revision: %s != %s", again, first)
	}
	if changed := buildEditRevision("tenant-1", "user-1", "app-1", bindings[1:], permissions); changed == first {
		t.Fatal("dropping a binding must change the revision")
	}
	if changed := buildEditRevision("tenant-1", "user-2", "app-1", bindings, permissions); changed == first {
		t.Fatal("different user must change the revision")
	}
}

func TestBuildEditRevisionDistinguishesLifecycleAndTimePrecision(t *testing.T) {
	validFrom := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	base := []editRevisionBinding{{ID: "b1", Version: 1, RoleID: "r1", ScopeType: "TENANT", Status: "ACTIVE", ValidFrom: revisionTimeFormat(&validFrom), GrantOrigin: "MANUAL"}}
	permissions := []editRevisionPermission{}
	baseline := buildEditRevision("tenant-1", "user-1", "app-1", base, permissions)

	// 状态变化（停用/恢复）必须改变版本：停用绑定仍在可编辑集合内。
	disabled := []editRevisionBinding{base[0]}
	disabled[0].Status = "DISABLED"
	if same := buildEditRevision("tenant-1", "user-1", "app-1", disabled, permissions); same == baseline {
		t.Fatal("status change must change the revision")
	}

	// 时间精度固定到毫秒：亚毫秒差异不得改变版本；整秒差异必须改变版本。
	oneSecondLater := validFrom.Add(time.Second)
	timeSensitive := []editRevisionBinding{{ID: "b1", Version: 1, RoleID: "r1", ScopeType: "TENANT", Status: "ACTIVE", ValidFrom: revisionTimeFormat(&oneSecondLater), GrantOrigin: "MANUAL"}}
	if same := buildEditRevision("tenant-1", "user-1", "app-1", timeSensitive, permissions); same == baseline {
		t.Fatal("one-second validity change must change the revision")
	}
}

func TestBuildEditRevisionCoversEmptyCollection(t *testing.T) {
	// 空集合仍返回合法版本，防止"从空新增"绕过并发保护（方案 R10）。
	empty := buildEditRevision("tenant-1", "user-1", "app-1", nil, nil)
	other := buildEditRevision("tenant-1", "user-1", "app-2", nil, nil)
	if !strings.HasPrefix(empty, EditRevisionPrefix) || len(empty) != len(EditRevisionPrefix)+64 {
		t.Fatalf("empty collection revision malformed: %q", empty)
	}
	if empty == other {
		t.Fatal("empty collections for different applications must differ")
	}
}

func TestEnsureExpectedRevisionRejectsMalformedCondition(t *testing.T) {
	// 格式校验不触达数据库：直接用零值 Service 检查 422 语义。
	service := &Service{}
	if _, err := service.ensureExpectedRevision(nil, "tenant-1", "user-1", "app-1", "not-a-revision", false); err == nil {
		t.Fatal("malformed revision must be rejected")
	} else if !strings.Contains(err.Error(), "expected_revision format is invalid") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureExpectedRevisionStrictModeRequiresCondition(t *testing.T) {
	// 严格模式（兼容发布第 2 步开关）下缺省条件必须 422；兼容模式跳过比较。
	service := &Service{}
	if _, err := service.ensureExpectedRevision(nil, "tenant-1", "user-1", "app-1", "", true); err == nil {
		t.Fatal("strict mode must reject missing expected_revision")
	} else if !strings.Contains(err.Error(), "expected_revision is required") {
		t.Fatalf("unexpected error: %v", err)
	}
	skipped, err := service.ensureExpectedRevision(nil, "tenant-1", "user-1", "app-1", "", false)
	if err != nil || skipped {
		t.Fatalf("compat mode must skip comparison without database: skipped=%v err=%v", skipped, err)
	}
}

func TestStrictRevisionFromEnvironment(t *testing.T) {
	for _, tt := range []struct{ value string; want bool }{
		{"1", true}, {"true", true}, {"YES", true}, {"on", true}, {" true ", true},
		{"", false}, {"0", false}, {"false", false}, {"off", false}, {"garbage", false},
	} {
		if got := strictRevisionFromEnvironment(tt.value); got != tt.want {
			t.Fatalf("strictRevisionFromEnvironment(%q)=%v; want %v", tt.value, got, tt.want)
		}
	}
}

// TestLoadEditRevisionRowsScopesToSubject 验证编辑版本查询必须限定目标主体：
// 他人（用户 B）的绑定变化不得推高用户 A 的编辑版本（否则制造无关 409）。
func TestLoadEditRevisionRowsScopesToSubject(t *testing.T) {
	driverName := fmt.Sprintf("appaccess-revision-scope-%d", atomic.AddUint64(&revisionScopeDriverSeq, 1))
	sql.Register(driverName, &revisionScopeDriver{})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}

	bindingsA, _, err := loadEditRevisionRows(db, "tenant-1", "user-a", "app-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindingsA) != 1 || bindingsA[0].ID != "binding-a" {
		t.Fatalf("user-a revision must only contain user-a bindings: %+v", bindingsA)
	}
	bindingsB, _, err := loadEditRevisionRows(db, "tenant-1", "user-b", "app-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindingsB) != 1 || bindingsB[0].ID != "binding-b" {
		t.Fatalf("user-b revision must only contain user-b bindings: %+v", bindingsB)
	}
}

var revisionScopeDriverSeq uint64

// revisionScopeDriver 按「查询是否携带主体参数」返回对应主体的绑定行：
// 旧实现（WHERE 缺 subject_id）会因缺少主体条件被 Prepare 断言拒绝。
type revisionScopeDriver struct{}

func (d *revisionScopeDriver) Open(string) (driver.Conn, error) { return revisionScopeConn{}, nil }

type revisionScopeConn struct{}

func (revisionScopeConn) Prepare(query string) (driver.Stmt, error) { return &revisionScopeStmt{query: query}, nil }
func (revisionScopeConn) Close() error                              { return nil }
func (revisionScopeConn) Begin() (driver.Tx, error)                 { return revisionScopeTx{}, nil }

type revisionScopeTx struct{}

func (revisionScopeTx) Commit() error   { return nil }
func (revisionScopeTx) Rollback() error { return nil }

type revisionScopeStmt struct{ query string }

func (s *revisionScopeStmt) Close() error  { return nil }
func (s *revisionScopeStmt) NumInput() int { return -1 }

func (s *revisionScopeStmt) Exec([]driver.Value) (driver.Result, error) { return nil, errors.New("unexpected exec") }

func (s *revisionScopeStmt) Query(args []driver.Value) (driver.Rows, error) {
	plain := strings.ReplaceAll(s.query, "`", "")
	if strings.Contains(plain, "authz_user_permission") {
		return &revisionScopeRows{columns: []string{"permission_id", "created_at"}}, nil
	}
	if !strings.Contains(plain, "rb.subject_id = ?") {
		return nil, fmt.Errorf("edit revision query must scope by subject: %s", s.query)
	}
	rows := &revisionScopeRows{columns: []string{"id", "version", "role_id", "scope_type", "scope_id", "status", "valid_from", "valid_until", "grant_origin", "origin_id", "origin_item_id"}}
	// WHERE 顺序：tenant_id, application_id, subject_type, subject_id, grant_origin。
	if len(args) < 5 || args[3] == nil {
		return nil, fmt.Errorf("unexpected args %v in %s", args, s.query)
	}
	switch fmt.Sprint(args[3]) {
	case "user-a":
		rows.values = [][]driver.Value{{"binding-a", int64(1), "role-1", "TENANT", "", "ACTIVE", nil, nil, "MANUAL", "", ""}}
	case "user-b":
		rows.values = [][]driver.Value{{"binding-b", int64(1), "role-1", "TENANT", "", "ACTIVE", nil, nil, "MANUAL", "", ""}}
	default:
		return nil, fmt.Errorf("unexpected subject arg %v in %s", args, s.query)
	}
	return rows, nil
}

type revisionScopeRows struct {
	columns []string
	values  [][]driver.Value
	cursor  int
}

func (r *revisionScopeRows) Columns() []string { return r.columns }
func (r *revisionScopeRows) Close() error      { return nil }
func (r *revisionScopeRows) Next(dest []driver.Value) error {
	if r.cursor >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.cursor])
	r.cursor++
	return nil
}
