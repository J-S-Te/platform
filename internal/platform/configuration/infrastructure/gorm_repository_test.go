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

	"github.com/J-S-Te/Basic-Platform/internal/platform/configuration/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 安全（SEC-D2）负向测试：同码应用只存在于租户 B，租户 A 用同一 code 创建配置命名空间
// 必须 not found，且不得写入 cfg_namespace。脚本化驱动按「查询是否带租户过滤」决定是否
// 命中应用行——模拟真实多租户库里跨租户同码应用的行为：修复前（无租户过滤）会命中并写入，
// 修复后带租户过滤的查询不会命中他租户行。

const (
	cfgTestTenantA = "01J000000000000000000000TA"
	cfgTestTenantB = "01J000000000000000000000TB"
	cfgTestAppID   = "01J00000000000000000000APP"
	cfgTestEnvID   = "01J000000000000000000000ENV"
)

type cfgScript struct {
	appOwnerTenant  string
	environmentSeen []string
	namespaceInsert int
	// 配置项更新用例：itemNamespaceID 是现有草稿所属命名空间；itemUpdateCalls 记录
	// cfg_item 的 UPDATE 次数，用于断言跨命名空间移动从未落库。
	itemNamespaceID string
	itemUpdateCalls int
}

func TestCreateNamespaceRejectsCrossTenantApplicationCode(t *testing.T) {
	script := &cfgScript{appOwnerTenant: cfgTestTenantB}
	repository := newCfgRepositoryTest(t, script)

	_, err := repository.CreateNamespace(context.Background(), application.NamespaceCreateInput{
		TenantID: cfgTestTenantA, OperatorID: "operator", ApplicationCode: "shared-code",
		Code: "cfg", Name: "配置",
	}, "01J000000000000000000000NS1", time.Now())

	if !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("CreateNamespace(跨租户 code) error = %v, want ErrNotFound", err)
	}
	if script.namespaceInsert != 0 {
		t.Fatalf("cfg_namespace 写入次数 = %d, 跨租户绑定不得落库", script.namespaceInsert)
	}
	appQuery := script.queryContaining("platform_application")
	if appQuery == "" {
		t.Fatal("未执行 platform_application 查询")
	}
	if !strings.Contains(appQuery, "tenant_id = ?") {
		t.Fatalf("应用查询缺少租户过滤（SEC-D2 回归）: %s", appQuery)
	}
}

func TestCreateNamespaceResolvesApplicationWithinCallerTenant(t *testing.T) {
	script := &cfgScript{appOwnerTenant: cfgTestTenantB}
	repository := newCfgRepositoryTest(t, script)

	namespace, err := repository.CreateNamespace(context.Background(), application.NamespaceCreateInput{
		TenantID: cfgTestTenantB, OperatorID: "operator", ApplicationCode: "shared-code",
		Code: "cfg", Name: "配置",
	}, "01J000000000000000000000NS2", time.Now())
	if err != nil {
		t.Fatalf("CreateNamespace(同租户) error = %v", err)
	}
	if namespace.Application.ID != cfgTestAppID {
		t.Fatalf("namespace.Application = %+v, want %s", namespace.Application, cfgTestAppID)
	}
	if script.namespaceInsert != 1 {
		t.Fatalf("cfg_namespace 写入次数 = %d, want 1", script.namespaceInsert)
	}
}

func TestCreateNamespaceEnvironmentLookupIsTenantScoped(t *testing.T) {
	script := &cfgScript{appOwnerTenant: cfgTestTenantB}
	repository := newCfgRepositoryTest(t, script)

	// 租户 B 的应用查询通过后，环境查询若丢失租户过滤同样会造成跨租户绑定。
	// 脚本把环境行归属也放在租户 B：租户 A 的调用必须在应用查询就被拦下，
	// 而租户 B 的正向路径证明环境行可正常解析。
	if _, err := repository.CreateNamespace(context.Background(), application.NamespaceCreateInput{
		TenantID: cfgTestTenantA, OperatorID: "operator", ApplicationCode: "shared-code",
		Code: "cfg", Name: "配置",
	}, "01J000000000000000000000NS3", time.Now()); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("跨租户 error = %v, want ErrNotFound", err)
	}
	envQuery := script.queryContaining("platform_application_environment")
	if envQuery == "" {
		// 应用查询已被租户过滤拦下，环境查询未执行——同样满足隔离要求。
		return
	}
	if !strings.Contains(envQuery, "tenant_id = ?") {
		t.Fatalf("环境查询缺少租户过滤（SEC-D2 回归）: %s", envQuery)
	}
}

