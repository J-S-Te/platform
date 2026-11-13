package infrastructure_test

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	identitystore "github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	oidcapplication "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	oidcstore "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/infrastructure"
	sessiondomain "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	sessionstore "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type codeLockFixture struct {
	revocationFixture
	db       *gorm.DB
	oidc     *oidcstore.Repository
	identity *identitystore.GORMRepository
	command  oidcapplication.ConsumeAuthorizationCodeCommand
	now      time.Time
}

// newCodeLockFixture 建立中央周期及可真实消费的隔离授权码和刷新令牌图。
// 参数：t为测试上下文，suffix隔离两个场景；仅使用显式测试DSN。
// 返回值：真实仓储和命令；夹具失败时终止测试，不读取真实凭据。
func newCodeLockFixture(t *testing.T, suffix string) codeLockFixture {
	t.Helper()
	db := revocationTestDatabase(t)
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	fixture := newRevocationFixture(t, db, suffix)
	t.Cleanup(func() {
		for _, table := range []string{"platform_oauth_backchannel_logout_outbox",
			"platform_oauth_backchannel_logout_uri"} {
			if err := db.Table(table).Where("oauth_client_id = ?", fixture.clientID).
				Delete(map[string]any{}).Error; err != nil {
				t.Error("clean isolated logout target:", err)
			}
		}
		_ = db.Table("sgov_platform_link").Where("iam_session_id = ?", fixture.sessionID).
			Delete(map[string]any{}).Error
		_ = db.Table("sgov_session").Where("id = ?", fixture.sessionID).Delete(map[string]any{}).Error
	})
	now := time.Now().UTC().Truncate(time.Millisecond)
	central, err := sessionstore.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		_, err := sessionstore.LinkPlatformTx(tx, sessiondomain.PlatformAnchor{
			ID: fixture.sessionID, TenantID: fixture.tenantID, IdentityID: fixture.userID,
			AuthenticatedAt: now, LastInteractiveAt: now, ExpiresAt: now.Add(2 * time.Hour),
		}, "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := revocationRepository(t, db)
	identity.EnableCentralSessions(central)
	oidc, err := oidcstore.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	oidc.EnableCentralSessions()
	if err := db.Table("platform_oauth_grant_type").Create(map[string]any{
		"oauth_client_id": fixture.clientID, "grant_type": "authorization_code",
	}).Error; err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(fixture.codeID))
	if err := db.Table("oauth_authorization_code").Where("id = ?", fixture.codeID).
		Updates(map[string]any{"code_hash": hash[:], "created_at": now,
			"authenticated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	command := oidcapplication.ConsumeAuthorizationCodeCommand{CodeHash: hash,
		ClientID: fixture.clientID, RedirectURI: "https://rp.invalid/callback",
		Refresh: &oidcapplication.NewRefreshToken{ID: "new-refresh-" + suffix,
			TokenFamilyID: "new-family-" + suffix, TokenHash: sha256.Sum256([]byte(suffix)),
			TenantID: fixture.tenantID, OAuthClientID: fixture.clientID,
			SessionID: fixture.sessionID, AccountID: fixture.accountID, UserID: fixture.userID,
			Scopes: []string{"openid", "profile"}, AuthorizedAt: now, IssuedAt: now,
			ExpiresAt: now.Add(time.Hour)}}
	return codeLockFixture{fixture, db, oidc, identity, command, now}
}

// beginCodeExchange 在专用测试库内异步执行真实授权码消费事务。
// 参数：fixture为隔离命令及仓储，不含任何真实授权码或Token。
// 返回值：有界消费结果通道，超时不会无限占用数据库锁。
func beginCodeExchange(fixture codeLockFixture) <-chan error {
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := fixture.oidc.ConsumeAuthorizationCode(ctx, fixture.command, fixture.now)
		result <- err
	}()
	return result
}

