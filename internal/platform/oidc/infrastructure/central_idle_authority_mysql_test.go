package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	sessiondomain "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/gorm"
)

// TestResolveSessionSubjectCentralIdlePolicyMySQL 验证中央模式只使用中央活动时间裁决空闲期限。
// 参数：t 为测试上下文，仅使用明确配置的专用 OIDC 隔离数据库。
// 返回值：无；有效中央会话误拒或失效中央会话被放行时报告失败。
func TestResolveSessionSubjectCentralIdlePolicyMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database := oidcSessionIdleMySQLDatabase(t, ctx)
	if !database.Migrator().HasColumn("iam_user", "valid_until") {
		ddl, err := migrations.Files.ReadFile("000104_complete_identity_expiry_lifecycle.sql")
		if err != nil {
			t.Fatal(err)
		}
		if err = database.Exec(string(ddl)).Error; err != nil {
			t.Fatal(err)
		}
	}
	ddl, err := migrations.Files.ReadFile("000133_central_session_governance.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Exec(string(ddl)).Error; err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	repository.EnableCentralSessions()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, test := range []struct {
		name                     string
		legacy, central, expires time.Duration
		revoked, wantSubject     bool
	}{
		{name: "central activity survives stale legacy clock", legacy: -31 * time.Minute, central: -time.Minute, expires: time.Hour, wantSubject: true},
		{name: "fresh legacy clock cannot revive idle central session", legacy: -time.Minute, central: -11 * time.Minute, expires: time.Hour},
		{name: "fresh activity cannot revive expired central session", legacy: -time.Minute, central: -time.Minute, expires: -time.Millisecond},
		{name: "fresh activity cannot revive revoked central session", legacy: -time.Minute, central: -time.Minute, expires: time.Hour, revoked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOIDCSessionIdleFixture(t, database, now, test.legacy, pointerTo[uint](600))
			seedOIDCCentralIdleFixture(t, database, fixture, now, test.central, test.expires, test.revoked)
			subject, err := repository.ResolveSessionSubject(ctx, fixture.sessionID, now)
			if !test.wantSubject {
				if !errors.Is(err, application.ErrNotFound) {
					t.Fatalf("inactive central session error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("active central session rejected: %v", err)
			}
			if subject.SessionID != fixture.sessionID || !subject.AuthenticatedAt.Equal(now.Add(-time.Hour)) {
				t.Fatalf("central subject or original authentication time changed: %+v", subject)
			}
		})
	}
}

// seedOIDCCentralIdleFixture 为独立身份夹具登记中央锚点与 IAM 关联。
// 参数：t 为测试上下文，database 为专用连接，fixture 为身份图，now 为基准时间，idle/expiry 为偏移，revoked 指定撤销状态。
// 返回值：无；创建或清理测试行失败时报告失败。
func seedOIDCCentralIdleFixture(t *testing.T, database *gorm.DB, fixture oidcSessionIdleFixture,
	now time.Time, idle, expiry time.Duration, revoked bool) {
	t.Helper()
	anchor := sessiondomain.Session{ID: fixture.sessionID, TenantID: fixture.tenantID,
		IdentityID: fixture.userID, AuthenticatedAt: now.Add(-time.Hour),
		LastInteractiveAt: now.Add(idle), ExpiresAt: now.Add(expiry), Version: 1}
	if revoked {
		anchor.RevokedAt = &now
		anchor.RevokeReason = "TEST_REVOKED"
	}
	if err := database.Table("sgov_session").Create(&anchor).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Table("sgov_platform_link").Create(map[string]any{
		"iam_session_id": fixture.sessionID, "session_id": fixture.sessionID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"sgov_platform_link", "sgov_session"} {
			column := "id"
			if table == "sgov_platform_link" {
				column = "iam_session_id"
			}
			if err := database.Table(table).Where(column+" = ?", fixture.sessionID).Delete(map[string]any{}).Error; err != nil {
				t.Errorf("clean central fixture: %v", err)
			}
		}
	})
}
