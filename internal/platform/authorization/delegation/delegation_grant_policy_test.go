package delegation

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

	authapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 子系统 CRM 发布 customer.read.all（grant_policy=SUPER_ADMIN_ONLY）后，统一委派入口
// 必须把它限制在"租户级平台超级管理员"这一条授予来源上：
//   * 普通操作者即使逐项持有全部目标权限也不得转授；
//   * 岗位模板路径的 platform-security-admin 豁免同样不得绕过它——否则安全管理员
//     只要先拿到包含受限权限的角色就能把"仅超管可授"的权限扩散出去。
//
// 这里用脚本化 SQL 驱动跑生产代码路径，不连接外部数据库。

type grantPolicyScenario struct {
	// permissionIDs 是目标角色包含的 ALLOW 权限 ID。
	permissionIDs []string
	// restrictedCount 是其中 grant_policy=SUPER_ADMIN_ONLY 的命中数。
	restrictedCount int64
	// delegableCount 是操作者本人已有权限的命中数（VerifyPermissions 的结果）。
	delegableCount int64
	queries        []string
	// grantPolicyArgs 记录含 grant_policy 条件的查询绑定值。
	grantPolicyArgs []driver.NamedValue
}

type grantPolicyRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *grantPolicyRows) Columns() []string { return rows.columns }
func (*grantPolicyRows) Close() error           { return nil }

func (rows *grantPolicyRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

type grantPolicyResult struct{}

func (grantPolicyResult) LastInsertId() (int64, error) { return 1, nil }
func (grantPolicyResult) RowsAffected() (int64, error) { return 1, nil }

type grantPolicyTx struct{}

func (grantPolicyTx) Commit() error   { return nil }
func (grantPolicyTx) Rollback() error { return nil }

type grantPolicyDriver struct{ scenario *grantPolicyScenario }
type grantPolicyConn struct{ scenario *grantPolicyScenario }

func (d *grantPolicyDriver) Open(string) (driver.Conn, error) {
	return &grantPolicyConn{scenario: d.scenario}, nil
}
func (*grantPolicyConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared SQL")
}
func (*grantPolicyConn) Close() error              { return nil }
func (*grantPolicyConn) Begin() (driver.Tx, error) { return grantPolicyTx{}, nil }
func (*grantPolicyConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return grantPolicyTx{}, nil
}
func (*grantPolicyConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return grantPolicyResult{}, nil
}

func (c *grantPolicyConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.scenario.queries = append(c.scenario.queries, query)
	normalized := strings.ToLower(strings.ReplaceAll(query, "`", ""))
	switch {
	case strings.Contains(normalized, "grant_policy"):
		c.scenario.grantPolicyArgs = args
		return &grantPolicyRows{columns: []string{"count"}, values: [][]driver.Value{{c.scenario.restrictedCount}}}, nil
	case strings.Contains(normalized, "from authz_role_permission"):
		values := make([][]driver.Value, 0, len(c.scenario.permissionIDs))
		for _, id := range c.scenario.permissionIDs {
			values = append(values, []driver.Value{id})
		}
		return &grantPolicyRows{columns: []string{"permission_id"}, values: values}, nil
	case strings.Contains(normalized, "from authz_role_binding"):
		return &grantPolicyRows{columns: []string{"count"}, values: [][]driver.Value{{c.scenario.delegableCount}}}, nil
	case strings.Contains(normalized, "from platform_application"):
		return &grantPolicyRows{columns: []string{"code"}, values: [][]driver.Value{{"contract_management"}}}, nil
	default:
		return &grantPolicyRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	}
}

var grantPolicyDriverCounter uint64

func newGrantPolicyDatabase(t *testing.T, scenario *grantPolicyScenario) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("delegation-grant-policy-%d", atomic.AddUint64(&grantPolicyDriverCounter, 1))
	sql.Register(name, &grantPolicyDriver{scenario: scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open scripted delegation database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open scripted gorm database: %v", err)
	}
	return database
}

func restrictedGrantPolicyQueries(scenario *grantPolicyScenario) []string {
	matched := make([]string, 0, 1)
	for _, query := range scenario.queries {
		if strings.Contains(strings.ToLower(query), "grant_policy") {
			matched = append(matched, query)
		}
	}
	return matched
}

func TestVerifyRolePermissionsRejectsSuperAdminOnlyPermissionsForNonSuperAdmin(t *testing.T) {
	scenario := &grantPolicyScenario{
		permissionIDs:   []string{"permission-read-all", "permission-read-own"},
		restrictedCount: 1,
		delegableCount:  2,
	}
	database := newGrantPolicyDatabase(t, scenario)
	// 普通操作者本人确实持有这两个权限；拒绝只能来自授予策略守卫。
	ctx := principalContext("tenant-1", "operator-1", "platform-audit-admin")

	err := VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-crm", "role-customer-all-viewer")
	if err == nil || !errors.Is(err, authapplication.ErrForbidden) {
		t.Fatalf("super-administrator-only permissions must be forbidden for a non-super administrator, got %v", err)
	}
	if !strings.Contains(err.Error(), "only a platform super administrator may delegate") {
		t.Fatalf("rejection must explain the super administrator boundary, got %v", err)
	}
	for _, query := range scenario.queries {
		normalized := strings.ToLower(query)
		if strings.Contains(normalized, "authz_role_binding") {
			t.Fatalf("grant policy guard must reject before the per-permission delegation check: %s", query)
		}
	}
}

func TestVerifyRolePermissionsForPositionTemplatesRejectsSecurityAdminBypass(t *testing.T) {
	scenario := &grantPolicyScenario{
		permissionIDs:   []string{"permission-read-all"},
		restrictedCount: 1,
	}
	database := newGrantPolicyDatabase(t, scenario)
	// 岗位模板路径允许 platform-security-admin 委派本人未持有的普通权限，
	// 但 SUPER_ADMIN_ONLY 必须仍然只认租户级超级管理员。
	ctx := principalContext("tenant-1", "operator-1", domain.SecurityAdminRoleCode)

	err := VerifyRolePermissionsForPositionTemplates(ctx, database, "tenant-1", "operator-1", "app-crm", "role-customer-all-viewer")
	if err == nil || !errors.Is(err, authapplication.ErrForbidden) {
		t.Fatalf("security administrator bypass must not cover super-administrator-only permissions, got %v", err)
	}
	if len(restrictedGrantPolicyQueries(scenario)) == 0 {
		t.Fatal("grant policy guard must run before the position template bypass")
	}
}

func TestVerifyRolePermissionsSuperAdminShortCircuitsGrantPolicyQuery(t *testing.T) {
	scenario := &grantPolicyScenario{
		permissionIDs:   []string{"permission-read-all"},
		restrictedCount: 1,
	}
	database := newGrantPolicyDatabase(t, scenario)
	ctx := principalContext("tenant-1", "operator-1", domain.SuperAdminRoleCode)

	if err := VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-crm", "role-customer-all-viewer"); err != nil {
		t.Fatalf("super administrator must keep the delegation exception, got %v", err)
	}
	if queries := restrictedGrantPolicyQueries(scenario); len(queries) != 0 {
		t.Fatalf("super administrator path must not query grant_policy, got %#v", queries)
	}
	for _, query := range scenario.queries {
		if strings.Contains(strings.ToLower(query), "authz_role_binding") {
			t.Fatalf("super administrator must short-circuit before the per-permission delegation check: %s", query)
		}
	}
}

