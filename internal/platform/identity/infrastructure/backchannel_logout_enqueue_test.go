package infrastructure

import (
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func stringPointerValue(value string) *string { return &value }

// 未登记 logout_uri 的 RP 必须照常产生 outbox 行，状态为 NO_ENDPOINT 且原因码固定：
// "缺登记"从"没有任务"变成"有任务但不可投递且可见"（BIZ-XEVT2-07）。
func TestBackchannelLogoutOutboxRowWithoutRegisteredURIIsNoEndpoint(t *testing.T) {
	now := time.Date(2026, time.October, 30, 8, 0, 0, 0, time.UTC)
	row := backchannelLogoutOutboxRow("01KEVENT0000000000000000001", backchannelLogoutTarget{
		OAuthClientID: "01KCLIENT000000000000000001", SessionID: "01KSESSION00000000000000001", UserID: "01KUSER0000000000000000001",
	}, now)
	if row["status"] != backchannel.StatusNoEndpoint {
		t.Fatalf("status = %v, want %s", row["status"], backchannel.StatusNoEndpoint)
	}
	if row["last_error"] != backchannel.ReasonMissingLogoutURI {
		t.Fatalf("last_error = %v, want %s", row["last_error"], backchannel.ReasonMissingLogoutURI)
	}
	if row["oauth_client_id"] != "01KCLIENT000000000000000001" || row["session_id"] != "01KSESSION00000000000000001" || row["subject_id"] != "01KUSER0000000000000000001" {
		t.Fatalf("row lost addressing fields: %#v", row)
	}
	if row["attempt_count"] != 0 || row["next_attempt_at"] != now.UTC() {
		t.Fatalf("row scheduling = %#v", row)
	}
}

// 已登记端点（含空白字符）时任务可直接投递，且不写错误原因。
func TestBackchannelLogoutOutboxRowWithRegisteredURIIsPending(t *testing.T) {
	now := time.Date(2026, time.October, 30, 8, 0, 0, 0, time.UTC)
	target := backchannelLogoutTarget{
		OAuthClientID: "01KCLIENT000000000000000001", SessionID: "01KSESSION00000000000000001", UserID: "01KUSER0000000000000000001",
		LogoutURI: stringPointerValue("https://rp.example.com/auth/backchannel-logout"),
	}
	row := backchannelLogoutOutboxRow("01KEVENT0000000000000000002", target, now)
	if row["status"] != backchannel.StatusPending {
		t.Fatalf("status = %v, want %s", row["status"], backchannel.StatusPending)
	}
	if _, ok := row["last_error"]; ok {
		t.Fatalf("pending row carries last_error: %#v", row)
	}
	blank := target
	blank.LogoutURI = stringPointerValue("   ")
	if got := backchannelLogoutOutboxRow("01KEVENT0000000000000000003", blank, now)["status"]; got != backchannel.StatusNoEndpoint {
		t.Fatalf("blank URI status = %v, want %s", got, backchannel.StatusNoEndpoint)
	}
}

// 生成侧不再以 logout_uri 登记为过滤条件：渲染后的查询必须是 LEFT JOIN（缺登记也要
// 出行），会话 ID 列表正确展开，并且仍然只针对已建立 Token Family 的活跃客户端。
// 用 GORM dry-run 渲染真实 SQL，避免只断言字符串常量而漏掉参数展开错误。
func TestBackchannelLogoutTargetsSQLDoesNotFilterByRegisteredURI(t *testing.T) {
	database, err := gorm.Open(mysql.New(mysql.Config{
		DriverName: "mysql", DSN: "platform:platform@tcp(127.0.0.1:1)/platform", SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	var targets []backchannelLogoutTarget
	statement := database.Session(&gorm.Session{DryRun: true}).
		Raw(backchannelLogoutTargetsSQL, domain.StatusActive, "01KTENANT00000000000000001", "01KACCOUNT0000000000000001", []string{"01KSESSION00000000000000001", "01KSESSION00000000000000002"}).
		Scan(&targets).Statement
	rendered := strings.Join(strings.Fields(statement.SQL.String()), " ")
	if !strings.Contains(rendered, "LEFT JOIN platform_oauth_backchannel_logout_uri AS uri ON uri.oauth_client_id = family.oauth_client_id") {
		t.Fatalf("targets query is not a LEFT JOIN on the registration table:\n%s", rendered)
	}
	if !strings.Contains(rendered, "session_id IN (?,?)") {
		t.Fatalf("session list was not expanded:\n%s", rendered)
	}
	if !strings.Contains(rendered, "client.status = ?") {
		t.Fatalf("client status filter was dropped:\n%s", rendered)
	}
	if len(statement.Vars) != 5 {
		t.Fatalf("vars = %#v", statement.Vars)
	}
}