// TestCodeExchangeLocksIdentityBeforeCodeMySQL 验证换码不先持有码锁再等待人员。
// 参数：t为专用隔离MySQL上下文，覆盖真实换码和后续正常退出。
// 返回值：无；反向持有码锁、换码失败或新令牌未被退出撤销时失败。
func TestCodeExchangeLocksIdentityBeforeCodeMySQL(t *testing.T) {
	fixture := newCodeLockFixture(t, "sg-code")
	tx := fixture.db.Begin()
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var user struct{ ID string }
	if err := tx.Table("iam_user").Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ?", fixture.userID).Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	callback := "qa:code-identity-order"
	if err := fixture.db.Callback().Query().Before("gorm:query").Register(callback,
		func(query *gorm.DB) {
			if !query.DryRun && query.Statement.Clauses["FOR"].Expression != nil &&
				(query.Statement.Table == "iam_user" || query.Statement.Table == "iam_session") {
				once.Do(func() { close(waiting) })
			}
		}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.db.Callback().Query().Remove(callback) })
	result := beginCodeExchange(fixture)
	select {
	case <-waiting:
	case err := <-result:
		t.Fatalf("exchange ended before identity/session lock: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not reach identity/session lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := fixture.db.WithContext(ctx).Transaction(func(probe *gorm.DB) error {
		var code struct{ ID string }
		return probe.Table("oauth_authorization_code").
			Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ?", fixture.codeID).
			Take(&code).Error
	})
	cancel()
	_ = tx.Rollback().Error
	if exchangeErr := <-result; exchangeErr != nil {
		t.Fatal("exchange failed after reader release:", exchangeErr)
	}
	if err != nil {
		t.Fatal("code was locked before identity:", err)
	}
	assertNewExchangeRevoked(t, fixture)
}

// TestLogoutSeesConcurrentCodeExchangeMySQL 验证等待换码后退出能看到新提交的刷新族。
// 参数：t为专用隔离数据库上下文，按人员锁串行化真实换码与退出。
// 返回值：无；新刷新令牌漏撤销、通知缺失或任一事务失败时失败。
func TestLogoutSeesConcurrentCodeExchangeMySQL(t *testing.T) {
	fixture := newCodeLockFixture(t, "sg-late")
	ready, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(resume) }) })
	if err := fixture.db.Callback().Create().Before("gorm:create").Register("qa:pause-family",
		func(query *gorm.DB) {
			if query.Statement.Table == "oauth_token_family" {
				close(ready)
				select {
				case <-resume:
				case <-query.Statement.Context.Done():
				}
			}
		}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.db.Callback().Create().Remove("qa:pause-family") })
	exchange := beginCodeExchange(fixture)
	select {
	case <-ready:
	case err := <-exchange:
		t.Fatalf("exchange ended before creating the family: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not reach family creation")
	}
	waiting := make(chan struct{})
	var signaled sync.Once
	if err := fixture.db.Callback().Query().Before("gorm:query").Register("qa:logout-wait",
		func(query *gorm.DB) {
			if query.Statement.Table == "iam_user" && query.Statement.Clauses["FOR"].Expression != nil {
				signaled.Do(func() { close(waiting) })
			}
		}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.db.Callback().Query().Remove("qa:logout-wait") })
	logout := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logout <- fixture.identity.RevokeAccountSessions(ctx, fixture.tenantID,
			fixture.accountID, fixture.now.Add(time.Second), "USER_LOGOUT")
	}()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("logout did not wait for the original exchange")
	}
	once.Do(func() { close(resume) })
	if err := <-exchange; err != nil {
		t.Fatal("exchange:", err)
	}
	if err := <-logout; err != nil {
		t.Fatal("logout:", err)
	}
	assertExchangedTokensRevoked(t, fixture)
}

// assertNewExchangeRevoked 执行一次普通退出并验证刚签发令牌已撤销。
// 参数：t为测试上下文，fixture为已成功换码的隔离身份。
// 返回值：无；退出或令牌与通知核验失败时终止测试。
func assertNewExchangeRevoked(t *testing.T, fixture codeLockFixture) {
	t.Helper()
	if err := fixture.identity.RevokeAccountSessions(context.Background(), fixture.tenantID,
		fixture.accountID, fixture.now.Add(time.Second), "USER_LOGOUT"); err != nil {
		t.Fatal(err)
	}
	assertExchangedTokensRevoked(t, fixture)
}

// assertExchangedTokensRevoked 复核新刷新节点、令牌族和唯一退出通知。
// 参数：t为测试上下文，fixture为真实换码并已退出的隔离夹具。
// 返回值：无；任何新材料遗漏撤销或通知不唯一时失败。
func assertExchangedTokensRevoked(t *testing.T, fixture codeLockFixture) {
	t.Helper()
	for table, id := range map[string]string{"oauth_refresh_token": fixture.command.Refresh.ID,
		"oauth_token_family": fixture.command.Refresh.TokenFamilyID} {
		var row struct{ Status string }
		if err := fixture.db.Table(table).Where("id = ?", id).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Status != "REVOKED" {
			t.Fatalf("new %s was not revoked: %s", table, row.Status)
		}
	}
	var count int64
	if err := fixture.db.Table("platform_oauth_backchannel_logout_outbox").
		Where("session_id = ? AND oauth_client_id = ? AND status = 'PENDING'",
			fixture.sessionID, fixture.clientID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("one durable logout notification required: count=%d err=%v", count, err)
	}
}
