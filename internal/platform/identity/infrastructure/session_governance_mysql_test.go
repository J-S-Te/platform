package infrastructure

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"gorm.io/gorm"
	"sync"
	"testing"
	"time"

	sessiondomain "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	sessionstore "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
)

// TestGlobalLogoutRevokesNeutralCentralSessionsMySQL 验证全局退出覆盖没有IAM关联的业务会话。
// 参数：t为只连接显式隔离MySQL的测试上下文。
// 返回值：无；原身份未撤销或其他租户被影响时失败。
func TestGlobalLogoutRevokesNeutralCentralSessionsMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	db := sessionIdleMySQLDatabase(t, ctx)
	fixture := newSessionIdleMySQLFixture(t, db, 8*time.Hour)
	other := newSessionIdleMySQLFixture(t, db, 8*time.Hour)
	central, err := sessionstore.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	fixture.repository.EnableCentralSessions(central)
	identity := sessionstore.VerifiedIdentity{Issuer: "https://isolated-issuer.test", SID: fixture.sessionID,
		TenantID: fixture.tenantID, IdentityID: fixture.userID, Subject: fixture.userID, AuthenticatedAt: fixture.now}
	neutral, err := central.ResolveOIDC(ctx, identity, "", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	identity.SID, identity.Subject = other.sessionID, other.userID
	identity.TenantID, identity.IdentityID = other.tenantID, other.userID
	unrelated, err := central.ResolveOIDC(ctx, identity, "", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.RevokeAccountSessions(ctx, fixture.tenantID, fixture.accountID, fixture.now.Add(time.Second), "USER_LOGOUT"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id      string
		revoked bool
	}{{neutral.ID, true}, {unrelated.ID, false}} {
		var row sessiondomain.Session
		if err := db.Table("sgov_session").Where("id = ?", item.id).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		if (row.RevokedAt != nil) != item.revoked {
			t.Fatal("global logout did not retain the exact identity and tenant boundary")
		}
	}
}

// TestSessionRefreshNoopMySQL 验证同毫秒认证与并发刷新不会把有效会话误判为退出。
// 参数：t为真实隔离MySQL上下文，覆盖旧模式与平台中央管理模式。
// 返回值：无；零变更行误401、原截止漂移或撤销后仍可刷新即失败。
func TestSessionRefreshNoopMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	db := sessionIdleMySQLDatabase(t, ctx)
	for _, enabled := range []bool{false, true} {
		name := "legacy"
		if enabled {
			name = "central"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newSessionIdleMySQLFixture(t, db, time.Hour)
			original := fixture.now.Add(time.Hour)
			if enabled {
				central, err := sessionstore.NewStore(db)
				if err != nil {
					t.Fatal(err)
				}
				err = db.Transaction(func(tx *gorm.DB) error {
					_, err := sessionstore.LinkPlatformTx(tx, sessiondomain.PlatformAnchor{ID: fixture.sessionID, TenantID: fixture.tenantID, IdentityID: fixture.userID, AuthenticatedAt: fixture.now.Add(-time.Minute), LastInteractiveAt: fixture.now.Add(-time.Minute), ExpiresAt: original}, "")
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				fixture.repository.EnableCentralSessions(central)
			}
			if _, err := fixture.repository.FindPrincipalBySession(ctx, fixture.sessionID, fixture.now, 30*time.Minute); err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			failures := make(chan error, 12)
			for range 12 {
				group.Add(1)
				go func() {
					defer group.Done()
					expires, err := fixture.repository.RefreshSession(ctx, fixture.sessionID, fixture.now)
					if err == nil && !expires.Equal(original) {
						err = errors.New("fixed expiry changed")
					}
					failures <- err
				}()
			}
			group.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Errorf("same-millisecond valid refresh failed: %v", err)
				}
			}
			if err := fixture.repository.RevokeSession(ctx, fixture.sessionID, fixture.now, "QA_REFRESH_REVOKE"); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.repository.RefreshSession(ctx, fixture.sessionID, fixture.now); !errors.Is(err, application.ErrUnauthenticated) {
				t.Fatalf("revoked session refresh accepted: %v", err)
			}
		})
	}
}
