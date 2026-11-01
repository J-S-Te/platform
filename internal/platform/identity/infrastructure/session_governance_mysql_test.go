package infrastructure

import (
	"context"
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
