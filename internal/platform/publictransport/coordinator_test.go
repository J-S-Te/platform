package publictransport

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestCanonicalOriginRejectsPathAndSchemeMismatch(t *testing.T) {
	if got, err := canonicalOrigin("https://platform.example.com/", "https"); err != nil || got != "https://platform.example.com" {
		t.Fatalf("canonical origin = %q, %v", got, err)
	}
	for _, value := range []string{"http://platform.example.com", "https://platform.example.com/path", "https://user@platform.example.com", "platform.example.com"} {
		if _, err := canonicalOrigin(value, "https"); err == nil {
			t.Fatalf("origin %q must be rejected", value)
		}
	}
}

func TestReplaceOriginDoesNotRewriteAttackerPrefix(t *testing.T) {
	source := "http://platform.example.com"
	target := "https://platform.example.com"
	if got, ok := replaceOrigin(source+"/project/auth/callback", source, target); !ok || got != target+"/project/auth/callback" {
		t.Fatalf("managed callback = %q, %v", got, ok)
	}
	for _, value := range []string{"http://platform.example.com.attacker.test/callback", "https://other.example.com/callback"} {
		if got, ok := replaceOrigin(value, source, target); ok || got != value {
			t.Fatalf("unmanaged callback %q was rewritten to %q", value, got)
		}
	}
}

// statementCall 记录一次被测语句执行，供无 MySQL 环境断言语句顺序与参数。
type statementCall struct {
	SQL    string
	Values []any
}

// fakeStatementExecutor 按调用序号返回预设的受影响行数；序号越界后返回 0，
// 因此"收敛"语义（record=0 且 apply=0 时停止）可以被精确观测。
type fakeStatementExecutor struct {
	calls     []statementCall
	affected  []int64
	failAt    int
	failedErr error
}

func (executor *fakeStatementExecutor) Exec(sql string, values ...any) *gorm.DB {
	index := len(executor.calls)
	executor.calls = append(executor.calls, statementCall{SQL: sql, Values: values})
	if executor.failAt > 0 && index == executor.failAt-1 {
		return &gorm.DB{Error: executor.failedErr}
	}
	var affected int64
	if index < len(executor.affected) {
		affected = executor.affected[index]
	}
	return &gorm.DB{RowsAffected: affected}
}

// 降级截断必须覆盖且只覆盖这三张会话/令牌表；漏一张表的后果是该表会话在失败降级后永久缩短。
func TestSessionLifetimeTablesCoverEveryCappedTable(t *testing.T) {
	expected := map[string]bool{"iam_session": true, "oauth_token_family": true, "oauth_refresh_token": true}
	if len(sessionLifetimeTables) != len(expected) {
		t.Fatalf("capped tables = %v, want exactly %v", sessionLifetimeTables, expected)
	}
	for _, table := range sessionLifetimeTables {
		if !expected[table] {
			t.Fatalf("unexpected capped table %q", table)
		}
		if !validSessionLifetimeTable(table) {
			t.Fatalf("table %q is not accepted by the statement builder", table)
		}
	}
	// 语句构造只接受白名单表名：表名会直接拼进 SQL，越界值必须被拒绝。
	for _, table := range []string{"iam_account", "platform_oauth_client_credential", "iam_session; DROP TABLE iam_session"} {
		if validSessionLifetimeTable(table) {
			t.Fatalf("table %q must not be interpolated into cap statements", table)
		}
	}
}

