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
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 安全审查 H-1 的第二条路径：零权限角色的委派检查曾经直接返回成功。
// 在控制面应用上，这意味着任何人（例如只持有角色绑定权限的安全管理员）都能
// 绑定一个当前没有权限的角色；目录同步随后可以给该角色重新挂上权限，从而绕过
// “只能委派本人已有权限”。控制面应用必须失败关闭，非控制面应用保持既有语义，
// 显式的超管/岗位模板 bypass 保持原行为。

type delegationScopeScenario struct {
	applicationCode string
	queries         []string
}

type delegationScopeRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *delegationScopeRows) Columns() []string { return rows.columns }
func (*delegationScopeRows) Close() error           { return nil }

func (rows *delegationScopeRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

type delegationScopeResult struct{}

func (delegationScopeResult) LastInsertId() (int64, error) { return 1, nil }
func (delegationScopeResult) RowsAffected() (int64, error) { return 1, nil }

type delegationScopeDriver struct{ scenario *delegationScopeScenario }
type delegationScopeConn struct{ scenario *delegationScopeScenario }
type delegationScopeTx struct{}

func (delegationScopeTx) Commit() error   { return nil }
func (delegationScopeTx) Rollback() error { return nil }

func (d *delegationScopeDriver) Open(string) (driver.Conn, error) {
	return &delegationScopeConn{scenario: d.scenario}, nil
}
func (*delegationScopeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared SQL")
}
func (*delegationScopeConn) Close() error              { return nil }
func (*delegationScopeConn) Begin() (driver.Tx, error) { return delegationScopeTx{}, nil }
func (*delegationScopeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return delegationScopeTx{}, nil
}
func (*delegationScopeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return delegationScopeResult{}, nil
}
func (c *delegationScopeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.scenario.queries = append(c.scenario.queries, query)
	normalized := strings.ToLower(strings.ReplaceAll(query, "`", ""))
	switch {
	case strings.Contains(normalized, "from authz_role_permission"):
		// 目标角色没有任何 ALLOW 权限。
		return &delegationScopeRows{columns: []string{"permission_id"}}, nil
	case strings.Contains(normalized, "from platform_application"):
		return &delegationScopeRows{
			columns: []string{"code"},
			values:  [][]driver.Value{{c.scenario.applicationCode}},
		}, nil
	default:
		return &delegationScopeRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	}
}

var delegationScopeDriverCounter uint64

func newDelegationScopeDatabase(t *testing.T, scenario *delegationScopeScenario) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("delegation-scope-%d", atomic.AddUint64(&delegationScopeDriverCounter, 1))
	sql.Register(name, &delegationScopeDriver{scenario: scenario})
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

func platformApplicationQueried(scenario *delegationScopeScenario) bool {
	for _, query := range scenario.queries {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(query, "`", "")), "from platform_application") {
			return true
		}
	}
	return false
}

func TestZeroPermissionRoleOnPlatformApplicationFailsClosed(t *testing.T) {
	scenario := &delegationScopeScenario{applicationCode: platformApplicationCode}
	database := newDelegationScopeDatabase(t, scenario)
	ctx := principalContext("tenant-1", "operator-1", "platform-security-admin")

	err := VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-platform", "role-without-permissions")
	if err == nil || !errors.Is(err, authapplication.ErrForbidden) {
		t.Fatalf("zero-permission platform role must be forbidden for a non-super administrator, got %v", err)
	}
	if !platformApplicationQueried(scenario) {
		t.Fatal("platform application identity must be resolved before allowing a zero-permission role")
	}
	for _, query := range scenario.queries {
		if strings.Contains(strings.ToLower(query), "from platform_application") && !strings.Contains(query, "tenant_id = ?") {
			t.Fatalf("platform application lookup lost its tenant boundary: %s", query)
		}
	}
}

func TestZeroPermissionRoleOnTenantApplicationKeepsExistingSemantics(t *testing.T) {
	scenario := &delegationScopeScenario{applicationCode: "contract_management"}
	database := newDelegationScopeDatabase(t, scenario)
	ctx := principalContext("tenant-1", "operator-1", "platform-security-admin")

	if err := VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-contract", "role-without-permissions"); err != nil {
		t.Fatalf("zero-permission role on a tenant application must stay allowed, got %v", err)
	}
}

func TestZeroPermissionRoleOnPlatformApplicationKeepsExplicitBypasses(t *testing.T) {
	t.Run("super administrator", func(t *testing.T) {
		scenario := &delegationScopeScenario{applicationCode: platformApplicationCode}
		database := newDelegationScopeDatabase(t, scenario)
		ctx := principalContext("tenant-1", "operator-1", "platform-super-admin")
		if err := VerifyRolePermissions(ctx, database, "tenant-1", "operator-1", "app-platform", "role-without-permissions"); err != nil {
			t.Fatalf("super administrator bypass must be preserved, got %v", err)
		}
		if platformApplicationQueried(scenario) {
			t.Fatal("super administrator bypass must short-circuit before the platform application lookup")
		}
	})

	t.Run("position template security administrator", func(t *testing.T) {
		scenario := &delegationScopeScenario{applicationCode: platformApplicationCode}
		database := newDelegationScopeDatabase(t, scenario)
		ctx := principalContext("tenant-1", "operator-1", "platform-security-admin")
		if err := VerifyRolePermissionsForPositionTemplates(ctx, database, "tenant-1", "operator-1", "app-platform", "role-without-permissions"); err != nil {
			t.Fatalf("position template bypass must be preserved, got %v", err)
		}
	})
}

func TestZeroPermissionRoleOnPlatformApplicationRejectsUnauthenticatedPrincipal(t *testing.T) {
	scenario := &delegationScopeScenario{applicationCode: platformApplicationCode}
	database := newDelegationScopeDatabase(t, scenario)
	if err := VerifyRolePermissions(context.Background(), database, "tenant-1", "operator-1", "app-platform", "role-without-permissions"); err == nil || !errors.Is(err, authapplication.ErrForbidden) {
		t.Fatalf("missing authenticated principal must not delegate a platform role, got %v", err)
	}
}
