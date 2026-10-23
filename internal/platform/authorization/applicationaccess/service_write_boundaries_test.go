package applicationaccess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 仅替换 SQL 输入输出；被测的对账、委派和 HTTP 错误映射
// 均执行生产实现，不连接外部数据库。
type writeBoundaryScenario struct {
	existing []bindingRow
	held     int64
	writes   int
	queries  []string
	upcoming *time.Time
}
type writeBoundaryResult struct{}

func (writeBoundaryResult) LastInsertId() (int64, error) { return 1, nil }
func (writeBoundaryResult) RowsAffected() (int64, error) { return 1, nil }

type writeBoundaryDriver struct{ scenario *writeBoundaryScenario }
type writeBoundaryConn struct{ scenario *writeBoundaryScenario }
type writeBoundaryRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *writeBoundaryRows) Columns() []string { return r.columns }
func (*writeBoundaryRows) Close() error        { return nil }
func (r *writeBoundaryRows) Next(out []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(out, r.values[r.index])
	r.index++
	return nil
}
func (d *writeBoundaryDriver) Open(string) (driver.Conn, error) {
	return &writeBoundaryConn{d.scenario}, nil
}
func (*writeBoundaryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared SQL")
}
func (*writeBoundaryConn) Close() error { return nil }
func (*writeBoundaryConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (c *writeBoundaryConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	q := strings.ToLower(query)
	if !strings.HasPrefix(q, "insert into `authz_role_binding`") && !strings.HasPrefix(q, "update `authz_role_binding`") {
		return nil, fmt.Errorf("unexpected write: %s", query)
	}
	c.scenario.writes++
	return writeBoundaryResult{}, nil
}
func boundaryTime(value *time.Time) driver.Value {
	if value == nil {
		return nil
	}
	return *value
}
func boundaryArg(args []driver.NamedValue, want string) bool {
	for _, arg := range args {
		if arg.Value == want {
			return true
		}
	}
	return false
}
func (c *writeBoundaryConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.scenario.queries = append(c.scenario.queries, query)
	q := strings.ToLower(query)
	if c.scenario.upcoming != nil {
		plain := strings.ReplaceAll(q, "`", "")
		switch {
		case strings.Contains(plain, "from platform_application "):
			return &writeBoundaryRows{columns: []string{"id", "tenant_id", "code"}, values: [][]driver.Value{{"app-1", "tenant-1", "sales-app"}}}, nil
		case strings.Contains(plain, "from authz_application_authorization_policy"):
			return &writeBoundaryRows{columns: []string{"application_id"}}, nil
		case strings.Contains(plain, "from authz_authorization_catalog"):
			return &writeBoundaryRows{columns: []string{"claims_role_config_hash"}, values: [][]driver.Value{{"test-catalog-hash"}}}, nil
		case strings.Contains(plain, "from authz_policy_revision"):
			return &writeBoundaryRows{columns: []string{"revision"}, values: [][]driver.Value{{int64(1)}}}, nil
		case strings.Contains(plain, "count(*)") && strings.Contains(plain, "from authz_role_binding as binding"):
			return &writeBoundaryRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
		case strings.Contains(plain, "from authz_role_binding as rb"):
			rows := &writeBoundaryRows{columns: []string{"role_id", "code", "name", "scope_type", "scope_id", "valid_from", "valid_until", "subject_type", "subject_id", "grant_origin"}}
			if !strings.Contains(plain, "rb.valid_from <=") {
				if !strings.Contains(plain, "rb.tenant_id") || !strings.Contains(plain, "rb.application_id") || !boundaryArg(args, "tenant-1") || !boundaryArg(args, "app-1") || !boundaryArg(args, "operator-1") || !boundaryArg(args, "MANUAL") {
					return nil, fmt.Errorf("unscoped manual management query: %s", query)
				}
				rows.values = [][]driver.Value{{"role-future", "scheduled-sales", "预授予销售", "TENANT", "", *c.scenario.upcoming, nil, "USER", "operator-1", "MANUAL"}}
			}
			return rows, nil
		}
	}
	switch {
	case strings.Contains(q, "authz_role_binding as rb") && strings.Contains(q, "rb.version"):
		if !strings.Contains(q, "rb.tenant_id = ?") || !strings.Contains(q, "rb.application_id = ?") || !boundaryArg(args, "tenant-1") || !boundaryArg(args, "app-1") || !boundaryArg(args, "operator-1") {
			return nil, fmt.Errorf("unscoped reconciliation query: %s", query)
		}
		rows := &writeBoundaryRows{columns: []string{"id", "role_id", "scope_type", "scope_id", "valid_from", "valid_until", "status", "version"}}
		for _, binding := range c.scenario.existing {
			rows.values = append(rows.values, []driver.Value{binding.ID, binding.RoleID, binding.ScopeType, binding.ScopeID, boundaryTime(binding.ValidFrom), boundaryTime(binding.ValidUntil), binding.Status, int64(binding.Version)})
		}
		return rows, nil
	case strings.Contains(q, "count(distinct(") && strings.Contains(q, "authz_role_binding as binding"):
		if c.scenario.writes > 0 {
			return nil, errors.New("delegation checked after authorization write")
		}
		if !boundaryArg(args, "tenant-1") || !boundaryArg(args, "app-1") || !boundaryArg(args, "operator-1") {
			return nil, errors.New("delegation check lost tenant/application/operator")
		}
		return &writeBoundaryRows{columns: []string{"count"}, values: [][]driver.Value{{c.scenario.held}}}, nil
	case strings.Contains(q, "authz_role_permission as role_permission"):
		if c.scenario.writes > 0 {
			return nil, errors.New("role permissions checked after authorization write")
		}
		if !boundaryArg(args, "tenant-1") || !boundaryArg(args, "app-1") || !boundaryArg(args, "role-new") {
			return nil, errors.New("role permission check lost tenant/application/role")
		}
		return &writeBoundaryRows{columns: []string{"permission_id"}, values: [][]driver.Value{{"permission-read"}, {"permission-manage"}}}, nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
}

type writeBoundaryIDs struct{}

func (writeBoundaryIDs) New(time.Time) (string, error) { return "binding-new", nil }

type writeBoundaryClock struct{ now time.Time }

func (c writeBoundaryClock) Now() time.Time { return c.now }

var writeBoundaryCounter uint64

func newWriteBoundaryService(t *testing.T, scenario *writeBoundaryScenario) *Service {
	t.Helper()
	name := fmt.Sprintf("appaccess-write-boundary-%d", atomic.AddUint64(&writeBoundaryCounter, 1))
	sql.Register(name, &writeBoundaryDriver{scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	return &Service{db: db, ids: writeBoundaryIDs{}, clock: writeBoundaryClock{time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}}
}
func TestManualBindingsDelegationRejectsNewOrExpandedGrantsBeforeAnyWrite(t *testing.T) {
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	early := start.Add(-time.Hour)
	end := start.Add(48 * time.Hour)
	late := end.Add(time.Hour)
	for _, tt := range []struct {
		name        string
		existing    *bindingRow
		from, until *time.Time
	}{
		{name: "new self grant"},
		{name: "reactivate", existing: &bindingRow{Status: disabledStatus}},
		{name: "earlier start", existing: &bindingRow{Status: activeStatus, ValidFrom: &start, ValidUntil: &end}, from: &early, until: &end},
		{name: "remove start", existing: &bindingRow{Status: activeStatus, ValidFrom: &start, ValidUntil: &end}, until: &end},
		{name: "extend end", existing: &bindingRow{Status: activeStatus, ValidFrom: &start, ValidUntil: &end}, from: &start, until: &late},
		{name: "remove end", existing: &bindingRow{Status: activeStatus, ValidFrom: &start, ValidUntil: &end}, from: &start},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// 已有的待移除角色确保拒绝操作也必须早于撤销。
			scenario := &writeBoundaryScenario{held: 1, existing: []bindingRow{{ID: "binding-old", RoleID: "role-old", ScopeType: scopeTypeTenant, Status: activeStatus, Version: 1}}}
			if tt.existing != nil {
				b := *tt.existing
				b.ID = "binding-retained"
				b.RoleID = "role-new"
				b.ScopeType = scopeTypeTenant
				b.Version = 1
				scenario.existing = append(scenario.existing, b)
			}
			service := newWriteBoundaryService(t, scenario)
			changed, err := service.replaceManualUserRoleBindings(service.db.WithContext(context.Background()), "tenant-1", "app-1", "operator-1", "operator-1", []resolvedBinding{{roleID: "role-new", scopeType: scopeTypeTenant, role: RoleInput{ValidFrom: tt.from, ValidUntil: tt.until}}}, service.clock.Now())
			if !errors.Is(err, authapplication.ErrForbidden) {
				t.Errorf("error = %v; want delegable-permission ErrForbidden", err)
			}
			if changed || scenario.writes != 0 {
				t.Errorf("rejected request changed=%v, SQL writes=%d; want false, 0", changed, scenario.writes)
			}
		})
	}
}
func TestManualBindingsDelegationAllowsHeldPermissionsAndAuthenticatedSuperAdmin(t *testing.T) {
	for _, super := range []bool{false, true} {
		t.Run(fmt.Sprintf("super=%v", super), func(t *testing.T) {
			scenario := &writeBoundaryScenario{held: 2}
			service := newWriteBoundaryService(t, scenario)
			ctx := context.Background()
			if super {
				scenario.held = 0
				ctx = authctx.WithPrincipal(ctx, authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"}, Roles: []authctx.ReferenceName{{Code: "platform-super-admin"}}})
			}
			changed, err := service.replaceManualUserRoleBindings(service.db.WithContext(ctx), "tenant-1", "app-1", "operator-1", "operator-1", []resolvedBinding{{roleID: "role-new", scopeType: scopeTypeTenant}}, service.clock.Now())
			if err != nil || !changed || scenario.writes != 1 {
				t.Fatalf("allowed grant changed=%v writes=%d err=%v", changed, scenario.writes, err)
			}
		})
	}
}
func TestManualBindingsDelegationAllowsUnchangedShorteningAndRevocation(t *testing.T) {
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	later := start.Add(time.Hour)
	end := start.Add(48 * time.Hour)
	shorter := end.Add(-time.Hour)
	for _, tt := range []struct {
		name        string
		from, until *time.Time
		revoke      bool
		wantWrites  int
	}{
		{name: "unchanged", from: &start, until: &end},
		{name: "later start", from: &later, until: &end, wantWrites: 1},
		{name: "earlier end", from: &start, until: &shorter, wantWrites: 1},
		{name: "revoke", revoke: true, wantWrites: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scenario := &writeBoundaryScenario{existing: []bindingRow{{ID: "binding-1", RoleID: "role-new", ScopeType: scopeTypeTenant, Status: activeStatus, ValidFrom: &start, ValidUntil: &end, Version: 1}}}
			service := newWriteBoundaryService(t, scenario)
			desired := []resolvedBinding{{roleID: "role-new", scopeType: scopeTypeTenant, role: RoleInput{ValidFrom: tt.from, ValidUntil: tt.until}}}
			if tt.revoke {
				desired = nil
			}
			changed, err := service.replaceManualUserRoleBindings(service.db, "tenant-1", "app-1", "operator-1", "operator-1", desired, service.clock.Now())
			if err != nil || scenario.writes != tt.wantWrites || changed != (tt.wantWrites > 0) {
				t.Fatalf("nonexpanding edit changed=%v writes=%d err=%v", changed, scenario.writes, err)
			}
			for _, query := range scenario.queries {
				if strings.Contains(query, "role_permission") {
					t.Errorf("nonexpanding edit unexpectedly needs target permission: %s", query)
				}
			}
		})
	}
}
func TestWriteErrorDelegationForbiddenReturns403(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/access", nil)
	writeError(response, request, fmt.Errorf("delegation denied: %w", authapplication.ErrForbidden))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s; want 403", response.Code, response.Body.String())
	}
}

func TestGetAccessPreservesUpcomingManualAssignmentsWithoutEffectiveAccess(t *testing.T) {
	upcoming := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	scenario := &writeBoundaryScenario{upcoming: &upcoming}
	service := newWriteBoundaryService(t, scenario)
	access, err := service.GetAccess(context.Background(), "tenant-1", "operator-1", "sales-app")
	if err != nil {
		t.Fatal(err)
	}
	if len(access.ManualRoles) != 1 || access.ManualRoles[0].Code != "scheduled-sales" || access.ManualRoles[0].ValidFrom == nil || !access.ManualRoles[0].ValidFrom.Equal(upcoming) {
		t.Fatalf("future manual assignment missing from editable set: %+v", access.ManualRoles)
	}
	if access.AuthorizationState != authorizationUnauthorized || len(access.Roles) != 0 || len(access.DirectRoles) != 0 || len(access.EffectivePermissions) != 0 {
		t.Fatalf("future-only assignment incorrectly became effective: %+v", access)
	}
	if scenario.writes != 0 {
		t.Fatalf("management read wrote %d records", scenario.writes)
	}
	granted, err := service.HasApplicationGrant(context.Background(), "tenant-1", "operator-1", "sales-app")
	if err != nil || granted {
		t.Fatalf("future-only assignment grants access=%v err=%v", granted, err)
	}
	// 在真实管理响应中添加无关角色并重新提交。
	// 未来生效的绑定必须保持不变，因此唯一写入应为创建 role-new。
	future := access.ManualRoles[0]
	writeScenario := &writeBoundaryScenario{held: 2, existing: []bindingRow{{ID: "binding-future", RoleID: "role-future", ScopeType: scopeTypeTenant, Status: activeStatus, ValidFrom: &upcoming, Version: 1}}}
	writer := newWriteBoundaryService(t, writeScenario)
	changed, err := writer.replaceManualUserRoleBindings(writer.db, "tenant-1", "app-1", "operator-1", "operator-1", []resolvedBinding{
		{roleID: "role-future", scopeType: scopeTypeTenant, role: RoleInput{RoleCode: future.Code, ValidFrom: future.ValidFrom, ValidUntil: future.ValidUntil}},
		{roleID: "role-new", scopeType: scopeTypeTenant, role: RoleInput{RoleCode: "new-role"}},
	}, writer.clock.Now())
	if err != nil || !changed || writeScenario.writes != 1 {
		t.Fatalf("scheduled grant roundtrip changed=%v writes=%d err=%v; want only new role inserted", changed, writeScenario.writes, err)
	}
}