// 语句层不变量：先记录后截断、重试幂等、恢复带等值守卫、清理只删已恢复的行。
// 这些谓词是 BIZ-RUNTIME-06 的全部安全性来源，任何一条被"优化"掉都会让失败的降级
// 再次变成不可恢复的全平台下线。
func TestSessionCapStatementsKeepRecordRestoreGuards(t *testing.T) {
	for _, table := range sessionLifetimeTables {
		record := capSessionRecordStatement(table)
		for _, required := range []string{
			"INSERT IGNORE INTO " + sessionCapTable,
			"transition_id, session_table, session_id, original_expires_at, capped_expires_at, created_at",
			"target.expires_at",
			"NOT EXISTS", // 已记录过的行不重复记录
			"ORDER BY target.expires_at",
			"LIMIT ?",
		} {
			if !strings.Contains(record, required) {
				t.Fatalf("%s record statement missing %q:\n%s", table, required, record)
			}
		}
		if strings.Contains(record, "UPDATE ") || strings.Contains(record, "SET ") {
			t.Fatalf("%s record statement must not truncate before the original value is stored:\n%s", table, record)
		}

		apply := capSessionApplyStatement(table)
		if !strings.HasPrefix(apply, "UPDATE "+table+" AS target") {
			t.Fatalf("%s apply statement targets the wrong table:\n%s", table, apply)
		}
		// 只截断"尚未截断到记录值"的行 ⇒ 重试不会把到期时间越截越短。
		if !strings.Contains(apply, "target.expires_at > recorded.capped_expires_at") {
			t.Fatalf("%s apply statement lost the no-double-cap guard:\n%s", table, apply)
		}
		if strings.Contains(apply, "original_expires_at") {
			t.Fatalf("%s apply statement must never rewrite the recorded original value:\n%s", table, apply)
		}

		restore := restoreSessionStatement(table)
		// 等值守卫是部分恢复的核心：截断之后发生的合法变更（续期/重新登录/自然过期）不得被覆盖。
		for _, required := range []string{
			"SET target.expires_at = recorded.original_expires_at",
			"WHERE target.expires_at = recorded.capped_expires_at",
		} {
			if !strings.Contains(restore, required) {
				t.Fatalf("%s restore statement missing %q:\n%s", table, required, restore)
			}
		}

		cleanup := restoreSessionCleanupStatement(table)
		for _, required := range []string{
			"DELETE recorded FROM " + sessionCapTable,
			"recorded.transition_id = ?",
			"recorded.session_table = ?",
			"target.expires_at = recorded.original_expires_at",
		} {
			if !strings.Contains(cleanup, required) {
				t.Fatalf("%s cap cleanup missing %q:\n%s", table, required, cleanup)
			}
		}
	}
}

// Begin 的截断顺序：每张表都必须先写 cap 行、再截断；并且以"本次没有新增记录且没有
// 新增截断"作为收敛条件，重试 Begin（或事务内重跑）不会重复记录或二次截断。
func TestCapSessionLifetimesRecordsBeforeTruncatingAndConverges(t *testing.T) {
	deadline := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	now := deadline.Add(-time.Hour)
	executor := &fakeStatementExecutor{
		// 每张表：先满批（500 记录 / 500 截断），再一批（3 / 3），最后收敛（0 / 0）。
		affected: []int64{500, 500, 3, 3, 0, 0, 500, 500, 0, 0, 7, 7, 0, 0},
	}
	if err := capSessionLifetimes(executor, "01TRANSITION000000000000001", deadline, now); err != nil {
		t.Fatalf("cap session lifetimes: %v", err)
	}
	if len(executor.calls)%2 != 0 {
		t.Fatalf("cap must always run record+apply pairs, got %d calls", len(executor.calls))
	}
	for index := 0; index < len(executor.calls); index += 2 {
		record, apply := executor.calls[index], executor.calls[index+1]
		if !strings.HasPrefix(record.SQL, "INSERT IGNORE INTO "+sessionCapTable) {
			t.Fatalf("call %d must record the original expiry first:\n%s", index, record.SQL)
		}
		if !strings.HasPrefix(apply.SQL, "UPDATE ") {
			t.Fatalf("call %d must truncate only after recording:\n%s", index+1, apply.SQL)
		}
		// 绑定参数里的表名必须与语句中拼接的表一致。
		var expectedTable string
		for _, table := range sessionLifetimeTables {
			if strings.Contains(record.SQL, "FROM "+table+" AS target") {
				expectedTable = table
				break
			}
		}
		if expectedTable == "" || record.Values[1] != expectedTable {
			t.Fatalf("record call %d values = %#v, want transition+%s", index, record.Values, expectedTable)
		}
		if !strings.Contains(apply.SQL, expectedTable+" AS target") {
			t.Fatalf("apply call %d must truncate %s after recording it:\n%s", index+1, expectedTable, apply.SQL)
		}
		if !record.Values[3].(time.Time).Equal(now) || !record.Values[2].(time.Time).Equal(deadline) {
			t.Fatalf("record call %d must persist capped_expires_at=%s created_at=%s, got %#v", index, deadline, now, record.Values)
		}
		if record.Values[len(record.Values)-1] != capSessionBatchSize {
			t.Fatalf("record call %d must stay batched, limit = %#v", index, record.Values[len(record.Values)-1])
		}
	}
	if len(executor.calls) != 14 {
		t.Fatalf("cap calls = %d, want 14 (per-table pairs up to convergence)", len(executor.calls))
	}
}

