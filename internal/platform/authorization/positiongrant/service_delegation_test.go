package positiongrant

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

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 岗位授权模板曾经只检查"目标角色有效 + 不是受保护角色"。持有 role-binding:update 的
// 人因此可以把本人并不拥有的普通角色挂到自己的有效继承岗位上，形成权限扩大。
// 现在模板路径复用普通角色绑定同一份「只能委派本人当前已有权限」判定。

type delegationScenario struct {
	// rolePermissionIDs 是目标角色包含的 ALLOW 权限数。
	rolePermissionIDs []string
	// operatorPermissionCount 是操作者在租户范围内实际拥有的权限命中数。
	operatorPermissionCount int64
	queries                 []string
}

type delegationDriver struct{ scenario *delegationScenario }
type delegationConn struct{ scenario *delegationScenario }

type delegationResult int64

func (result delegationResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result delegationResult) RowsAffected() (int64, error) { return int64(result), nil }

type delegationRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *delegationRows) Columns() []string { return rows.columns }
func (*delegationRows) Close() error           { return nil }

func (rows *delegationRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

func (d *delegationDriver) Open(string) (driver.Conn, error) {
	return &delegationConn{scenario: d.scenario}, nil
}

func (*delegationConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepared statements are not supported")
}
func (*delegationConn) Close() error { return nil }
func (*delegationConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("transactions are not supported")
}
func (*delegationConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return nil, fmt.Errorf("transactions are not supported")
}

func (c *delegationConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return delegationResult(1), nil
}

func (c *delegationConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.scenario.queries = append(c.scenario.queries, query)
	lowered := strings.ToLower(query)
	switch {
	case strings.Contains(lowered, "authz_role as role") && strings.Contains(lowered, "platform_application as application"):
		// 目标角色有效性检查：返回一个普通的 APPLICATION 角色，让既有检查通过，
		// 使委派边界成为唯一可能拒绝的原因。
		return &delegationRows{
			columns: []string{"id", "role_type", "code"},
			values:  [][]driver.Value{{"role-under-test", "APPLICATION", "sales"}},
		}, nil
	case strings.Contains(lowered, "count(distinct("):
		// 委派判定计数查询：必须排在 role_permission 明细查询之前，两者共用同一批 JOIN。
		return &delegationRows{columns: []string{"count"}, values: [][]driver.Value{{c.scenario.operatorPermissionCount}}}, nil
	case strings.Contains(lowered, "authz_role_permission as role_permission") && strings.Contains(lowered, "authz_permission as permission"):
		values := make([][]driver.Value, 0, len(c.scenario.rolePermissionIDs))
		for _, id := range c.scenario.rolePermissionIDs {
			values = append(values, []driver.Value{id})
		}
		return &delegationRows{columns: []string{"permission_id"}, values: values}, nil
	case strings.Contains(lowered, "authz_application_authorization_policy"):
		return &delegationRows{columns: []string{"application_id", "max_effective_roles"}}, nil
	default:
		return &delegationRows{columns: []string{"id"}}, nil
	}
}

var delegationDriverCounter uint64

func newDelegationTestService(t *testing.T, scenario *delegationScenario) *Service {
	t.Helper()
	driverName := fmt.Sprintf("positiongrant-delegation-%d", atomic.AddUint64(&delegationDriverCounter, 1))
	sql.Register(driverName, &delegationDriver{scenario: scenario})
	sqlDB, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open delegation test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open delegation gorm database: %v", err)
	}
	return &Service{db: database}
}

// 目标角色已经满足"有效 + 非受保护"两项既有检查；这里通过角色查询的返回值让它通过，
// 只留委派边界作为唯一可能拒绝的原因。
func delegationRoleLookupDriverStub() *delegationScenario {
	return &delegationScenario{}
}

