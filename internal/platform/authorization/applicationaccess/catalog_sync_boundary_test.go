package applicationaccess

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 安全审查 H-1：控制面应用（code=platform）的授权目录只能由 bootstrap 镜像维护。
// 若应用通道可以发布它，持 authorization.catalog.sync 的控制台客户端就能把
// platform-* 内置角色改写为 role_type=APPLICATION、built_in=0，或干脆在目录中
// 不声明它们从而被停用，最终取得任意 platform:* 权限。
//
// 这里用脚本化 SQL 驱动执行生产代码路径，不连接外部数据库：断言拒绝发生在任何
// 写入之前，且目录元数据也不会被写。

type catalogBoundaryScenario struct {
	applicationCode string
	roleBuiltIn     int
	roleType        string
	execs           []string
	queries         []string
}

type catalogBoundaryResult struct{}

func (catalogBoundaryResult) LastInsertId() (int64, error) { return 1, nil }
func (catalogBoundaryResult) RowsAffected() (int64, error) { return 1, nil }

type catalogBoundaryRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (rows *catalogBoundaryRows) Columns() []string { return rows.columns }
func (*catalogBoundaryRows) Close() error           { return nil }

func (rows *catalogBoundaryRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

type catalogBoundaryDriver struct{ scenario *catalogBoundaryScenario }
type catalogBoundaryConn struct{ scenario *catalogBoundaryScenario }
type catalogBoundaryTx struct{}

func (catalogBoundaryTx) Commit() error   { return nil }
func (catalogBoundaryTx) Rollback() error { return nil }

func (d *catalogBoundaryDriver) Open(string) (driver.Conn, error) {
	return &catalogBoundaryConn{scenario: d.scenario}, nil
}

func (*catalogBoundaryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepared SQL")
}
func (*catalogBoundaryConn) Close() error              { return nil }
func (*catalogBoundaryConn) Begin() (driver.Tx, error) { return catalogBoundaryTx{}, nil }
func (*catalogBoundaryConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return catalogBoundaryTx{}, nil
}

func (c *catalogBoundaryConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.scenario.execs = append(c.scenario.execs, query)
	return catalogBoundaryResult{}, nil
}

func (c *catalogBoundaryConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.scenario.queries = append(c.scenario.queries, query)
	normalized := strings.ToLower(strings.ReplaceAll(query, "`", ""))
	switch {
	case strings.Contains(normalized, "from platform_application"):
		return &catalogBoundaryRows{
			columns: []string{"id", "tenant_id", "code", "owner_user_id", "status"},
			values:  [][]driver.Value{{"app-1", "tenant-1", c.scenario.applicationCode, nil, "ACTIVE"}},
		}, nil
	case strings.Contains(normalized, "from authz_role_permission"):
		return &catalogBoundaryRows{columns: []string{"permission_id"}}, nil
	case strings.Contains(normalized, "from authz_role where"):
		return &catalogBoundaryRows{
			columns: []string{"id", "built_in", "role_type"},
			values:  [][]driver.Value{{"role-existing", int64(c.scenario.roleBuiltIn), c.scenario.roleType}},
		}, nil
	default:
		return &catalogBoundaryRows{columns: []string{"id"}}, nil
	}
}

type catalogBoundaryIDs struct{}

func (catalogBoundaryIDs) New(time.Time) (string, error) { return "generated-id", nil }

type catalogBoundaryClock struct{ now time.Time }

func (c catalogBoundaryClock) Now() time.Time { return c.now }

var catalogBoundaryDriverCounter uint64

func newCatalogBoundaryService(t *testing.T, scenario *catalogBoundaryScenario) *Service {
	t.Helper()
	name := fmt.Sprintf("appaccess-catalog-boundary-%d", atomic.AddUint64(&catalogBoundaryDriverCounter, 1))
	sql.Register(name, &catalogBoundaryDriver{scenario: scenario})
	sqlDB, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open scripted catalog database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing: true, SkipDefaultTransaction: true, Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open scripted gorm database: %v", err)
	}
	return &Service{db: database, ids: catalogBoundaryIDs{}, clock: catalogBoundaryClock{time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)}}
}

func platformCatalogSyncInput() CatalogInput {
	return CatalogInput{
		CatalogVersion: "2026.11.02",
		Permissions: []PermissionInput{{
			Code: "contract.read", Name: "查看合同", Action: "read", ResourceCode: "contract",
		}},
		Roles: []CatalogRoleInput{{
			Code: "platform-security-admin", Name: "平台安全管理员", Permissions: []string{"contract.read"},
		}},
	}
}

