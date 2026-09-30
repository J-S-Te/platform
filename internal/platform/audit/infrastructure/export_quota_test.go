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
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 安全审查 AUD-2026-013 配额回归：每租户同时最多 maxActiveAuditExportJobsPerTenant 个
// 活跃（PENDING/RUNNING）审计导出任务——第 4 个任务必须被 409 拒绝且不落库，
// 任务完成（活跃数回落）后即可再次创建。脚本化 SQL 驱动按「查询是否带租户过滤」
// 计数，保证配额统计严格限定在调用方租户内（沿用 configuration 仓储的测试模式）。
const quotaTestTenant = "01J000000000000000000000TQ"

type exportQuotaScript struct {
	// counts 是活跃任务数的应答序列，按 count 查询次序消费。
	counts []int64
	count  int
	// queries 记录全部 SQL 与参数，供租户过滤断言。
	queries []string
	args    [][]driver.Value
	inserts int
}

func (script *exportQuotaScript) nextCount() (int64, bool) {
	if script.count >= len(script.counts) {
		return 0, false
	}
	value := script.counts[script.count]
	script.count++
	return value, true
}

var exportQuotaDriverCounter uint64

func newExportQuotaRepository(t *testing.T, script *exportQuotaScript) *Repository {
	t.Helper()
	driverName := fmt.Sprintf("audit-quota-test-%d", atomic.AddUint64(&exportQuotaDriverCounter, 1))
	sql.Register(driverName, &exportQuotaScriptDriver{script: script})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open audit quota test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open audit quota GORM database: %v", err)
	}
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	return repository
}

func (script *exportQuotaScript) createExportJob(t *testing.T, repository *Repository, publicID string) error {
	t.Helper()
	_, err := repository.CreateExportJob(context.Background(), quotaTestTenant, "operator-1", application.PageRequest{Page: 1, PageSize: 20}, publicID, time.Now())
	return err
}

func TestCreateExportJobEnforcesPerTenantActiveQuota(t *testing.T) {
	script := &exportQuotaScript{counts: []int64{0, 3, 2}}
	repository := newExportQuotaRepository(t, script)

	// 第 1 个任务：无活跃任务，创建成功。
	if err := script.createExportJob(t, repository, "export-job-1"); err != nil {
		t.Fatalf("CreateExportJob(first) error = %v", err)
	}
	// 第 4 个任务：3 个 PENDING/RUNNING 任务已占用全部配额，必须 409 拒绝且不落库。
	if err := script.createExportJob(t, repository, "export-job-4"); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("CreateExportJob(over quota) error = %v, want ErrConflict", err)
	}
	// 任务完成（活跃数回落到 2）后可再次创建。
	if err := script.createExportJob(t, repository, "export-job-5"); err != nil {
		t.Fatalf("CreateExportJob(after completion) error = %v", err)
	}
	if script.inserts != 2 {
		t.Fatalf("async_job insert count = %d, want 2（超限任务不得落库）", script.inserts)
	}
}

func TestCreateExportJobQuotaQueryIsTenantScopedAndStatusBound(t *testing.T) {
	script := &exportQuotaScript{counts: []int64{0}}
	repository := newExportQuotaRepository(t, script)
	if err := script.createExportJob(t, repository, "export-job-1"); err != nil {
		t.Fatalf("CreateExportJob error = %v", err)
	}
	var countQuery string
	var countArgs []driver.Value
	for index, query := range script.queries {
		if strings.Contains(query, "async_job") && strings.Contains(strings.ToLower(query), "count(") {
			countQuery = query
			countArgs = script.args[index]
			break
		}
	}
	if countQuery == "" {
		t.Fatal("CreateExportJob must count active export jobs before inserting")
	}
	if !strings.Contains(countQuery, "tenant_id = ?") {
		t.Fatalf("quota count query is not tenant scoped: %s", countQuery)
	}
	joined := make([]string, 0, len(countArgs))
	for _, value := range countArgs {
		if text, ok := value.(string); ok {
			joined = append(joined, text)
		}
	}
	for _, expected := range []string{quotaTestTenant, "AUDIT_EXPORT", "PENDING", "RUNNING"} {
		found := false
		for _, value := range joined {
			if value == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("quota count query args %v missing %q", joined, expected)
		}
	}
}

// —— 脚本化 SQL 驱动：与 configuration 仓储测试同一模式。 ——

type exportQuotaScriptDriver struct{ script *exportQuotaScript }
type exportQuotaScriptConn struct{ script *exportQuotaScript }
type exportQuotaScriptTx struct{}
type exportQuotaScriptResult int64

func (result exportQuotaScriptResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result exportQuotaScriptResult) RowsAffected() (int64, error) { return int64(result), nil }

type exportQuotaScriptRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (driver *exportQuotaScriptDriver) Open(string) (driver.Conn, error) {
	return &exportQuotaScriptConn{script: driver.script}, nil
}

func (*exportQuotaScriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by audit quota test driver")
}

func (*exportQuotaScriptConn) Close() error { return nil }
func (*exportQuotaScriptConn) Begin() (driver.Tx, error) {
	return exportQuotaScriptTx{}, nil
}
func (*exportQuotaScriptConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return exportQuotaScriptTx{}, nil
}
func (exportQuotaScriptTx) Commit() error   { return nil }
func (exportQuotaScriptTx) Rollback() error { return nil }

func (connection *exportQuotaScriptConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "async_job") {
		connection.script.inserts++
		return exportQuotaScriptResult(1), nil
	}
	return exportQuotaScriptResult(0), nil
}

func (connection *exportQuotaScriptConn) QueryContext(_ context.Context, query string, values []driver.NamedValue) (driver.Rows, error) {
	args := make([]driver.Value, 0, len(values))
	for _, value := range values {
		args = append(args, value.Value)
	}
	connection.script.queries = append(connection.script.queries, query)
	connection.script.args = append(connection.script.args, args)
	switch {
	case strings.Contains(query, "platform_application"):
		return &exportQuotaScriptRows{
			columns: []string{"id", "tenant_id", "code", "name", "status"},
			values:  [][]driver.Value{{"01J00000000000000000000APP", quotaTestTenant, "platform", "基础平台", "ACTIVE"}},
		}, nil
	case strings.Contains(query, "async_job") && strings.Contains(strings.ToLower(query), "count("):
		count, ok := connection.script.nextCount()
		if !ok {
			return &exportQuotaScriptRows{columns: []string{"count(*)"}, values: [][]driver.Value{{int64(0)}}}, nil
		}
		return &exportQuotaScriptRows{columns: []string{"count(*)"}, values: [][]driver.Value{{count}}}, nil
	default:
		return &exportQuotaScriptRows{columns: []string{"id"}}, nil
	}
}

func (rows *exportQuotaScriptRows) Columns() []string { return rows.columns }
func (rows *exportQuotaScriptRows) Close() error      { return nil }
func (rows *exportQuotaScriptRows) Next(dest []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(dest, rows.values[rows.index])
	rows.index++
	return nil
}
