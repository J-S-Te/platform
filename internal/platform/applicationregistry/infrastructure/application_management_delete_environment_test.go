package infrastructure

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

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 安全（SEC-D2）负向测试：历史跨租户残留的 cfg_namespace 行（tenant_id 与调用方不同，
// 但 application_id+environment_id 指向被删环境）同样会以 FK RESTRICT 阻塞删除。
// 前置检查若仍按调用方租户过滤，这类残留会让删除一路走到数据库 FK 才报原始错误；
// 修复后必须在前置检查即返回 ErrEnvironmentDeletionBlocked，且不执行任何 DELETE。

const (
	deleteEnvTestTenant = "01J000000000000000000000TA"
	deleteEnvTestApp    = "01J00000000000000000000APP"
	deleteEnvTestEnv    = "01J00000000000000000000ENV"
	deleteEnvTestVer    = uint64(3)
)

type deleteEnvScript struct {
	queries []string
}

func (script *deleteEnvScript) record(query string) { script.queries = append(script.queries, query) }

func (script *deleteEnvScript) hasPrefix(prefix string) bool {
	for _, query := range script.queries {
		if strings.HasPrefix(strings.TrimSpace(strings.ToLower(query)), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

func (script *deleteEnvScript) find(fragment string) string {
	for _, query := range script.queries {
		if strings.Contains(query, fragment) {
			return query
		}
	}
	return ""
}

func TestDeleteEnvironmentBlocksOnCrossTenantNamespaceResidue(t *testing.T) {
	script := &deleteEnvScript{}
	repository := newDeleteEnvRepositoryTest(t, script)

	_, err := repository.DeleteEnvironment(context.Background(), application.EnvironmentDeleteInput{
		TenantID: deleteEnvTestTenant, ApplicationID: deleteEnvTestApp, EnvironmentID: deleteEnvTestEnv,
		Version: deleteEnvTestVer, OperatorID: "operator",
	})
	if !errors.Is(err, application.ErrEnvironmentDeletionBlocked) {
		t.Fatalf("DeleteEnvironment(跨租户残留) error = %v, want ErrEnvironmentDeletionBlocked; queries=%v", err, script.queries)
	}
	if script.hasPrefix("delete") {
		t.Fatalf("残留未被前置检查拦截时执行了 DELETE: %v", script.queries)
	}
	countQuery := script.find("cfg_namespace")
	if countQuery == "" {
		t.Fatal("未执行 cfg_namespace 残留检查")
	}
	if strings.Contains(countQuery, "tenant_id") {
		t.Fatalf("cfg_namespace 残留检查仍按调用方租户过滤，跨租户残留会漏检（SEC-D2 回归）: %s", countQuery)
	}
}

func TestDeleteEnvironmentProceedsWithoutAnyResidue(t *testing.T) {
	script := &deleteEnvScript{}
	repository := newDeleteEnvRepositoryWithResidue(t, script, true)

	removed, err := repository.DeleteEnvironment(context.Background(), application.EnvironmentDeleteInput{
		TenantID: deleteEnvTestTenant, ApplicationID: deleteEnvTestApp, EnvironmentID: deleteEnvTestEnv,
		Version: deleteEnvTestVer, OperatorID: "operator",
	})
	if err != nil {
		t.Fatalf("DeleteEnvironment(无残留) error = %v; queries=%v", err, script.queries)
	}
	if removed.ID != deleteEnvTestEnv {
		t.Fatalf("removed = %+v, want %s", removed, deleteEnvTestEnv)
	}
}

var deleteEnvScriptDriverCounter uint64

// residueFree 控制 cfg_namespace/audit_ingestion_receipt 的 count 返回值。
type deleteEnvScriptDriver struct {
	script      *deleteEnvScript
	residueFree bool
}

func newDeleteEnvRepositoryTest(t *testing.T, script *deleteEnvScript) *ManagementRepository {
	t.Helper()
	return newDeleteEnvRepositoryWithResidue(t, script, false)
}

func newDeleteEnvRepositoryWithResidue(t *testing.T, script *deleteEnvScript, residueFree bool) *ManagementRepository {
	t.Helper()
	driverName := fmt.Sprintf("env-delete-test-%d", atomic.AddUint64(&deleteEnvScriptDriverCounter, 1))
	sql.Register(driverName, &deleteEnvScriptDriver{script: script, residueFree: residueFree})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open env delete test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open env delete GORM database: %v", err)
	}
	repository, err := NewManagementRepository(database)
	if err != nil {
		t.Fatalf("NewManagementRepository: %v", err)
	}
	return repository
}

type deleteEnvConn struct {
	driver *deleteEnvScriptDriver
}
type deleteEnvTx struct{}

type deleteEnvResult int64

func (result deleteEnvResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result deleteEnvResult) RowsAffected() (int64, error) { return int64(result), nil }

type deleteEnvRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (driver *deleteEnvScriptDriver) Open(string) (driver.Conn, error) {
	return &deleteEnvConn{driver: driver}, nil
}

func (connection *deleteEnvConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by env delete test driver")
}

func (*deleteEnvConn) Close() error { return nil }

func (*deleteEnvConn) Begin() (driver.Tx, error) { return deleteEnvTx{}, nil }
func (*deleteEnvConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return deleteEnvTx{}, nil
}
func (deleteEnvTx) Commit() error   { return nil }
func (deleteEnvTx) Rollback() error { return nil }

func (connection *deleteEnvConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	connection.driver.script.record(query)
	// 前置检查放行后才可能到达 DELETE；无残留正向用例允许删除执行。
	if strings.HasPrefix(strings.TrimSpace(strings.ToLower(query)), "delete") && connection.driver.residueFree {
		return deleteEnvResult(1), nil
	}
	return deleteEnvResult(0), nil
}

func (connection *deleteEnvConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	connection.driver.script.record(query)
	lower := strings.ToLower(query)
	switch {
	case strings.Contains(lower, "count(") && strings.Contains(lower, "cfg_namespace"):
		// 残留行存在，但其 tenant_id 不属于调用方：只有不带租户过滤的检查才会计入。
		if strings.Contains(query, "tenant_id") || connection.driver.residueFree {
			return &deleteEnvRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
		}
		return &deleteEnvRows{columns: []string{"count"}, values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(lower, "count(") && strings.Contains(lower, "audit_ingestion_receipt"):
		return &deleteEnvRows{columns: []string{"count"}, values: [][]driver.Value{{int64(0)}}}, nil
	case strings.Contains(lower, "platform_application_environment"):
		return &deleteEnvRows{
			columns: []string{"id", "tenant_id", "application_id", "environment", "status", "version"},
			values:  [][]driver.Value{{deleteEnvTestEnv, deleteEnvTestTenant, deleteEnvTestApp, "prod", "ACTIVE", int64(deleteEnvTestVer)}},
		}, nil
	default:
		// 其余查询（oauth client pluck 等）一律返回空结果集。
		return &deleteEnvRows{columns: []string{"id"}}, nil
	}
}

func (rows *deleteEnvRows) Columns() []string { return rows.columns }
func (*deleteEnvRows) Close() error           { return nil }

func (rows *deleteEnvRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}
