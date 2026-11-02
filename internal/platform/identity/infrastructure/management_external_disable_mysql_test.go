package infrastructure_test

import (
	"context"
	"testing"

	identitydomain "github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// 外部提供方停用对账（IAM-02 对齐）：DisableExternalIdentity 必须在同一事务内完成与停用用户/
// 账号一致的完整撤销——平台会话、OAuth 刷新令牌族/刷新令牌、未消费授权码，以及面向已登录
// RP 的 Back-Channel Logout 入队，并且重复对账幂等。
//
// 与同目录其它 MySQL 用例一样，只在显式配置的隔离测试库上运行；未配置
// PLATFORM_IDENTITY_TEST_DSN 时跳过，绝不连接业务库。
func TestDisableExternalIdentityRevokesFullAccountStateOnMySQL(t *testing.T) {
	database := revocationTestDatabase(t)
	repository := revocationRepository(t, database)
	ctx := context.Background()
	fixture := newRevocationFixture(t, database, "external-disable")

	if err := repository.DisableExternalIdentity(ctx, fixture.tenantID, fixture.userID); err != nil {
		t.Fatalf("DisableExternalIdentity() error = %v", err)
	}

	assertAccountFullyRevoked(t, database, fixture)

	// 既有停用语义不变：用户、账号与任职都停用，账号登录名被清空。
	var userStatus string
	if err := database.Table("iam_user").Select("status").Where("id = ?", fixture.userID).Scan(&userStatus).Error; err != nil {
		t.Fatalf("read externally disabled user status: %v", err)
	}
	if userStatus != identitydomain.StatusDisabled {
		t.Fatalf("user status = %q, want %s", userStatus, identitydomain.StatusDisabled)
	}
	var accountLifecycle struct {
		Status   string
		Username *string
	}
	if err := database.Table("iam_account").Select("status", "username").Where("id = ?", fixture.accountID).Take(&accountLifecycle).Error; err != nil {
		t.Fatalf("read externally disabled account: %v", err)
	}
	if accountLifecycle.Status != identitydomain.StatusDisabled {
		t.Fatalf("account status = %q, want %s", accountLifecycle.Status, identitydomain.StatusDisabled)
	}
	if accountLifecycle.Username != nil {
		t.Fatalf("externally disabled account username = %q, want cleared", *accountLifecycle.Username)
	}

	// 重复对账必须幂等：已经撤销的派生物不再命中，回信道登出不会重复入队。
	if err := repository.DisableExternalIdentity(ctx, fixture.tenantID, fixture.userID); err != nil {
		t.Fatalf("repeated DisableExternalIdentity() error = %v", err)
	}
	var outbox int64
	if err := database.Table("platform_oauth_backchannel_logout_outbox").
		Where("oauth_client_id = ? AND session_id = ?", fixture.clientID, fixture.sessionID).Count(&outbox).Error; err != nil {
		t.Fatalf("count back-channel logout outbox rows after repeated reconcile: %v", err)
	}
	if outbox != 1 {
		t.Fatalf("back-channel logout outbox rows after repeated reconcile = %d, want 1", outbox)
	}
}
