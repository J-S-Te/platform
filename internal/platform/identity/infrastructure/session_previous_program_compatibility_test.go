package infrastructure

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	oidcapp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	oidcdomain "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/domain"
	oidcstore "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// TestPreviousProgramOnMigratedDatabase 使用上一主线原业务源码验证新迁移数据库的实际读写。
// 参数：t为隔离兼容性测试上下文，专用DSN由既有严格守卫校验。
// 返回值：无；迁移目标不符、旧IAM会话或OIDC授权码读写/消费失败即终止。
func TestPreviousProgramOnMigratedDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	db := sessionIdleMySQLDatabase(t, ctx)
	var columns int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='oauth_authorization_code' AND column_name='authenticated_at' AND is_nullable='YES'").Scan(&columns).Error; err != nil || columns != 1 {
		t.Fatal("new nullable authentication-time migration must already be applied")
	}
	fixture := newSessionIdleMySQLFixture(t, db, time.Hour)
	var ids [4]string
	for i := range ids {
		var err error
		ids[i], err = ulid.New(fixture.now)
		if err != nil {
			t.Fatal(err)
		}
	}
	applicationID, environmentID, clientID, codeID := ids[0], ids[1], ids[2], ids[3]
	t.Cleanup(func() {
		for _, item := range []struct{ table, column string }{{"oauth_authorization_code", "id"}, {"platform_oauth_grant_type", "oauth_client_id"}, {"platform_oauth_client", "id"}, {"platform_application_environment", "id"}, {"platform_application", "id"}} {
			value := clientID
			switch item.table {
			case "oauth_authorization_code":
				value = codeID
			case "platform_application_environment":
				value = environmentID
			case "platform_application":
				value = applicationID
			}
			if err := db.Table(item.table).Where(item.column+" = ?", value).Delete(map[string]any{}).Error; err != nil {
				t.Error(err)
			}
		}
	})
	rows := []struct {
		table  string
		values map[string]any
	}{
		{"platform_application", map[string]any{"id": applicationID, "tenant_id": fixture.tenantID, "code": "compat-" + applicationID, "name": "Previous program compatibility", "application_type": "INTERNAL", "status": "ACTIVE"}},
		{"platform_application_environment", map[string]any{"id": environmentID, "tenant_id": fixture.tenantID, "application_id": applicationID, "environment": "compat", "status": "ACTIVE"}},
		{"platform_oauth_client", map[string]any{"id": clientID, "tenant_id": fixture.tenantID, "application_id": applicationID, "environment_id": environmentID, "client_id": "compat-" + clientID, "client_name": "Isolated compatibility", "client_type": "PUBLIC", "token_auth_method": "none", "access_token_ttl_seconds": 300, "refresh_token_ttl_seconds": 28800, "require_pkce": false, "status": "ACTIVE"}},
	}
	for _, row := range rows {
		row.values["created_at"] = fixture.now
		row.values["updated_at"] = fixture.now
		row.values["version"] = 1
		if err := db.Table(row.table).Create(row.values).Error; err != nil {
			t.Fatal("create previous-program fixture", row.table, err)
		}
	}
	if err := db.Table("platform_oauth_grant_type").Create(map[string]any{"oauth_client_id": clientID, "grant_type": "authorization_code"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repository.FindPrincipalBySession(ctx, fixture.sessionID, fixture.now, 30*time.Minute); err != nil {
		t.Fatal("previous IAM read", err)
	}
	if _, err := fixture.repository.RefreshSession(ctx, fixture.sessionID, fixture.now.Add(time.Millisecond)); err != nil {
		t.Fatal("previous IAM refresh", err)
	}
	if err := fixture.repository.RecordSessionInteraction(ctx, fixture.sessionID, fixture.now.Add(2*time.Millisecond), 30*time.Minute); err != nil {
		t.Fatal("previous IAM activity", err)
	}
	repository, err := oidcstore.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("compat-" + codeID))
	code := oidcdomain.AuthorizationCode{ID: codeID, TenantID: fixture.tenantID, OAuthClientID: clientID, SessionID: fixture.sessionID, AccountID: fixture.accountID, UserID: fixture.userID, CodeHash: hash, RedirectURI: "https://compat.example.invalid/callback", Scopes: []string{"openid"}, CreatedAt: fixture.now, ExpiresAt: fixture.now.Add(time.Minute), Status: oidcdomain.AuthorizationCodeStatusActive}
	if err := repository.CreateAuthorizationCode(ctx, code); err != nil {
		t.Fatal("previous OAuth insert", err)
	}
	stored, err := repository.FindAuthorizationCode(ctx, hash, fixture.now)
	if err != nil || stored.ID != codeID {
		t.Fatal("previous OAuth read failed", err)
	}
	grant, err := repository.ConsumeAuthorizationCode(ctx, oidcapp.ConsumeAuthorizationCodeCommand{CodeHash: hash, ClientID: clientID, RedirectURI: code.RedirectURI}, fixture.now)
	if err != nil || grant.SessionID != fixture.sessionID {
		t.Fatal("previous OAuth consume failed", err)
	}
	if _, err := repository.ConsumeAuthorizationCode(ctx, oidcapp.ConsumeAuthorizationCodeCommand{CodeHash: hash, ClientID: clientID, RedirectURI: code.RedirectURI}, fixture.now); err == nil {
		t.Fatal("previous program accepted consumed code")
	}
	if err := fixture.repository.RevokeAccountSessions(ctx, fixture.tenantID, fixture.accountID, fixture.now, "COMPATIBILITY_TEST"); err != nil {
		t.Fatal("previous IAM revoke", err)
	}
	t.Log("unmodified previous main business code: real IAM read/refresh/activity/revoke and OIDC insert/read/single-use consume on migration134 database passed")
}