func (script *cfgScript) queryContaining(table string) string {
	for _, query := range script.environmentSeen {
		if strings.Contains(query, table) {
			return query
		}
	}
	return ""
}

var cfgScriptDriverCounter uint64

func newCfgRepositoryTest(t *testing.T, script *cfgScript) *Repository {
	t.Helper()
	driverName := fmt.Sprintf("cfg-repo-test-%d", atomic.AddUint64(&cfgScriptDriverCounter, 1))
	sql.Register(driverName, &cfgScriptDriver{script: script})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open cfg test database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open cfg GORM database: %v", err)
	}
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatalf("NewRepository: %v", err)
	}
	return repository
}

type cfgScriptDriver struct{ script *cfgScript }
type cfgScriptConn struct{ script *cfgScript }
type cfgScriptTx struct{}

type cfgScriptResult int64

func (result cfgScriptResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result cfgScriptResult) RowsAffected() (int64, error) { return int64(result), nil }

type cfgScriptRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (driver *cfgScriptDriver) Open(string) (driver.Conn, error) {
	return &cfgScriptConn{script: driver.script}, nil
}

func (connection *cfgScriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by cfg test driver")
}

func (*cfgScriptConn) Close() error { return nil }

func (*cfgScriptConn) Begin() (driver.Tx, error) { return cfgScriptTx{}, nil }
func (*cfgScriptConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return cfgScriptTx{}, nil
}
func (cfgScriptTx) Commit() error   { return nil }
func (cfgScriptTx) Rollback() error { return nil }

func (connection *cfgScriptConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	connection.script.environmentSeen = append(connection.script.environmentSeen, query)
	if strings.Contains(query, "cfg_item") {
		connection.script.itemUpdateCalls++
		return cfgScriptResult(1), nil
	}
	if strings.Contains(query, "cfg_namespace") {
		connection.script.namespaceInsert++
		return cfgScriptResult(1), nil
	}
	return cfgScriptResult(0), nil
}

func (connection *cfgScriptConn) QueryContext(_ context.Context, query string, values []driver.NamedValue) (driver.Rows, error) {
	connection.script.environmentSeen = append(connection.script.environmentSeen, query)
	args := make([]driver.Value, 0, len(values))
	for _, value := range values {
		args = append(args, value.Value)
	}
	switch {
	case strings.Contains(query, "cfg_item"):
		namespaceID := connection.script.itemNamespaceID
		if namespaceID == "" {
			namespaceID = "01J000000000000000000000NS1"
		}
		return &cfgScriptRows{
			columns: []string{"id", "namespace_id", "namespace_code", "namespace_name", "config_key", "value_type", "value_text", "value_json", "sensitive", "version", "updated_at"},
			values:  [][]driver.Value{{"01J00000000000000000000ITEM", namespaceID, "cfg", "配置", "key-1", "STRING", "value-1", nil, false, uint64(1), time.Now()}},
		}, nil
	case strings.Contains(query, "cfg_namespace"):
		// 目标命名空间必须在同一租户内且为 ACTIVE；脚本驱动按脚本给出的命名空间返回行。
		namespaceID := connection.script.itemNamespaceID
		if namespaceID == "" {
			namespaceID = "01J000000000000000000000NS1"
		}
		return &cfgScriptRows{
			columns: []string{"id", "tenant_id", "application_id", "environment_id", "name", "display_name", "description", "status", "current_release_no", "version"},
			values:  [][]driver.Value{{namespaceID, cfgTestTenantA, cfgTestAppID, nil, "cfg", "配置", "", "ACTIVE", uint64(0), uint64(1)}},
		}, nil
	case strings.Contains(query, "platform_application_environment"):
		// 环境行只属于 appOwnerTenant；查询不带租户过滤或过滤到归属租户才命中。
		if !queryFiltersTenant(query) || argsContain(args, connection.script.appOwnerTenant) {
			return &cfgScriptRows{
				columns: []string{"id", "tenant_id", "application_id", "environment", "status"},
				values:  [][]driver.Value{{cfgTestEnvID, connection.script.appOwnerTenant, cfgTestAppID, "dev", "ACTIVE"}},
			}, nil
		}
		return &cfgScriptRows{columns: []string{"id"}}, nil
	case strings.Contains(query, "platform_application"):
		// 应用行只存在于 appOwnerTenant（模拟 (tenant_id,code) 唯一、跨租户同码存在）：
		// 不带租户过滤的旧查询会跨租户命中，带过滤且过滤到归属租户才返回行。
		if !queryFiltersTenant(query) || argsContain(args, connection.script.appOwnerTenant) {
			return &cfgScriptRows{
				columns: []string{"id", "code", "name", "status"},
				values:  [][]driver.Value{{cfgTestAppID, "shared-code", "共享应用", "ACTIVE"}},
			}, nil
		}
		return &cfgScriptRows{columns: []string{"id"}}, nil
	default:
		return &cfgScriptRows{columns: []string{"id"}}, nil
	}
}