func TestVerifyRolePermissionsKeepsExistingSemanticsForDefaultGrantPolicy(t *testing.T) {
	t.Run("operator holding every permission is still allowed", func(t *testing.T) {
		scenario := &grantPolicyScenario{
			permissionIDs:   []string{"permission-read-own"},
			restrictedCount: 0,
			delegableCount:  1,
		}
		database := newGrantPolicyDatabase(t, scenario)
		ctx := principalContext("tenant-1", "operator-1", "platform-audit-admin")
		if err := VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-crm", "role-customer-viewer"); err != nil {
			t.Fatalf("DEFAULT grant policy must keep the existing delegation semantics, got %v", err)
		}
	})

	t.Run("operator missing the permission is still forbidden", func(t *testing.T) {
		scenario := &grantPolicyScenario{
			permissionIDs:   []string{"permission-read-own"},
			restrictedCount: 0,
			delegableCount:  0,
		}
		database := newGrantPolicyDatabase(t, scenario)
		ctx := principalContext("tenant-1", "operator-1", "platform-audit-admin")
		err := VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-crm", "role-customer-viewer")
		if err == nil || !errors.Is(err, authapplication.ErrForbidden) {
			t.Fatalf("missing delegated permissions must stay forbidden, got %v", err)
		}
	})
}

func TestGrantPolicyGuardKeepsTenantAndApplicationBoundaries(t *testing.T) {
	scenario := &grantPolicyScenario{
		permissionIDs:   []string{"permission-read-all", "permission-read-own"},
		restrictedCount: 1,
	}
	database := newGrantPolicyDatabase(t, scenario)
	ctx := principalContext("tenant-1", "operator-1", "platform-audit-admin")

	_ = VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-crm", "role-customer-all-viewer")

	queries := restrictedGrantPolicyQueries(scenario)
	if len(queries) != 1 {
		t.Fatalf("grant policy guard must run exactly one query, got %#v", queries)
	}
	sql := queries[0]
	for _, boundary := range []string{"tenant_id = ?", "application_id = ?", "id in (", "grant_policy = ?"} {
		if !strings.Contains(strings.ToLower(sql), boundary) {
			t.Fatalf("grant policy query lost %q: %s", boundary, sql)
		}
	}
	bindings := make([]string, 0, len(scenario.grantPolicyArgs))
	for _, arg := range scenario.grantPolicyArgs {
		if value, ok := arg.Value.(string); ok {
			bindings = append(bindings, value)
		}
	}
	for _, want := range []string{"tenant-1", "app-crm", domain.GrantPolicySuperAdminOnly, "permission-read-all", "permission-read-own"} {
		found := false
		for _, binding := range bindings {
			if binding == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("grant policy query did not bind %q: %#v", want, bindings)
		}
	}
}

func TestGrantPolicyGuardRejectsMissingPrincipal(t *testing.T) {
	scenario := &grantPolicyScenario{
		permissionIDs:   []string{"permission-read-all"},
		restrictedCount: 1,
	}
	database := newGrantPolicyDatabase(t, scenario)

	err := VerifyRolePermissions(context.Background(), database, "tenant-1", "operator-1", "app-crm", "role-customer-all-viewer")
	if err == nil || !errors.Is(err, authapplication.ErrForbidden) {
		t.Fatalf("unauthenticated caller must not delegate super-administrator-only permissions, got %v", err)
	}
}