// 记录失败必须直接失败，不能继续截断：否则截断值没有原值可恢复。
func TestCapSessionLifetimesStopsWhenRecordingFails(t *testing.T) {
	deadline := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	executor := &fakeStatementExecutor{failAt: 1, failedErr: errors.New("insert denied")}
	err := capSessionLifetimes(executor, "01TRANSITION000000000000001", deadline, deadline.Add(-time.Hour))
	if err == nil || !strings.Contains(err.Error(), "iam_session") || !strings.Contains(err.Error(), "insert denied") {
		t.Fatalf("cap error = %v, want wrapped iam_session record failure", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("cap must not truncate after a failed record, calls = %d", len(executor.calls))
	}
}

// Fail 的恢复顺序：每张表先按守卫复原、再删除已复原的 cap 行；参数必须绑定切换 ID 与表名，
// 保证只处理本次切换、本表的记录。
func TestRestoreSessionLifetimesRestoresThenCleansPerTable(t *testing.T) {
	executor := &fakeStatementExecutor{}
	if err := restoreSessionLifetimes(executor, "01TRANSITION000000000000001"); err != nil {
		t.Fatalf("restore session lifetimes: %v", err)
	}
	if len(executor.calls) != 2*len(sessionLifetimeTables) {
		t.Fatalf("restore calls = %d, want 2 per table", len(executor.calls))
	}
	for index := 0; index < len(executor.calls); index += 2 {
		restore, cleanup := executor.calls[index], executor.calls[index+1]
		table := sessionLifetimeTables[index/2]
		if !strings.HasPrefix(restore.SQL, "UPDATE "+table+" AS target") {
			t.Fatalf("call %d must restore %s first:\n%s", index, table, restore.SQL)
		}
		if len(restore.Values) != 2 || restore.Values[0] != table || restore.Values[1] != "01TRANSITION000000000000001" {
			t.Fatalf("restore %s values = %#v", table, restore.Values)
		}
		if !strings.HasPrefix(cleanup.SQL, "DELETE recorded FROM "+sessionCapTable) {
			t.Fatalf("call %d must clean up cap rows after restoring:\n%s", index+1, cleanup.SQL)
		}
		if len(cleanup.Values) != 2 || cleanup.Values[0] != "01TRANSITION000000000000001" || cleanup.Values[1] != table {
			t.Fatalf("cleanup %s values = %#v", table, cleanup.Values)
		}
	}
}

// 恢复失败必须整体失败（Fail 事务回滚），不能"部分恢复成功还报成功"。
func TestRestoreSessionLifetimesPropagatesFailure(t *testing.T) {
	executor := &fakeStatementExecutor{failAt: 2, failedErr: errors.New("lock wait timeout")}
	err := restoreSessionLifetimes(executor, "01TRANSITION000000000000001")
	if err == nil || !strings.Contains(err.Error(), "iam_session") || !strings.Contains(err.Error(), "lock wait timeout") {
		t.Fatalf("restore error = %v, want wrapped iam_session cleanup failure", err)
	}
}