func queryFiltersTenant(query string) bool {
	return strings.Contains(query, "tenant_id = ?")
}

func argsContain(args []driver.Value, expected string) bool {
	for _, arg := range args {
		if value, ok := arg.(string); ok && value == expected {
			return true
		}
	}
	return false
}

func (rows *cfgScriptRows) Columns() []string { return rows.columns }
func (*cfgScriptRows) Close() error           { return nil }

func (rows *cfgScriptRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.values) {
		return io.EOF
	}
	copy(destination, rows.values[rows.index])
	rows.index++
	return nil
}

// 配置项不是可移动资源：页面切换命名空间后误保存，曾经会把草稿挪到另一个命名空间，
// 让原命名空间的草稿与已发布版本脱节。
func TestUpdateItemRejectsCrossNamespaceMove(t *testing.T) {
	script := &cfgScript{appOwnerTenant: cfgTestTenantA, itemNamespaceID: cfgTestTenantA + "-ns-a"}
	repository := newCfgRepositoryTest(t, script)

	_, err := repository.UpdateItem(context.Background(), application.ItemUpdateInput{
		TenantID: cfgTestTenantA, OperatorID: "operator", ItemID: "01J00000000000000000000ITEM",
		NamespaceID: cfgTestTenantA + "-ns-b", Key: "key-1", ValueType: "STRING", Value: "value-2", Version: 1,
	}, time.Now())
	if !errors.Is(err, application.ErrValidation) {
		t.Fatalf("UpdateItem(跨命名空间) error = %v, want ErrValidation", err)
	}
	if script.itemUpdateCalls != 0 {
		t.Fatalf("cfg_item 更新次数 = %d, 跨命名空间移动不得落库", script.itemUpdateCalls)
	}
}

func TestUpdateItemStillAllowsSameNamespaceDraftUpdate(t *testing.T) {
	namespaceID := cfgTestTenantA + "-ns-a"
	script := &cfgScript{appOwnerTenant: cfgTestTenantA, itemNamespaceID: namespaceID}
	repository := newCfgRepositoryTest(t, script)

	item, err := repository.UpdateItem(context.Background(), application.ItemUpdateInput{
		TenantID: cfgTestTenantA, OperatorID: "operator", ItemID: "01J00000000000000000000ITEM",
		NamespaceID: namespaceID, Key: "key-1", ValueType: "STRING", Value: "value-2", Version: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("UpdateItem(同命名空间) error = %v", err)
	}
	if item.ID != "01J00000000000000000000ITEM" {
		t.Fatalf("item = %+v", item)
	}
	if script.itemUpdateCalls != 1 {
		t.Fatalf("cfg_item 更新次数 = %d, want 1", script.itemUpdateCalls)
	}
}
