package publictransport

import (
	"context"
	"errors"
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
	// 接入自动登记的 Back-Channel Logout 端点（BIZ-XEVT2-07）同样必须随域名迁移改写。
	if err := db.Table("platform_oauth_backchannel_logout_uri").Create(map[string]any{"oauth_client_id": clientID, "logout_uri": "http://platform.example.test/app/auth/backchannel-logout", "created_at": now, "updated_at": now}).Error; err != nil {
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
	// 单行登记表在应用阶段原地改写：遗留源值会让所有 logout_token 打到旧源域。
	var logoutURIs []string
	if err := db.Table("platform_oauth_backchannel_logout_uri").Where("oauth_client_id = ?", clientID).Pluck("logout_uri", &logoutURIs).Error; err != nil || len(logoutURIs) != 1 || logoutURIs[0] != "https://platform.example.test/app/auth/backchannel-logout" {
		t.Fatalf("back-channel logout URIs=%v err=%v", logoutURIs, err)
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
	ids := map[string]string{"iam_session": sessionID, "oauth_token_family": familyID, "oauth_refresh_token": refreshID}
	var registerDowngrade = func() Transition {
		downgrade, _, err := coordinator.Begin(ctx, BeginInput{TargetMode: ModeHTTP, TargetPlatformOrigin: "http://platform.example.test", TargetSSOOrigin: "http://sso.example.test", DrainUntil: &drain})
		if err != nil {
			t.Helper()
			t.Fatal(err)
		}
		for _, table := range sessionLifetimeTables {
			if got := sessionExpiry(t, db, table, ids[table]); !got.Equal(drain) {
				t.Fatalf("%s expiry=%s want=%s", table, got, drain)
			}
		}
		return downgrade
	}
	downgrade := registerDowngrade()

	// BIZ-RUNTIME-06：Begin 阶段必须先把原值写进 cap 表，再截断到期时间。
	for _, table := range sessionLifetimeTables {
		if original := sessionCapOriginal(t, db, downgrade.ID, table, ids[table]); !original.Equal(farFuture) {
			t.Fatalf("%s capped original=%s want=%s", table, original, farFuture)
		}
	}
	// 重复 Begin 幂等：同一目标复用同一切换，不重复截断、不重复记录。
	replayed, _, err := coordinator.Begin(ctx, BeginInput{TargetMode: ModeHTTP, TargetPlatformOrigin: "http://platform.example.test", TargetSSOOrigin: "http://sso.example.test", DrainUntil: &drain})
	if err != nil || replayed.ID != downgrade.ID {
		t.Fatalf("replayed Begin transition=%s want=%s err=%v", replayed.ID, downgrade.ID, err)
	}
	// 记录-截断语句本身也必须幂等：用更早的截止时间重跑不得改动已记录的原值或已截断的值。
	if err := db.Transaction(func(tx *gorm.DB) error {
		return capSessionLifetimes(tx, downgrade.ID, drain.Add(-time.Hour), now)
	}); err != nil {
		t.Fatalf("repeat cap: %v", err)
	}
	for _, table := range sessionLifetimeTables {
		if original := sessionCapOriginal(t, db, downgrade.ID, table, ids[table]); !original.Equal(farFuture) {
			t.Fatalf("%s original changed on repeat cap: %s", table, original)
		}
		if got := sessionExpiry(t, db, table, ids[table]); !got.Equal(drain) {
			t.Fatalf("%s expiry changed on repeat cap: %s", table, got)
		}
	}

	// 部分恢复守卫：模拟一条会话在截断后又被合法延长，Fail 必须跳过它而不是覆盖。
	extended := drain.Add(30 * time.Minute)
	if err := db.Table("iam_session").Where("id = ?", sessionID).Update("expires_at", extended).Error; err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Fail(ctx, downgrade.ID, "OPERATOR_ABORT", errors.New("operator aborted the downgrade"), "Keycloak callbacks restored"); err != nil {
		t.Fatalf("fail downgrade: %v", err)
	}
	if got := sessionExpiry(t, db, "iam_session", sessionID); !got.Equal(extended) {
		t.Fatalf("guarded session expiry=%s want unchanged %s", got, extended)
	}
	for _, table := range []string{"oauth_token_family", "oauth_refresh_token"} {
		if got := sessionExpiry(t, db, table, ids[table]); !got.Equal(farFuture) {
			t.Fatalf("%s expiry=%s want restored %s", table, got, farFuture)
		}
	}
	// 已恢复的 cap 行删除；被守卫跳过的行保留为证据。
	if count := sessionCapCount(t, db, downgrade.ID, "iam_session"); count != 1 {
		t.Fatalf("guarded cap rows=%d want 1", count)
	}
	for _, table := range []string{"oauth_token_family", "oauth_refresh_token"} {
		if count := sessionCapCount(t, db, downgrade.ID, table); count != 0 {
			t.Fatalf("%s restored cap rows=%d want 0", table, count)
		}
	}
	if state, err := coordinator.Status(ctx); err != nil || state.State != StateHTTPS || state.ActiveTransitionID != nil || state.DrainUntil != nil {
		t.Fatalf("state after failed downgrade=%+v err=%v", state, err)
	}

	// 放弃后重新开始同一次降级，走完既有的提交路径。
	if err := db.Table("iam_session").Where("id = ?", sessionID).Update("expires_at", farFuture).Error; err != nil {
		t.Fatal(err)
	}
	downgrade = registerDowngrade()
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
	// 控制面提交后仍拒绝自动回滚（forward recovery）：Fail 必须报错且不恢复到期时间。
	if err := coordinator.Fail(ctx, downgrade.ID, "OPERATOR_ABORT", errors.New("late abort"), "Keycloak callbacks restored"); err == nil {
		t.Fatal("abort after control-plane commit must be rejected")
	}
	for _, table := range sessionLifetimeTables {
		if got := sessionExpiry(t, db, table, ids[table]); !got.Equal(drain) {
			t.Fatalf("%s expiry=%s want capped %s after rejected abort", table, got, drain)
		}
		if count := sessionCapCount(t, db, downgrade.ID, table); count != 1 {
			t.Fatalf("%s cap rows=%d want 1 after rejected abort", table, count)
		}
	}
	if err := coordinator.Finalize(ctx, downgrade.ID); err != nil {
		t.Fatal(err)
	}
}

func sessionExpiry(t *testing.T, db *gorm.DB, table, id string) time.Time {
	t.Helper()
	var expiry time.Time
	if err := db.Table(table).Select("expires_at").Where("id = ?", id).Scan(&expiry).Error; err != nil {
		t.Fatalf("read %s expiry: %v", table, err)
	}
	return expiry
}

func sessionCapOriginal(t *testing.T, db *gorm.DB, transitionID, table, id string) time.Time {
	t.Helper()
	var original time.Time
	if err := db.Table(sessionCapTable).
		Select("original_expires_at").
		Where("transition_id = ? AND session_table = ? AND session_id = ?", transitionID, table, id).
		Scan(&original).Error; err != nil {
		t.Fatalf("read %s cap original: %v", table, err)
	}
	if original.IsZero() {
		t.Fatalf("%s cap row is missing for transition %s", table, transitionID)
	}
	return original
}

func sessionCapCount(t *testing.T, db *gorm.DB, transitionID, table string) int64 {
	t.Helper()
	var count int64
	if err := db.Table(sessionCapTable).Where("transition_id = ? AND session_table = ?", transitionID, table).Count(&count).Error; err != nil {
		t.Fatalf("count %s cap rows: %v", table, err)
	}
	return count
}
