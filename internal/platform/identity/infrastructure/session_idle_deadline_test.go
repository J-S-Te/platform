package infrastructure

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type idleSessionScenario struct {
	interactive, expires time.Time
	writes               []string
}

type idleSessionDriver struct{ scenario *idleSessionScenario }
type idleSessionConn struct{ scenario *idleSessionScenario }

// Open 建立仅供 SQL 契约回归使用的内存连接。
// 参数：连接名不使用，测试场景由驱动实例提供。
// 返回值：可观测的连接及空错误。
func (d *idleSessionDriver) Open(string) (driver.Conn, error) {
	return &idleSessionConn{scenario: d.scenario}, nil
}

// Prepare 拒绝本回归不使用的预处理入口。
// 参数：SQL 文本不使用。
// 返回值：空语句及不支持错误。
func (*idleSessionConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepared statements are not supported")
}

// Close 结束不持有外部资源的测试连接。
// 参数：无。
// 返回值：空错误。
func (*idleSessionConn) Close() error { return nil }

// Begin 提供 GORM 更新默认使用的事务边界。
// 参数：无。
// 返回值：内存事务及空错误。
func (*idleSessionConn) Begin() (driver.Tx, error) { return staleCredentialTx{}, nil }

// ExecContext 记录实际 GORM 写语句，避免普通身份读取推进交互或固定到期。
// 参数：上下文与绑定值不使用，query 为实际 SQL。
// 返回值：一行写入结果及空错误。
func (c *idleSessionConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.scenario.writes = append(c.scenario.writes, query)
	return staleCredentialResult(1), nil
}

// QueryContext 在持久化边界提供完整会话和当前身份投影。
// 参数：上下文与绑定值不使用，query 为实际 GORM 查询。
// 返回值：会话、身份或空授权行；不连接任何数据库。
func (c *idleSessionConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "FROM `iam_session`") {
		return &staleCredentialRows{
			columns: []string{"id", "tenant_id", "account_id", "last_interactive_at", "expires_at", "status"},
			values:  [][]driver.Value{{"s1", "t1", "a1", c.scenario.interactive, c.scenario.expires, domain.StatusActive}},
		}, nil
	}
	if strings.Contains(query, "FROM iam_session AS session") {
		return &staleCredentialRows{
			columns: []string{"session_id", "tenant_id", "user_id", "account_id"},
			values:  [][]driver.Value{{"s1", "t1", "u1", "a1"}},
		}, nil
	}
	return &staleCredentialRows{columns: []string{"id"}}, nil
}

// newIdleSessionRepository 构造执行真实 GORM 查询的隔离内存 SQL 适配器。
// 参数：t 为测试上下文，scenario 为会话事实与写语句记录。
// 返回值：真实身份仓储；构造异常时终止测试。
func newIdleSessionRepository(t *testing.T, scenario *idleSessionScenario) *GORMRepository {
	t.Helper()
	name := fmt.Sprintf("idle-session-%d", atomic.AddUint64(&staleCredentialDriverCounter, 1))
	sql.Register(name, &idleSessionDriver{scenario: scenario})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	orm, err := gorm.Open(mysql.New(mysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewGORMRepository(orm)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

// TestFindPrincipalReturnsIdleDeadlineWithoutRenewal 验证当前策略、固定上限和只读不续期。
// 参数：t 为测试运行上下文。
// 返回值：无；期限或写入语义错误时报告失败。
func TestFindPrincipalReturnsIdleDeadlineWithoutRenewal(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                   string
		idle, fixed, remaining time.Duration
	}{
		{"idle window", 30 * time.Minute, time.Hour, 29 * time.Minute},
		{"absolute cap", 30 * time.Minute, 5 * time.Minute, 5 * time.Minute},
		{"shortened policy", 2 * time.Minute, time.Hour, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := &idleSessionScenario{interactive: now.Add(-time.Minute), expires: now.Add(tc.fixed)}
			repository := newIdleSessionRepository(t, scenario)
			principal, err := repository.FindPrincipalBySession(context.Background(), "s1", now, tc.idle)
			if err != nil {
				t.Fatal(err)
			}
			if !principal.SessionIdleDeadline.Equal(now.Add(tc.remaining)) {
				t.Fatalf("deadline = %v, want %v", principal.SessionIdleDeadline, now.Add(tc.remaining))
			}
			if len(scenario.writes) != 1 || !strings.Contains(scenario.writes[0], "SET `last_seen_at`=") {
				t.Fatalf("unexpected writes: %v", scenario.writes)
			}
			assignment := strings.Split(strings.Split(scenario.writes[0], " SET ")[1], " WHERE ")[0]
			if strings.Contains(assignment, "last_interactive_at") || strings.Contains(assignment, "expires_at") {
				t.Fatalf("ordinary read renewed session: %s", assignment)
			}
		})
	}
}

// TestFindPrincipalRejectsShortenedIdlePolicy 验证新策略已过期时不返回可重排窗口。
// 参数：t 为测试运行上下文。
// 返回值：无；没有返回未认证时报告失败。
func TestFindPrincipalRejectsShortenedIdlePolicy(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	scenario := &idleSessionScenario{interactive: now.Add(-2 * time.Minute), expires: now.Add(time.Hour)}
	repository := newIdleSessionRepository(t, scenario)
	_, err := repository.FindPrincipalBySession(context.Background(), "s1", now, time.Minute)
	if !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("error = %v", err)
	}
}