func TestValidateItemsRejectsDelegatingRolePermissionsOperatorLacks(t *testing.T) {
	scenario := delegationRoleLookupDriverStub()
	scenario.rolePermissionIDs = []string{"permission-read", "permission-manage"}
	scenario.operatorPermissionCount = 1 // 操作者只拥有其中 1 个权限
	service := newDelegationTestService(t, scenario)

	items := []TemplateRoleInput{{ApplicationID: "app-1", RoleID: "role-security-admin", ScopeType: scopeTenant}}

	err := service.validateItems(context.Background(), service.db, "tenant-1", "operator-1", items)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("validateItems(超出本人权限) error = %v, want ErrForbidden", err)
	}
	if !containsQuery(scenario.queries, "authz_role_binding as binding") {
		t.Fatalf("未执行委派权限判定查询：%v", scenario.queries)
	}
}

func TestValidateItemsAllowsDelegatingRolePermissionsOperatorHolds(t *testing.T) {
	scenario := delegationRoleLookupDriverStub()
	scenario.rolePermissionIDs = []string{"permission-read", "permission-manage"}
	scenario.operatorPermissionCount = 2 // 操作者拥有全部权限
	service := newDelegationTestService(t, scenario)

	items := []TemplateRoleInput{{ApplicationID: "app-1", RoleID: "role-sales", ScopeType: scopeTenant}}

	if err := service.validateItems(context.Background(), service.db, "tenant-1", "operator-1", items); err != nil {
		t.Fatalf("validateItems(本人已有权限) error = %v, want nil", err)
	}
}

func TestValidateItemsAllowsPlatformSuperAdminToDelegateSubsystemRoles(t *testing.T) {
	scenario := delegationRoleLookupDriverStub()
	scenario.rolePermissionIDs = []string{"contract-read", "contract-manage", "settlement-read"}
	scenario.operatorPermissionCount = 0 // 超管没有逐应用的普通绑定
	service := newDelegationTestService(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "operator-1"},
		Roles:  []authctx.ReferenceName{{Code: "platform-super-admin"}},
	})

	items := []TemplateRoleInput{{ApplicationID: "app-contract", RoleID: "role-contract-admin", ScopeType: scopeTenant}}

	if err := service.validateItems(ctx, service.db, "tenant-1", "operator-1", items); err != nil {
		t.Fatalf("validateItems(平台超级管理员委派子系统角色) error = %v, want nil", err)
	}
	if containsQuery(scenario.queries, "count(distinct(") {
		t.Fatalf("平台超级管理员不应再走逐权限绑定计数：%v", scenario.queries)
	}
}

func TestValidateItemsAllowsPlatformSecurityAdminToDelegateSubsystemRoles(t *testing.T) {
	scenario := delegationRoleLookupDriverStub()
	scenario.rolePermissionIDs = []string{"contract-read", "contract-manage", "settlement-read"}
	scenario.operatorPermissionCount = 0 // 安全管理员没有逐应用的普通绑定
	service := newDelegationTestService(t, scenario)
	ctx := authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "operator-1"},
		Roles:  []authctx.ReferenceName{{Code: "platform-security-admin"}},
	})

	items := []TemplateRoleInput{{ApplicationID: "app-contract", RoleID: "role-contract-admin", ScopeType: scopeTenant}}

	if err := service.validateItems(ctx, service.db, "tenant-1", "operator-1", items); err != nil {
		t.Fatalf("validateItems(平台安全管理员委派子系统角色) error = %v, want nil", err)
	}
	if containsQuery(scenario.queries, "count(distinct(") {
		t.Fatalf("平台安全管理员不应再走逐权限绑定计数：%v", scenario.queries)
	}
}

func TestValidateItemsSkipsDelegationQueryForRoleWithoutPermissions(t *testing.T) {
	scenario := delegationRoleLookupDriverStub()
	service := newDelegationTestService(t, scenario)

	items := []TemplateRoleInput{{ApplicationID: "app-1", RoleID: "role-empty", ScopeType: scopeTenant}}

	if err := service.validateItems(context.Background(), service.db, "tenant-1", "operator-1", items); err != nil {
		t.Fatalf("validateItems(无权限角色) error = %v, want nil", err)
	}
	if containsQuery(scenario.queries, "authz_role_binding as binding") {
		t.Fatalf("无权限角色不应触发委派判定查询：%v", scenario.queries)
	}
}

func containsQuery(queries []string, fragment string) bool {
	for _, query := range queries {
		if strings.Contains(strings.ToLower(query), fragment) {
			return true
		}
	}
	return false
}
