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
	interactive, expires, seen time.Time
	writes                     []string
	// concurrentWriterWon 模拟并发请求已先写入观测值：UPDATE 的 last_seen_at 守卫匹配零行。
	concurrentWriterWon bool
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
// 返回值：一行写入结果；concurrentWriterWon 场景返回零变更行以复现并发节流竞争。
func (c *idleSessionConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.scenario.writes = append(c.scenario.writes, query)
	if c.scenario.concurrentWriterWon {
		return staleCredentialResult(0), nil
	}
	return staleCredentialResult(1), nil
}

// QueryContext 在持久化边界提供完整会话和当前身份投影。
// 参数：上下文与绑定值不使用，query 为实际 GORM 查询。
// 返回值：会话、身份或空授权行；不连接任何数据库。
func (c *idleSessionConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "FROM `iam_session`") {
		return &staleCredentialRows{
			columns: []string{"id", "tenant_id", "account_id", "last_seen_at", "last_interactive_at", "expires_at", "status"},
			values:  [][]driver.Value{{"s1", "t1", "a1", c.scenario.seen, c.scenario.interactive, c.scenario.expires, domain.StatusActive}},
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

// TestFindPrincipalThrottlesLastSeenWrite 验证 last_seen_at 的观测写入按阈值节流，
// 且节流前后空闲期限判定完全一致。
// 参数：t 为测试运行上下文。
// 返回值：无；写入次数、写入列或期限语义不符时报告失败。
func TestFindPrincipalThrottlesLastSeenWrite(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	idle := 30 * time.Minute
	for _, tc := range []struct {
		name       string
		seen       time.Time
		wantWrites int
	}{
		// 控制台一次页面打开会扇出数十个并发 GET，间隔内的请求不得再争用同一行行锁。
		{"inside throttle window", now.Add(-time.Second), 0},
		{"one millisecond inside throttle window", now.Add(-sessionLastSeenWriteInterval + time.Millisecond), 0},
		{"newer observation than clock", now.Add(time.Minute), 0},
		{"reaches throttle boundary", now.Add(-sessionLastSeenWriteInterval), 1},
		{"past throttle window", now.Add(-sessionLastSeenWriteInterval - time.Millisecond), 1},
		// 历史行或从未写入观测值时仍要补写一次，避免观测永久停在零值。
		{"never observed", time.Time{}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := &idleSessionScenario{interactive: now.Add(-time.Minute), expires: now.Add(time.Hour), seen: tc.seen}
			repository := newIdleSessionRepository(t, scenario)
			principal, err := repository.FindPrincipalBySession(context.Background(), "s1", now, idle)
			if err != nil {
				t.Fatal(err)
			}
			// 节流只影响观测列：空闲期限仍由 last_interactive_at 加策略得出。
			if !principal.SessionIdleDeadline.Equal(now.Add(idle - time.Minute)) {
				t.Fatalf("deadline = %v, want %v", principal.SessionIdleDeadline, now.Add(idle-time.Minute))
			}
			if len(scenario.writes) != tc.wantWrites {
				t.Fatalf("writes = %v, want %d", scenario.writes, tc.wantWrites)
			}
			for _, write := range scenario.writes {
				if !strings.Contains(write, "SET `last_seen_at`=") {
					t.Fatalf("unexpected write: %s", write)
				}
				// 写语句自带 last_seen_at 守卫：窗口刚过期时同时到达的并发请求只有一个真正写入。
				if !strings.Contains(write, "last_seen_at <= ?") {
					t.Fatalf("observation write lacks concurrency guard: %s", write)
				}
				assignment := strings.Split(strings.Split(write, " SET ")[1], " WHERE ")[0]
				if strings.Contains(assignment, "last_interactive_at") || strings.Contains(assignment, "expires_at") {
					t.Fatalf("observation write renewed idle or absolute deadline: %s", assignment)
				}
			}
		})
	}
}

// TestFindPrincipalKeepsAuthenticationWhenConcurrentWriterWonObservation 验证并发请求先写入
// 观测值时，本请求的守卫更新匹配零行仍被复核为有效会话，不会被误判为登出。
// 参数：t 为测试运行上下文。
// 返回值：无；守卫零变更行导致认证失败或漏掉复核时报告失败。
func TestFindPrincipalKeepsAuthenticationWhenConcurrentWriterWonObservation(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	scenario := &idleSessionScenario{
		interactive:         now.Add(-time.Minute),
		expires:             now.Add(time.Hour),
		seen:                now.Add(-sessionLastSeenWriteInterval - time.Second),
		concurrentWriterWon: true,
	}
	repository := newIdleSessionRepository(t, scenario)
	principal, err := repository.FindPrincipalBySession(context.Background(), "s1", now, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if principal.SessionID != "s1" {
		t.Fatalf("principal = %+v", principal)
	}
	if len(scenario.writes) != 1 {
		t.Fatalf("writes = %v, want 1 guarded write", scenario.writes)
	}
	if !strings.Contains(scenario.writes[0], "last_seen_at <= ?") {
		t.Fatalf("write lacks concurrency guard: %s", scenario.writes[0])
	}
}

// TestFindPrincipalKeepsAuthenticationWhenObservationWriteSkipped 验证节流跳过写入时
// 仍然完成会话、角色与权限校验，不把“未写观测行”误判为未认证。
// 参数：t 为测试运行上下文。
// 返回值：无；跳过写入导致认证失败时报告失败。
func TestFindPrincipalKeepsAuthenticationWhenObservationWriteSkipped(t *testing.T) {
	now := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	scenario := &idleSessionScenario{interactive: now.Add(-time.Minute), expires: now.Add(time.Hour), seen: now.Add(-time.Second)}
	repository := newIdleSessionRepository(t, scenario)
	principal, err := repository.FindPrincipalBySession(context.Background(), "s1", now, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if principal.SessionID != "s1" || principal.Tenant.ID != "t1" || principal.Account.ID != "a1" {
		t.Fatalf("principal = %+v", principal)
	}
	if len(scenario.writes) != 0 {
		t.Fatalf("throttled request wrote session row: %v", scenario.writes)
	}
}
