package publictransport

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestCoordinatorMySQLCutoverAndRecovery(t *testing.T) {
	dsn := os.Getenv("PUBLIC_TRANSPORT_TEST_DSN")
	if dsn == "" {
		t.Skip("PUBLIC_TRANSPORT_TEST_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var tenantID string
	if err := db.Table("iam_tenant").Select("id").Limit(1).Scan(&tenantID).Error; err != nil || tenantID == "" {
		t.Fatalf("load seed tenant: %v", err)
	}
	now := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	appID, envID, clientID := "01KPTAPP000000000000000001", "01KPTENV000000000000000001", "01KPTOAUTH0000000000000001"
	if err := db.Table("platform_application").Create(map[string]any{"id": appID, "tenant_id": tenantID, "code": "transport_test", "name": "Transport Test", "application_type": "BUSINESS", "homepage_url": "http://platform.example.test/app", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("platform_application_environment").Create(map[string]any{"id": envID, "tenant_id": tenantID, "application_id": appID, "environment": "test", "base_url": "http://platform.example.test", "path_prefix": "/app", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("platform_oauth_client").Create(map[string]any{"id": clientID, "tenant_id": tenantID, "application_id": appID, "environment_id": envID, "client_id": "transport-test", "client_name": "Transport Test", "client_type": "PUBLIC", "token_auth_method": "none", "access_token_ttl_seconds": 300, "refresh_token_ttl_seconds": 3600, "require_pkce": true, "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("platform_oauth_redirect_uri").Create(map[string]any{"oauth_client_id": clientID, "redirect_uri": "http://platform.example.test/app/auth/callback", "created_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("platform_oauth_post_logout_redirect_uri").Create(map[string]any{"oauth_client_id": clientID, "post_logout_redirect_uri": "http://platform.example.test/app/logged-out", "created_at": now}).Error; err != nil {
		t.Fatal(err)
	}

	coordinator, _ := New(db)
	coordinator.now = func() time.Time { return now }
	if _, err := coordinator.Initialize(ctx, ModeHTTP, "http://platform.example.test", "http://sso.example.test"); err != nil {
		t.Fatal(err)
	}
	transition, _, err := coordinator.Begin(ctx, BeginInput{TargetMode: ModeHTTPS, TargetPlatformOrigin: "https://platform.example.test", TargetSSOOrigin: "https://sso.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	resumed, _, err := coordinator.Begin(ctx, BeginInput{TargetMode: ModeHTTPS, TargetPlatformOrigin: "https://platform.example.test", TargetSSOOrigin: "https://sso.example.test"})
	if err != nil || resumed.ID != transition.ID {
		t.Fatalf("restart resume transition=%s want=%s err=%v", resumed.ID, transition.ID, err)
	}
	if _, _, err := coordinator.Begin(ctx, BeginInput{TargetMode: ModeHTTP, TargetPlatformOrigin: "http://platform.example.test", TargetSSOOrigin: "http://sso.example.test"}); err == nil {
		t.Fatal("conflicting target was accepted while transition is active")
	}
	var dual int64
	if err := db.Table("platform_oauth_redirect_uri").Where("oauth_client_id = ?", clientID).Count(&dual).Error; err != nil || dual != 2 {
		t.Fatalf("dual callbacks=%d err=%v", dual, err)
	}
	if err := coordinator.MarkExternalPrepared(ctx, transition.ID); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.CommitControlPlane(ctx, transition.ID); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Finalize(ctx, transition.ID); err != nil {
		t.Fatal(err)
	}
	state, err := coordinator.Status(ctx)
	if err != nil || state.State != StateHTTPS || state.PlatformOrigin != "https://platform.example.test" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	var redirects []string
	if err := db.Table("platform_oauth_redirect_uri").Where("oauth_client_id = ?", clientID).Pluck("redirect_uri", &redirects).Error; err != nil || len(redirects) != 1 || redirects[0] != "https://platform.example.test/app/auth/callback" {
		t.Fatalf("redirects=%v err=%v", redirects, err)
	}

	drain := now.Add(time.Hour)
	userID, accountID, sessionID := "01KPTUSER00000000000000001", "01KPTACCT00000000000000001", "01KPTSESS00000000000000001"
	familyID, refreshID := "01KPTFAM000000000000000001", "01KPTREFR00000000000000001"
	if err := db.Table("iam_user").Create(map[string]any{"id": userID, "tenant_id": tenantID, "display_name": "Transport User", "employment_status": "ACTIVE", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("iam_account").Create(map[string]any{"id": accountID, "tenant_id": tenantID, "user_id": userID, "username": "transport-user", "account_type": "EMPLOYEE", "auth_source": "LOCAL", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	farFuture := now.Add(24 * time.Hour)
	if err := db.Table("iam_session").Create(map[string]any{"id": sessionID, "tenant_id": tenantID, "account_id": accountID, "oauth_client_id": clientID, "created_at": now, "last_seen_at": now, "last_interactive_at": now, "expires_at": farFuture, "status": "ACTIVE"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("oauth_token_family").Create(map[string]any{"id": familyID, "tenant_id": tenantID, "oauth_client_id": clientID, "session_id": sessionID, "account_id": accountID, "user_id": userID, "scope": "openid", "authorized_at": now, "created_at": now, "expires_at": farFuture, "status": "ACTIVE"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("oauth_refresh_token").Create(map[string]any{"id": refreshID, "tenant_id": tenantID, "oauth_client_id": clientID, "token_family_id": familyID, "token_hash": make([]byte, 32), "issued_at": now, "expires_at": farFuture, "status": "ACTIVE"}).Error; err != nil {
		t.Fatal(err)
	}
	downgrade, _, err := coordinator.Begin(ctx, BeginInput{TargetMode: ModeHTTP, TargetPlatformOrigin: "http://platform.example.test", TargetSSOOrigin: "http://sso.example.test", DrainUntil: &drain})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{"iam_session": sessionID, "oauth_token_family": familyID, "oauth_refresh_token": refreshID}
	for _, table := range []string{"iam_session", "oauth_token_family", "oauth_refresh_token"} {
		var expiry time.Time
		if err := db.Table(table).Select("expires_at").Where("id = ?", ids[table]).Scan(&expiry).Error; err != nil || !expiry.Equal(drain) {
			t.Fatalf("%s expiry=%s want=%s err=%v", table, expiry, drain, err)
		}
	}
	if err := coordinator.MarkExternalPrepared(ctx, downgrade.ID); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.CommitControlPlane(ctx, downgrade.ID); err == nil {
		t.Fatal("downgrade committed before drain deadline")
	}
	coordinator.now = func() time.Time { return drain.Add(time.Second) }
	if err := coordinator.CommitControlPlane(ctx, downgrade.ID); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Finalize(ctx, downgrade.ID); err != nil {
		t.Fatal(err)
	}
}
