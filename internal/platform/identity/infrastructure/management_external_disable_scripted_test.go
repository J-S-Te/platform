package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 外部提供方停用对账（IAM-02 对齐）：DisableExternalIdentity 必须在同一事务内执行完整撤销，
// 而不只是把 iam_session 置 REVOKED。下面用脚本化本地驱动回放一次对账，断言实际下发的语句
// 覆盖 OAuth 刷新令牌族/刷新令牌、未消费授权码与平台会话，且全部落在同一事务里；
// 不连接任何数据库。

type scriptedRowSource struct {
	fragment string
	columns  []string
	rows     [][]driver.Value
}

type scriptedStatement struct {
	query string
	args  []driver.NamedValue
}

type externalDisableScript struct {
	mutex      sync.Mutex
	rowSources []scriptedRowSource
	queries    []scriptedStatement
	executed   []scriptedStatement
	begins     int
	commits    int
}

func (script *externalDisableScript) recordQuery(query string, args []driver.NamedValue) {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	script.queries = append(script.queries, scriptedStatement{query: query, args: append([]driver.NamedValue(nil), args...)})
}

func (script *externalDisableScript) recordExec(query string, args []driver.NamedValue) {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	script.executed = append(script.executed, scriptedStatement{query: query, args: append([]driver.NamedValue(nil), args...)})
}

func (script *externalDisableScript) rowsFor(query string) scriptedRowSource {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	for _, source := range script.rowSources {
		if strings.Contains(query, source.fragment) {
			return source
		}
	}
	// 未预置结果集的查询一律“查不到”：包括 iam_bootstrap_state、活跃会话与 Keycloak 投影目标。
	return scriptedRowSource{}
}

func (script *externalDisableScript) executedMatching(fragment string) []scriptedStatement {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	matched := make([]scriptedStatement, 0, 1)
	for _, statement := range script.executed {
		if strings.Contains(statement.query, fragment) {
			matched = append(matched, statement)
		}
	}
	return matched
}

func (script *externalDisableScript) transactionCounts() (int, int) {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	return script.begins, script.commits
}

func scriptedStatementArguments(statement scriptedStatement) string {
	values := make([]string, 0, len(statement.args))
	for _, argument := range statement.args {
		values = append(values, fmt.Sprintf("%v", argument.Value))
	}
	return strings.Join(values, "|")
}

type externalDisableDriver struct{ script *externalDisableScript }
type externalDisableConn struct{ script *externalDisableScript }
type externalDisableTx struct{ script *externalDisableScript }
type externalDisableResult int64

func (result externalDisableResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result externalDisableResult) RowsAffected() (int64, error) { return int64(result), nil }

func (driver *externalDisableDriver) Open(string) (driver.Conn, error) {
	return &externalDisableConn{script: driver.script}, nil
}
func (connection *externalDisableConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not supported by the external disable script driver")
}
func (*externalDisableConn) Close() error              { return nil }
func (*externalDisableConn) Begin() (driver.Tx, error) { return nil, errors.New("use BeginTx") }
func (connection *externalDisableConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	connection.script.mutex.Lock()
	connection.script.begins++
	connection.script.mutex.Unlock()
	return &externalDisableTx{script: connection.script}, nil
}
func (transaction *externalDisableTx) Commit() error {
	transaction.script.mutex.Lock()
	transaction.script.commits++
	transaction.script.mutex.Unlock()
	return nil
}
func (*externalDisableTx) Rollback() error { return nil }
func (connection *externalDisableConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	connection.script.recordExec(query, args)
	return externalDisableResult(1), nil
}
func (connection *externalDisableConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	connection.script.recordQuery(query, args)
	source := connection.script.rowsFor(query)
	return &externalDisableRows{columns: source.columns, rows: source.rows}, nil
}

type externalDisableRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (rows *externalDisableRows) Columns() []string { return rows.columns }
func (rows *externalDisableRows) Close() error      { return nil }
func (rows *externalDisableRows) Next(destination []driver.Value) error {
	if rows.index >= len(rows.rows) {
		return io.EOF
	}
	copy(destination, rows.rows[rows.index])
	rows.index++
	return nil
}

var externalDisableDriverCounter uint64

