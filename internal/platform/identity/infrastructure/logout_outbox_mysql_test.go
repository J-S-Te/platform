package infrastructure_test

import (
	"context"
	"errors"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// TestAccountLogoutEnqueuesBackchannelOnMySQL 验证真实退出事务和通知去重不会因 MySQL 插入语法失败。
// 参数：t 为仅通过显式隔离测试 DSN 连接数据库的测试上下文。
// 返回值：无；会话撤销、通知状态或重复投递证据不正确时失败。
func TestAccountLogoutEnqueuesBackchannelOnMySQL(t *testing.T) {
	for _, name := range []string{"pending", "missing", "duplicate", "db_error"} {
		t.Run(name, func(t *testing.T) {
			db := revocationTestDatabase(t)
			fixture := newRevocationFixture(t, db, "lo-"+name[:3])
			t.Cleanup(func() {
				for _, table := range []string{
					"platform_oauth_backchannel_logout_outbox",
					"platform_oauth_backchannel_logout_uri",
				} {
					if err := db.Exec("DELETE FROM "+table+" WHERE oauth_client_id = ?", fixture.clientID).Error; err != nil {
						t.Errorf("clean logout fixture: %v", err)
					}
				}
			})
			now := time.Now().UTC().Truncate(time.Millisecond)
			prepareLogoutOutbox(t, db, fixture, name, now)
			err := revocationRepository(t, db).RevokeAccountSessions(
				context.Background(), fixture.tenantID, fixture.accountID, now, "LOGOUT",
			)
			if name == "db_error" {
				var databaseError *driver.MySQLError
				if !errors.As(err, &databaseError) || databaseError.Number != 1644 {
					t.Fatalf("want injected database error, got %v", err)
				}
				assertLogoutStatuses(t, db, fixture, "ACTIVE")
				var count int64
				if err := db.Table("platform_oauth_backchannel_logout_outbox").
					Where("oauth_client_id = ?", fixture.clientID).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("failed logout must leave no outbox: count=%d error=%v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("logout must atomically revoke and enqueue: %v", err)
			}
			assertLogoutOutbox(t, db, fixture, name)
		})
	}
}

// prepareLogoutOutbox 配置缺失端点或已投递通知，覆盖首次入队与幂等边界。
// 参数：t 为测试上下文，db 为隔离连接，fixture 为数据范围，name/now 为场景和时钟。
// 返回值：无；夹具写入失败时终止测试。
func prepareLogoutOutbox(t *testing.T, db *gorm.DB, fixture revocationFixture, name string, now time.Time) {
	t.Helper()
	if name == "db_error" {
		err := db.Exec("CREATE TRIGGER logout_outbox_failure BEFORE INSERT ON " +
			"platform_oauth_backchannel_logout_outbox FOR EACH ROW SIGNAL SQLSTATE '45000' " +
			"SET MESSAGE_TEXT = 'injected logout enqueue failure'").Error
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Exec("DROP TRIGGER logout_outbox_failure").Error; err != nil {
				t.Error(err)
			}
		})
	}
	if name == "missing" {
		if err := db.Exec("DELETE FROM platform_oauth_backchannel_logout_uri WHERE oauth_client_id = ?", fixture.clientID).Error; err != nil {
			t.Fatal(err)
		}
	}
	if name == "duplicate" {
		err := db.Table("platform_oauth_backchannel_logout_outbox").Create(map[string]any{
			"id": "existing-logout-event", "oauth_client_id": fixture.clientID,
			"session_id": fixture.sessionID, "subject_id": fixture.userID,
			"jti": "original-logout-jti", "status": "DELIVERED", "attempt_count": 2,
			"created_at": now.Add(-time.Minute), "next_attempt_at": now,
		}).Error
		if err != nil {
			t.Fatal(err)
		}
	}
}

// assertLogoutOutbox 检查持久撤销及通知证据，防止重复请求覆盖已有任务。
// 参数：t 为测试上下文，db 为隔离连接，fixture/name 为账号范围和场景。
// 返回值：无；任一撤销缺失、通知数量或原始证据被覆盖时失败。
func assertLogoutOutbox(t *testing.T, db *gorm.DB, fixture revocationFixture, name string) {
	t.Helper()
	assertLogoutStatuses(t, db, fixture, "REVOKED")
	var rows []struct {
		ID, JTI, Status string
		AttemptCount    int
	}
	if err := db.Table("platform_oauth_backchannel_logout_outbox").
		Where("oauth_client_id = ? AND session_id = ?", fixture.clientID, fixture.sessionID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one durable notification, got %d", len(rows))
	}
	want := map[string]string{"pending": "PENDING", "missing": "NO_ENDPOINT", "duplicate": "DELIVERED"}[name]
	if rows[0].Status != want {
		t.Fatalf("notification status=%q want=%q", rows[0].Status, want)
	}
	if name == "duplicate" && (rows[0].ID != "existing-logout-event" ||
		rows[0].JTI != "original-logout-jti" || rows[0].AttemptCount != 2) {
		t.Fatal("duplicate logout must preserve the original event and delivery evidence")
	}
}

// assertLogoutStatuses 核对注销成功或失败回滚后的所有登录派生物状态。
// 参数：t 为测试上下文，db 为隔离连接，fixture 为数据范围，want 为预期状态。
// 返回值：无；任一状态不符或读取失败时终止测试。
func assertLogoutStatuses(t *testing.T, db *gorm.DB, fixture revocationFixture, want string) {
	t.Helper()
	for table, id := range map[string]string{
		"iam_session": fixture.sessionID, "oauth_token_family": fixture.familyID,
		"oauth_refresh_token": fixture.refreshID, "oauth_authorization_code": fixture.codeID,
	} {
		var status string
		if err := db.Table(table).Select("status").Where("id = ?", id).Scan(&status).Error; err != nil || status != want {
			t.Fatalf("%s: status=%q want=%q error=%v", table, status, want, err)
		}
	}
}
