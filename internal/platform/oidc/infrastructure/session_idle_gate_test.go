package infrastructure

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
)

// 安全审计 M-1 回归：ResolveSessionSubject 是授权、换码、刷新令牌、userinfo 与授权码
// 消费共用的唯一会话门禁，空闲超时必须落在这一条 SQL 上。假驱动不解释 WHERE 谓词，
// 因此这里锁定“门禁仍由 SQL 表达”的原始问题：任何人删除 sec_login_policy 连接、
// 空闲比较或 0 策略的失败关闭守卫，都会在此失败，强制重新评估续期绕过面。
// 真实 MySQL 下的取值语义（策略行缺失/为 0/正数、边界）见 session_idle_mysql_test.go。
func TestResolveSessionSubjectGatesOnTenantIdlePolicy(t *testing.T) {
	t.Parallel()
	// 含亚毫秒部分的 now：门禁必须与 DATETIME(3) 列对齐后再比较。
	now := time.Date(2026, time.November, 2, 12, 0, 0, 456789000, time.UTC)
	script := &rotateScript{authorizedAt: now.Add(-24 * time.Hour), now: now}
	repository := newRotateRepositoryTest(t, script)

	// 假驱动不返回会话行，共享门禁必须失败关闭（不能把“查不到”当作已认证）。
	if _, err := repository.ResolveSessionSubject(context.Background(), "session-1", now); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("ResolveSessionSubject() error = %v, want ErrNotFound", err)
	}

	var sessionQuery rotateStatement
	found := false
	for _, entry := range script.statements {
		if strings.Contains(entry.query, "FROM "+backtick+"iam_session"+backtick) && strings.Contains(entry.query, "iam_session.expires_at > ?") {
			sessionQuery, found = entry, true
			break
		}
	}
	if !found {
		t.Fatalf("未找到会话门禁查询; statements=%+v", script.statements)
	}

	// 断言一：租户策略以 LEFT JOIN 参与判定（策略行缺失不得让查询整体无行）。
	if !strings.Contains(sessionQuery.query, "LEFT JOIN sec_login_policy ON sec_login_policy.tenant_id = iam_session.tenant_id") {
		t.Fatalf("会话门禁缺少 sec_login_policy 连接: %s", sessionQuery.query)
	}
	// 断言二：空闲谓词必须存在，且严格大于（与身份侧 After(idleCutoff) 一致）。
	if !strings.Contains(sessionQuery.query, "iam_session.last_interactive_at > DATE_SUB(?, INTERVAL COALESCE(sec_login_policy.idle_timeout_seconds, ?) SECOND)") {
		t.Fatalf("会话门禁缺少空闲窗口谓词: %s", sessionQuery.query)
	}
	// 断言三：策略值为 0 时必须失败关闭，而不是被当成“已禁用空闲检查”。
	if !strings.Contains(sessionQuery.query, "COALESCE(sec_login_policy.idle_timeout_seconds, ?) > 0") {
		t.Fatalf("会话门禁缺少 0 策略失败关闭守卫: %s", sessionQuery.query)
	}
	// 断言四：策略行缺失的默认窗口必须由包级常量提供（与 security 模块默认一致），
	// 不能散落魔数；两处 COALESCE 必须绑定同一默认值。
	if defaultSessionIdleTimeoutSeconds != 1800 {
		t.Fatalf("defaultSessionIdleTimeoutSeconds = %d, want 1800", defaultSessionIdleTimeoutSeconds)
	}
	defaults := 0
	var cutoffBase time.Time
	for _, value := range sessionQuery.values {
		switch typed := value.(type) {
		case int64:
			if typed == int64(defaultSessionIdleTimeoutSeconds) {
				defaults++
			}
		case time.Time:
			cutoffBase = typed
		}
	}
	if defaults != 2 {
		t.Fatalf("默认空闲窗口绑定次数 = %d, want 2; values=%+v", defaults, sessionQuery.values)
	}
	// 断言五：截止基准按毫秒截断，等于身份侧 DATETIME(3) 的比较精度。
	if !cutoffBase.Equal(now.Truncate(time.Millisecond)) {
		t.Fatalf("空闲截止基准 = %v, want %v", cutoffBase, now.Truncate(time.Millisecond))
	}
	// 断言六：门禁 SQL 仍与既有占位符/绑定个数一致，避免新增条件后参数错位。
	if sessionQuery.placeholders != sessionQuery.args {
		t.Fatalf("会话门禁占位符=%d 参数=%d: %s", sessionQuery.placeholders, sessionQuery.args, sessionQuery.query)
	}
}