func newExternalDisableScriptedDatabase(t *testing.T, script *externalDisableScript) *gorm.DB {
	t.Helper()
	driverName := fmt.Sprintf("external-disable-script-%d", externalDisableDriverCounter)
	externalDisableDriverCounter++
	sql.Register(driverName, &externalDisableDriver{script: script})
	sqlDatabase, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open scripted external disable database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	database, err := gorm.Open(mysql.New(mysql.Config{Conn: sqlDatabase, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open scripted external disable GORM database: %v", err)
	}
	return database
}

// 外部停用对账必须撤销账号派生的 OAuth 派生物，并在同一事务内提交。
func TestExternalDisableRevokesOAuthDerivationsInOneTransaction(t *testing.T) {
	script := &externalDisableScript{rowSources: []scriptedRowSource{
		{fragment: "FROM `iam_user`", columns: []string{"id", "tenant_id", "status"}, rows: [][]driver.Value{{"user-1", "tenant-1", "ACTIVE"}}},
		{fragment: "FROM `iam_account`", columns: []string{"id", "tenant_id", "user_id", "status"}, rows: [][]driver.Value{{"account-1", "tenant-1", "user-1", "ACTIVE"}}},
		// 非 default 租户没有部署初始化管理员，bootstrap 保护不应介入。
		{fragment: "FROM `iam_tenant`", columns: []string{"code"}, rows: [][]driver.Value{{"external-disable-tenant"}}},
		{fragment: "FROM `oauth_token_family`", columns: []string{"id"}, rows: [][]driver.Value{{"family-1"}}},
	}}
	database := newExternalDisableScriptedDatabase(t, script)
	repository := &GORMRepository{database: database}

	if err := repository.DisableExternalIdentity(context.Background(), "tenant-1", "user-1"); err != nil {
		t.Fatalf("DisableExternalIdentity() error = %v", err)
	}

	begins, commits := script.transactionCounts()
	if begins != 1 || commits != 1 {
		t.Fatalf("external disable must commit exactly one transaction; begins=%d commits=%d", begins, commits)
	}

	assertExecuted := func(t *testing.T, fragment string, required ...string) {
		t.Helper()
		statements := script.executedMatching(fragment)
		if len(statements) == 0 {
			t.Fatalf("no statement executed for %q", fragment)
		}
		for _, statement := range statements {
			arguments := scriptedStatementArguments(statement)
			missing := make([]string, 0, len(required))
			for _, value := range required {
				if !strings.Contains(arguments, value) {
					missing = append(missing, value)
				}
			}
			if len(missing) == 0 {
				return
			}
		}
		t.Fatalf("statement %q never carried %v; executed = %+v", fragment, required, script.executedMatching(fragment))
	}

	// 完整撤销：刷新令牌族、刷新令牌、未消费授权码与平台会话都必须按外部停用原因失效。
	assertExecuted(t, "UPDATE `oauth_token_family`", "REVOKED", externalAccountDisabledReason)
	assertExecuted(t, "UPDATE `oauth_refresh_token`", "REVOKED", externalAccountDisabledReason)
	assertExecuted(t, "UPDATE `oauth_authorization_code`", "REVOKED")
	assertExecuted(t, "UPDATE `iam_session`", "REVOKED", externalAccountDisabledReason)
	// 既有停用语义不变：用户与账号仍然被停用。
	assertExecuted(t, "UPDATE `iam_user`", "DISABLED")
	assertExecuted(t, "UPDATE `iam_account`", "DISABLED")
}

// 对每个受影响的账号都必须独立执行完整撤销：一个用户可能挂多个账号，不能只处理第一个。
func TestExternalDisableRevokesEveryAccount(t *testing.T) {
	database, capture := newStatementCapturingDryRunDatabase(t)
	repository := &GORMRepository{database: database}
	revokedAt := time.Date(2026, time.October, 30, 9, 0, 0, 0, time.UTC)

	if err := repository.revokeExternallyDisabledAccountSessionsTx(database, "tenant-1", []string{"account-1", "account-2"}, revokedAt); err != nil {
		t.Fatalf("revokeExternallyDisabledAccountSessionsTx() error = %v", err)
	}

	statements := capture.all()
	for _, accountID := range []string{"account-1", "account-2"} {
		scoped := statementsContaining(statements, "account_id = '"+accountID+"'")
		if len(statementsContaining(scoped, "UPDATE `oauth_token_family`")) == 0 {
			t.Fatalf("account %s token families were not revoked; statements = %v", accountID, scoped)
		}
		if len(statementsContaining(scoped, "UPDATE `iam_session`")) == 0 {
			t.Fatalf("account %s sessions were not revoked; statements = %v", accountID, scoped)
		}
		if len(statementsContaining(scoped, "oauth_authorization_code")) == 0 {
			t.Fatalf("account %s authorization codes were not revoked; statements = %v", accountID, scoped)
		}
		if len(statementsContaining(scoped, externalAccountDisabledReason)) == 0 {
			t.Fatalf("account %s revocation lost the external disable reason; statements = %v", accountID, scoped)
		}
	}
}
