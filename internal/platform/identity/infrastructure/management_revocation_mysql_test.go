package infrastructure_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identitydomain "github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// IAM-02：停用/删除用户、停用账号必须在同一业务事务内完成与离职执行一致的完整撤销
// （平台会话 + OAuth 刷新令牌族/令牌 + 未消费授权码 + RP Back-Channel Logout 入队），
// 任一撤销失败要回滚整个业务写入。
//
// 该用例只在显式配置的隔离测试库上运行；未配置 PLATFORM_IDENTITY_TEST_DSN 时跳过，
// 绝不连接业务库。

// revocationFixture 是一次撤销用例所需的完整最小数据图。
type revocationFixture struct {
	tenantID   string
	userID     string
	accountID  string
	sessionID  string
	clientID   string
	familyID   string
	refreshID  string
	codeID     string
	orgUnitID  string
	positionID string
	membership string
}

func revocationTestDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_IDENTITY_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_IDENTITY_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open isolated test database: %v", err)
	}
	if _, err := migration.Run(context.Background(), database, migrations.Files); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return database
}

// newRevocationFixture 建立租户、应用/环境/客户端、组织/岗位/任职、用户/账号/会话以及
// 一套活跃的 OAuth 令牌族；测试结束时按外键顺序清理。
func newRevocationFixture(t *testing.T, database *gorm.DB, suffix string) revocationFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	fixture := revocationFixture{
		tenantID:   "rev-tenant-" + suffix,
		userID:     "rev-user-" + suffix,
		accountID:  "rev-account-" + suffix,
		sessionID:  "rev-session-" + suffix,
		clientID:   "rev-client-" + suffix,
		familyID:   "rev-family-" + suffix,
		refreshID:  "rev-refresh-" + suffix,
		codeID:     "rev-code-" + suffix,
		orgUnitID:  "rev-org-" + suffix,
		positionID: "rev-position-" + suffix,
		membership: "rev-membership-" + suffix,
	}

	create := func(table string, values map[string]any) {
		if err := database.Table(table).Create(values).Error; err != nil {
			t.Fatalf("create %s fixture: %v", table, err)
		}
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			"DELETE FROM platform_oauth_backchannel_logout_outbox WHERE oauth_client_id = ?",
			"DELETE FROM platform_oauth_backchannel_logout_uri WHERE oauth_client_id = ?",
			"DELETE FROM oauth_authorization_code WHERE tenant_id = ?",
			"DELETE FROM oauth_refresh_token WHERE tenant_id = ?",
			"DELETE FROM oauth_token_family WHERE tenant_id = ?",
			"DELETE FROM iam_session WHERE tenant_id = ?",
			"DELETE FROM platform_oauth_client WHERE tenant_id = ?",
			"DELETE FROM platform_application_environment WHERE tenant_id = ?",
			"DELETE FROM platform_application WHERE tenant_id = ?",
			"DELETE FROM iam_membership WHERE tenant_id = ?",
			"DELETE FROM iam_position WHERE tenant_id = ?",
			"DELETE FROM iam_account WHERE tenant_id = ?",
			"DELETE FROM iam_user WHERE tenant_id = ?",
			"DELETE FROM iam_org_unit WHERE tenant_id = ?",
			"DELETE FROM iam_tenant WHERE id = ?",
		} {
			if err := database.Exec(statement, fixture.tenantID).Error; err != nil {
				t.Logf("clean revocation fixture (%s): %v", statement, err)
			}
		}
	})

	create("iam_tenant", map[string]any{"id": fixture.tenantID, "code": fixture.tenantID, "name": "Revocation test tenant", "timezone": "UTC", "locale": "en-US", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now})
	create("platform_application", map[string]any{"id": "rev-app-" + suffix, "tenant_id": fixture.tenantID, "code": "rev-app-" + suffix, "name": "Revocation test app", "application_type": "PLATFORM", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now})
	create("platform_application_environment", map[string]any{"id": "rev-env-" + suffix, "tenant_id": fixture.tenantID, "application_id": "rev-app-" + suffix, "environment": "prod", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now})
	create("platform_oauth_client", map[string]any{
		"id": fixture.clientID, "tenant_id": fixture.tenantID, "application_id": "rev-app-" + suffix, "environment_id": "rev-env-" + suffix,
		"client_id": "rev-client-" + suffix, "client_name": "Revocation test client", "client_type": "CONFIDENTIAL",
		"token_auth_method": "CLIENT_SECRET_BASIC", "access_token_ttl_seconds": 900, "refresh_token_ttl_seconds": 86400,
		"require_pkce": 1, "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now,
	})
	create("platform_oauth_backchannel_logout_uri", map[string]any{"oauth_client_id": fixture.clientID, "logout_uri": "https://rp.invalid/logout/back-channel", "created_at": now, "updated_at": now})
	create("iam_org_unit", map[string]any{"id": fixture.orgUnitID, "tenant_id": fixture.tenantID, "code": "REV-" + suffix, "name": "Revocation test org", "org_type": "DEPARTMENT", "path": "/" + fixture.orgUnitID + "/", "depth": 1, "sort_order": 0, "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now})
	create("iam_position", map[string]any{"id": fixture.positionID, "tenant_id": fixture.tenantID, "org_unit_id": fixture.orgUnitID, "code": "REV-" + suffix, "name": "Revocation test position", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now})
	create("iam_user", map[string]any{"id": fixture.userID, "tenant_id": fixture.tenantID, "display_name": "Revocation test user", "employment_status": "ACTIVE", "status": "ACTIVE", "primary_org_id": fixture.orgUnitID, "version": 1, "created_at": now, "updated_at": now})
	create("iam_membership", map[string]any{"id": fixture.membership, "tenant_id": fixture.tenantID, "user_id": fixture.userID, "org_unit_id": fixture.orgUnitID, "position_id": fixture.positionID, "membership_type": "PRIMARY", "is_primary": 1, "status": "ACTIVE", "inherit_authorization": 1, "version": 1, "created_at": now, "updated_at": now})
	create("iam_account", map[string]any{"id": fixture.accountID, "tenant_id": fixture.tenantID, "user_id": fixture.userID, "username": "rev-user-" + suffix, "account_type": "HUMAN", "auth_source": "LOCAL", "status": "ACTIVE", "version": 1, "created_at": now, "updated_at": now})
	create("iam_session", map[string]any{
		"id": fixture.sessionID, "tenant_id": fixture.tenantID, "account_id": fixture.accountID, "oauth_client_id": fixture.clientID,
		"created_at": now, "last_seen_at": now, "last_interactive_at": now, "expires_at": now.Add(2 * time.Hour), "status": "ACTIVE",
	})
	create("oauth_token_family", map[string]any{
		"id": fixture.familyID, "tenant_id": fixture.tenantID, "oauth_client_id": fixture.clientID, "session_id": fixture.sessionID,
		"account_id": fixture.accountID, "user_id": fixture.userID, "scope": "openid profile", "authorized_at": now,
		"created_at": now, "expires_at": now.Add(24 * time.Hour), "status": "ACTIVE",
	})
	create("oauth_refresh_token", map[string]any{
		"id": fixture.refreshID, "tenant_id": fixture.tenantID, "oauth_client_id": fixture.clientID, "token_family_id": fixture.familyID,
		"token_hash": []byte(fmt.Sprintf("%-32s", "refresh-"+suffix)), "issued_at": now, "expires_at": now.Add(24 * time.Hour), "status": "ACTIVE",
	})
	create("oauth_authorization_code", map[string]any{
		"id": fixture.codeID, "tenant_id": fixture.tenantID, "oauth_client_id": fixture.clientID, "session_id": fixture.sessionID,
		"account_id": fixture.accountID, "user_id": fixture.userID, "code_hash": []byte(fmt.Sprintf("%-32s", "code-"+suffix)),
		"redirect_uri": "https://rp.invalid/callback", "scope": "openid profile", "created_at": now, "expires_at": now.Add(5 * time.Minute), "status": "ACTIVE",
	})
	return fixture
}

// assertAccountFullyRevoked 断言账号的会话、令牌族/刷新令牌与授权码全部失效，并存在一条
// 与该会话绑定的 Back-Channel Logout 投递任务。
func assertAccountFullyRevoked(t *testing.T, database *gorm.DB, fixture revocationFixture) {
	t.Helper()
	var sessionStatus string
	if err := database.Table("iam_session").Select("status").Where("id = ?", fixture.sessionID).Scan(&sessionStatus).Error; err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if sessionStatus != "REVOKED" {
		t.Fatalf("session status = %q, want REVOKED", sessionStatus)
	}

	var familyStatus string
	if err := database.Table("oauth_token_family").Select("status").Where("id = ?", fixture.familyID).Scan(&familyStatus).Error; err != nil {
		t.Fatalf("read token family status: %v", err)
	}
	if familyStatus != "REVOKED" {
		t.Fatalf("OAuth token family status = %q, want REVOKED", familyStatus)
	}

	var refreshStatus string
	if err := database.Table("oauth_refresh_token").Select("status").Where("id = ?", fixture.refreshID).Scan(&refreshStatus).Error; err != nil {
		t.Fatalf("read refresh token status: %v", err)
	}
	if refreshStatus != "REVOKED" {
		t.Fatalf("OAuth refresh token status = %q, want REVOKED", refreshStatus)
	}

	var codeStatus string
	if err := database.Table("oauth_authorization_code").Select("status").Where("id = ?", fixture.codeID).Scan(&codeStatus).Error; err != nil {
		t.Fatalf("read authorization code status: %v", err)
	}
	if codeStatus != "REVOKED" {
		t.Fatalf("OAuth authorization code status = %q, want REVOKED", codeStatus)
	}

	var outbox int64
	if err := database.Table("platform_oauth_backchannel_logout_outbox").
		Where("oauth_client_id = ? AND session_id = ? AND subject_id = ? AND status = ?", fixture.clientID, fixture.sessionID, fixture.userID, "PENDING").
		Count(&outbox).Error; err != nil {
		t.Fatalf("count back-channel logout outbox rows: %v", err)
	}
	if outbox != 1 {
		t.Fatalf("back-channel logout outbox rows = %d, want 1", outbox)
	}
}

func revocationRepository(t *testing.T, database *gorm.DB) *infrastructure.GORMRepository {
	t.Helper()
	repository, err := infrastructure.NewGORMRepository(database)
	if err != nil {
		t.Fatalf("NewGORMRepository: %v", err)
	}
	return repository
}

// 停用用户：账号、任职、会话、令牌族/刷新令牌与授权码全部失效，并登记回信道登出。
func TestDisableUserRevokesSessionsAndOAuthTokensOnMySQL(t *testing.T) {
	database := revocationTestDatabase(t)
	repository := revocationRepository(t, database)
	ctx := context.Background()
	fixture := newRevocationFixture(t, database, "disable-user")

	disabled := identitydomain.StatusDisabled
	if _, err := repository.UpdateUser(ctx, application.UserUpdateInput{
		TenantID: fixture.tenantID, OperatorID: "rev-operator", UserID: fixture.userID,
		DisplayName: "Revocation test user", Status: &disabled, Version: 1,
	}, nil, nil); err != nil {
		t.Fatalf("UpdateUser(disable) error = %v", err)
	}

	assertAccountFullyRevoked(t, database, fixture)

	var accountStatus string
	if err := database.Table("iam_account").Select("status").Where("id = ?", fixture.accountID).Scan(&accountStatus).Error; err != nil {
		t.Fatalf("read account status: %v", err)
	}
	if accountStatus != identitydomain.StatusDisabled {
		t.Fatalf("account status = %q, want %s", accountStatus, identitydomain.StatusDisabled)
	}
}

// 删除用户：与停用一致，删除也必须撤销全部登录派生物。
func TestDeleteUserRevokesSessionsAndOAuthTokensOnMySQL(t *testing.T) {
	database := revocationTestDatabase(t)
	repository := revocationRepository(t, database)
	ctx := context.Background()
	fixture := newRevocationFixture(t, database, "delete-user")

	if err := repository.DeleteUser(ctx, application.UserDeleteInput{
		TenantID: fixture.tenantID, OperatorID: "rev-operator", UserID: fixture.userID, Version: 1,
	}); err != nil {
		t.Fatalf("DeleteUser() error = %v", err)
	}

	assertAccountFullyRevoked(t, database, fixture)
}

// 停用账号：账号级停用同样撤销令牌族、刷新令牌、授权码并登记回信道登出。
func TestDisableAccountRevokesSessionsAndOAuthTokensOnMySQL(t *testing.T) {
	database := revocationTestDatabase(t)
	repository := revocationRepository(t, database)
	ctx := context.Background()
	fixture := newRevocationFixture(t, database, "disable-account")

	if _, err := repository.UpdateAccount(ctx, application.AccountUpdateInput{
		TenantID: fixture.tenantID, OperatorID: "rev-operator", AccountID: fixture.accountID,
		Status: identitydomain.StatusDisabled, Version: 1,
	}); err != nil {
		t.Fatalf("UpdateAccount(disable) error = %v", err)
	}

	assertAccountFullyRevoked(t, database, fixture)
}

// 撤销失败必须回滚整个业务写入：这里用只针对本用例的唯一触发器让回信道登出 Outbox 插入
// 失败，再断言用户仍为 ACTIVE、令牌族仍有效（没有任何部分撤销被提交）。
func TestDeleteUserRollsBackWhenRevocationFailsOnMySQL(t *testing.T) {
	database := revocationTestDatabase(t)
	repository := revocationRepository(t, database)
	ctx := context.Background()
	fixture := newRevocationFixture(t, database, "rollback")
	trigger := "rev_fail_backchannel_" + fixture.tenantID

	// 触发器是 DDL，无法参与事务；创建失败（例如测试账号无 TRIGGER 权限）时跳过注入用例，
	// 而不是把环境限制误报成产品缺陷。
	if err := database.Exec("CREATE TRIGGER " + trigger + " BEFORE INSERT ON platform_oauth_backchannel_logout_outbox FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected back-channel logout failure'").Error; err != nil {
		t.Skipf("cannot create failure injection trigger: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Exec("DROP TRIGGER IF EXISTS " + trigger).Error; err != nil {
			t.Errorf("drop failure injection trigger: %v", err)
		}
	})

	if err := repository.DeleteUser(ctx, application.UserDeleteInput{
		TenantID: fixture.tenantID, OperatorID: "rev-operator", UserID: fixture.userID, Version: 1,
	}); err == nil {
		t.Fatal("DeleteUser must fail when back-channel logout cannot be enqueued")
	}

	var userLifecycle struct {
		Status    string
		DeletedAt *time.Time
	}
	if err := database.Table("iam_user").Select("status", "deleted_at").Where("id = ?", fixture.userID).Take(&userLifecycle).Error; err != nil {
		t.Fatalf("read user after rollback: %v", err)
	}
	if userLifecycle.Status != identitydomain.StatusActive || userLifecycle.DeletedAt != nil {
		t.Fatalf("user write was not rolled back: status=%q deleted_at=%v", userLifecycle.Status, userLifecycle.DeletedAt)
	}

	var familyStatus string
	if err := database.Table("oauth_token_family").Select("status").Where("id = ?", fixture.familyID).Scan(&familyStatus).Error; err != nil {
		t.Fatalf("read token family after rollback: %v", err)
	}
	if familyStatus != "ACTIVE" {
		t.Fatalf("token family status = %q, want ACTIVE after rollback", familyStatus)
	}

	var sessionStatus string
	if err := database.Table("iam_session").Select("status").Where("id = ?", fixture.sessionID).Scan(&sessionStatus).Error; err != nil {
		t.Fatalf("read session after rollback: %v", err)
	}
	if sessionStatus != "ACTIVE" {
		t.Fatalf("session status = %q, want ACTIVE after rollback", sessionStatus)
	}
}