func assertNoCatalogWrites(t *testing.T, execs []string) {
	t.Helper()
	for _, statement := range execs {
		normalized := strings.ToLower(strings.ReplaceAll(statement, "`", ""))
		for _, forbidden := range []string{"insert into authz_role", "update authz_role", "authz_role_permission", "authz_authorization_catalog", "authz_permission", "authz_resource"} {
			if strings.Contains(normalized, forbidden) {
				t.Fatalf("rejected catalog synchronization still wrote %q: %s", forbidden, statement)
			}
		}
	}
}

func TestSyncCatalogRejectsPlatformApplicationBeforeAnyWrite(t *testing.T) {
	scenario := &catalogBoundaryScenario{applicationCode: PlatformApplicationCode}
	service := newCatalogBoundaryService(t, scenario)

	_, err := service.SyncCatalog(context.Background(), "tenant-1", "app-1", "operator-1", platformCatalogSyncInput())
	if err == nil || !errorsIs(err, ErrAccessDenied) {
		t.Fatalf("platform catalog sync must fail closed with ErrAccessDenied, got %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "bootstrap") {
		t.Fatalf("platform catalog rejection must explain bootstrap ownership, got %v", err)
	}
	if len(scenario.execs) != 0 {
		t.Fatalf("platform catalog sync executed writes: %#v", scenario.execs)
	}
	assertNoCatalogWrites(t, scenario.execs)

	// HTTP 映射：writeError 必须把它呈现为 403，而不是 400/500。
	recorder := httptest.NewRecorder()
	writeError(recorder, httptest.NewRequest("PUT", "/api/v1/applications/app-1/authorization-catalog", nil), err)
	if recorder.Code != 403 {
		t.Fatalf("platform catalog rejection mapped to HTTP %d, want 403", recorder.Code)
	}
}

func TestCatalogSyncTargetAllowedOnlyRejectsPlatformApplication(t *testing.T) {
	if err := catalogSyncTargetAllowed(applicationRow{ID: "app-1", Code: "contract_management"}); err != nil {
		t.Fatalf("ordinary application catalog must stay publishable: %v", err)
	}
	if err := catalogSyncTargetAllowed(applicationRow{ID: "app-2", Code: PlatformApplicationCode}); err == nil || !errorsIs(err, ErrAccessDenied) {
		t.Fatalf("platform application catalog must be rejected, got %v", err)
	}
}

func TestSyncCatalogRejectsRewritingBuiltInRoles(t *testing.T) {
	cases := []struct {
		name      string
		builtIn   int
		roleType  string
		forbidden string
	}{
		{name: "built_in flag", builtIn: 1, roleType: "APPLICATION", forbidden: "built-in"},
		{name: "platform role type", builtIn: 0, roleType: "PLATFORM", forbidden: "built-in"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scenario := &catalogBoundaryScenario{applicationCode: "contract_management", roleBuiltIn: testCase.builtIn, roleType: testCase.roleType}
			service := newCatalogBoundaryService(t, scenario)

			_, err := service.SyncCatalog(context.Background(), "tenant-1", "app-1", "operator-1", platformCatalogSyncInput())
			if err == nil || !errorsIs(err, ErrValidation) {
				t.Fatalf("catalog sync rewriting a built-in role must fail validation, got %v", err)
			}
			if !strings.Contains(err.Error(), testCase.forbidden) {
				t.Fatalf("built-in role rejection must name the protected role, got %v", err)
			}
			// 事务已回滚；关键断言是任何环节都没有改写内置角色本身。
			for _, statement := range scenario.execs {
				normalized := strings.ToLower(strings.ReplaceAll(statement, "`", ""))
				if strings.Contains(normalized, "authz_role") && (strings.HasPrefix(normalized, "insert") || strings.HasPrefix(normalized, "update")) {
					t.Fatalf("built-in role was rewritten by catalog synchronization: %s", statement)
				}
			}
		})
	}
}

func TestRemovedCatalogRolesDisableQuerySkipsBuiltInRoles(t *testing.T) {
	database, err := gorm.Open(mysql.New(mysql.Config{
		DSN: "test:test@tcp(localhost:3306)/test?parseTime=true", SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	statement := removedCatalogRolesQuery(database, "tenant-1", "app-1", []string{"sales"}).
		Updates(map[string]any{"status": disabledStatus}).Statement
	sql := statement.SQL.String()
	if !strings.Contains(sql, "built_in = ?") {
		t.Fatalf("removed-role disable query must constrain built_in, got %s", sql)
	}
	foundZero := false
	for _, variable := range statement.Vars {
		if value, ok := variable.(int); ok && value == 0 {
			foundZero = true
		}
		if value, ok := variable.(int64); ok && value == 0 {
			foundZero = true
		}
	}
	if !foundZero {
		t.Fatalf("removed-role disable query must bind built_in = 0, got vars %#v", statement.Vars)
	}
	if !strings.Contains(sql, "role_type <> ?") || !strings.Contains(sql, "code NOT IN") {
		t.Fatalf("removed-role disable query lost its existing boundaries: %s", sql)
	}
}
