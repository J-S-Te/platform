package infrastructure

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	securityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/security/application"
	securitydomain "github.com/J-S-Te/Basic-Platform/internal/platform/security/domain"
	securitystore "github.com/J-S-Te/Basic-Platform/internal/platform/security/infrastructure"
	sessiondomain "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	sessionstore "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type centralLockFixture struct {
	sessionIdleMySQLFixture
	central *sessionstore.Store
	binding sessionstore.Binding
}

// newCentralLockFixture 创建只连接专用高位端口数据库的中央会话夹具。
// 参数：t 为测试上下文，连接沿用显式 PLATFORM_IDENTITY_IDLE_TEST_DSN。
// 返回值：同一身份的平台会话和子系统绑定；初始化失败时终止测试。
func newCentralLockFixture(t *testing.T) centralLockFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	db := sessionIdleMySQLDatabase(t, ctx)
	fixture := newSessionIdleMySQLFixture(t, db, 8*time.Hour)
	central, err := sessionstore.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		_, err := sessionstore.LinkPlatformTx(tx, sessiondomain.PlatformAnchor{
			ID: fixture.sessionID, TenantID: fixture.tenantID, IdentityID: fixture.userID,
			AuthenticatedAt: fixture.now, LastInteractiveAt: fixture.now,
			ExpiresAt: fixture.now.Add(8 * time.Hour),
		}, "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	client := sessionstore.Client{ID: "project-lock-qa", TenantID: fixture.tenantID,
		ApplicationCode: "project", EnvironmentCode: "test"}
	ticket, err := central.Begin(ctx, client, "lock-test-nonce", fixture.sessionID, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := central.Complete(ctx, ticket, "lock-test-nonce", client.ID,
		sessionstore.VerifiedIdentity{Issuer: "https://lock-qa.test", SID: fixture.sessionID,
			Subject: fixture.userID, TenantID: fixture.tenantID, IdentityID: fixture.userID,
			AuthenticatedAt: fixture.now}, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	fixture.repository.EnableCentralSessions(central)
	t.Cleanup(func() {
		if err := db.Table("sec_login_attempt").Where("tenant_id = ?", fixture.tenantID).
			Delete(map[string]any{}).Error; err != nil {
			t.Error("clean isolated login attempts:", err)
		}
	})
	return centralLockFixture{fixture, central, binding}
}

// holdCentralReadLock 保留一个真实中央共享行锁以验证读写互斥。
// 参数：t 为测试上下文，fixture 为专用数据库内的中央会话。
// 返回值：持锁事务；测试结束自动回滚，调用方也可提前释放。
func holdCentralReadLock(t *testing.T, fixture centralLockFixture) *gorm.DB {
	t.Helper()
	tx := fixture.database.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var row sessiondomain.Session
	if err := tx.Table("sgov_session").Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ?", fixture.sessionID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return tx
}

// TestCentralSessionReadersShareLocksMySQL 验证普通状态查询并行且不推进期限。
// 参数：t 为只使用专用隔离 MySQL 的测试上下文。
// 返回值：无；只读被排他锁串行化、交互越锁或撤销失效时失败。
func TestCentralSessionReadersShareLocksMySQL(t *testing.T) {
	for _, platform := range []bool{false, true} {
		name := "subsystem"
		if platform {
			name = "platform"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newCentralLockFixture(t)
			tx := holdCentralReadLock(t, fixture)
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			read := func(ctx context.Context, interact bool) (sessionstore.Context, error) {
				if platform {
					return fixture.central.PlatformState(ctx, fixture.sessionID,
						fixture.now.Add(time.Second), interact)
				}
				return fixture.central.State(ctx, fixture.binding.Token,
					fixture.now.Add(time.Second), interact)
			}
			state, err := read(ctx, false)
			if err != nil {
				t.Fatalf("read-only session must coexist with another reader: %v", err)
			}
			if state.IdleRemainingMS != (30*time.Minute-time.Second).Milliseconds() ||
				!state.AbsoluteExpiresAt.Equal(fixture.now.Add(8*time.Hour)) {
				t.Fatal("read-only session changed the authoritative lifetime")
			}
			blocked, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_, err = read(blocked, true)
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("interaction must wait for shared readers: %v", err)
			}
			if err := tx.Rollback().Error; err != nil {
				t.Fatal(err)
			}
			assertCentralReadBoundaries(t, fixture, read)
		})
	}
}

// assertCentralReadBoundaries 检查普通读取未续期且撤销后两个入口均拒绝。
// 参数：t 为测试上下文，fixture 为中央夹具，read 为当前平台或子系统入口。
// 返回值：无；时间、版本或撤销语义不符时失败。
func assertCentralReadBoundaries(t *testing.T, fixture centralLockFixture,
	read func(context.Context, bool) (sessionstore.Context, error)) {
	t.Helper()
	var stored sessiondomain.Session
	if err := fixture.database.Table("sgov_session").
		Where("id = ?", fixture.sessionID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.LastInteractiveAt.Equal(fixture.now) || stored.Version != 1 {
		t.Fatal("read-only query or canceled interaction renewed the session")
	}
	if _, err := fixture.central.State(context.Background(), fixture.binding.Token,
		fixture.now.Add(30*time.Minute), false); !errors.Is(err, sessiondomain.ErrInactive) {
		t.Fatalf("idle expiry must reject read-only access: %v", err)
	}
	if _, err := read(context.Background(), true); err != nil {
		t.Fatal("interaction must succeed after readers release:", err)
	}
	if err := fixture.repository.RevokeSession(context.Background(), fixture.sessionID,
		fixture.now.Add(2*time.Second), "QA_LOCK_REVOKE"); err != nil {
		t.Fatal(err)
	}
	if _, err := read(context.Background(), false); !errors.Is(err, sessiondomain.ErrInactive) {
		t.Fatalf("revoked session must not be readable: %v", err)
	}
}

// TestSessionLoginLocksIdentityBeforeAccountMySQL 验证接管登录不再反向持有账号锁。
// 参数：t 为只连接显式隔离 MySQL 的测试上下文。
// 返回值：无；人员锁未取得就锁账号、接管失败或旧周期仍有效时失败。
func TestSessionLoginLocksIdentityBeforeAccountMySQL(t *testing.T) {
	fixture := newCentralLockFixture(t)
	tx := fixture.database.Begin()
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var user userModel
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ?", fixture.userID).Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	identityLock := make(chan struct{})
	var once sync.Once
	callback := "qa:identity-before-account-" + fixture.userID
	if err := fixture.database.Callback().Query().Before("gorm:query").Register(callback,
		func(query *gorm.DB) {
			if !query.DryRun && query.Statement.Table == "iam_user" &&
				query.Statement.Clauses["FOR"].Expression != nil {
				once.Do(func() { close(identityLock) })
			}
		}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.database.Callback().Query().Remove(callback) })
	result := beginReplacementLogin(t, fixture)
	select {
	case <-identityLock:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement login did not reach the identity lock")
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	probe := fixture.database.WithContext(probeCtx).Begin()
	var account accountModel
	err := probe.Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ?", fixture.accountID).Take(&account).Error
	_ = probe.Rollback().Error
	cancel()
	_ = tx.Rollback().Error
	if loginErr := <-result; loginErr != nil {
		t.Fatalf("replacement login failed after lock release: %v", loginErr)
	}
	if err != nil {
		t.Fatalf("login held the account lock while waiting for identity: %v", err)
	}
	if _, err := fixture.central.State(context.Background(), fixture.binding.Token,
		fixture.now.Add(2*time.Second), false); !errors.Is(err, sessiondomain.ErrInactive) {
		t.Fatalf("explicit replacement must still revoke the old central cycle: %v", err)
	}
}

// beginReplacementLogin 在专用夹具内并发执行一次正常显式接管。
// 参数：t 为测试上下文，fixture 为当前平台和中央周期。
// 返回值：有界登录结果通道；不会访问任何业务环境或真实凭据。
func beginReplacementLogin(t *testing.T, fixture centralLockFixture) <-chan error {
	t.Helper()
	now := fixture.now.Add(time.Second)
	id, err := ulid.New(now)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result <- fixture.repository.CreateSession(ctx, domain.LoginAccount{
			TenantID: fixture.tenantID, UserID: fixture.userID, AccountID: fixture.accountID,
		}, domain.Session{ID: id, TenantID: fixture.tenantID, AccountID: fixture.accountID,
			CreatedAt: now, ExpiresAt: now.Add(8 * time.Hour)}, 30*time.Minute, true)
	}()
	return result
}

// TestSessionRevocationLocksIdentityFirstMySQL 验证登录相关撤销不会反向等待人员锁。
// 参数：t为隔离MySQL上下文，覆盖主动退出、失败锁定与停用账号。
// 返回值：无；等待人员时已占用账号或会话锁，或撤销未生效时失败。
func TestSessionRevocationLocksIdentityFirstMySQL(t *testing.T) {
	for _, name := range []string{"logout", "failed-login", "disable-account"} {
		t.Run(name, func(t *testing.T) {
			fixture := newCentralLockFixture(t)
			mutation := func(ctx context.Context) error {
				now := fixture.now.Add(time.Second)
				switch name {
				case "logout":
					return fixture.repository.RevokeAccountSessions(ctx, fixture.tenantID,
						fixture.accountID, now, "USER_LOGOUT")
				case "disable-account":
					_, err := fixture.repository.UpdateAccount(ctx, application.AccountUpdateInput{
						TenantID: fixture.tenantID, AccountID: fixture.accountID,
						OperatorID: fixture.userID, Status: domain.StatusDisabled, Version: 1,
					})
					return err
				default:
					security, err := securitystore.NewGORMRepository(fixture.database)
					if err != nil {
						return err
					}
					security.EnableCentralSessions()
					_, err = security.RecordFailedLogin(ctx, securityapplication.LoginFailureInput{
						TenantID: fixture.tenantID, AccountID: fixture.accountID,
					}, securitydomain.LoginPolicy{MaxFailedAttempts: 1,
						LockoutDurationSeconds: 900, FailureResetWindowSeconds: 1800}, now)
					return err
				}
			}
			assertMutationIdentityOrder(t, fixture, mutation)
		})
	}
}

// assertMutationIdentityOrder 在人员共享锁下检查撤销事务的锁顺序和最终效果。
// 参数：t为测试上下文，fixture为中央夹具，mutation为真实仓储操作。
// 返回值：无；锁顺序、事务结果或撤销边界不符时失败。
func assertMutationIdentityOrder(t *testing.T, fixture centralLockFixture,
	mutation func(context.Context) error) {
	t.Helper()
	tx := fixture.database.Begin()
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var user userModel
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ?", fixture.userID).Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	identityLock := make(chan struct{})
	var once sync.Once
	callback := "qa:revocation-identity-" + fixture.userID
	if err := fixture.database.Callback().Query().Before("gorm:query").Register(callback,
		func(query *gorm.DB) {
			if !query.DryRun && query.Statement.Table == "iam_user" &&
				query.Statement.Clauses["FOR"].Expression != nil {
				once.Do(func() { close(identityLock) })
			}
		}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.database.Callback().Query().Remove(callback) })
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result <- mutation(ctx)
	}()
	select {
	case <-identityLock:
	case err := <-result:
		t.Fatalf("mutation ended before locking identity: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("mutation did not reach identity lock")
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := fixture.database.WithContext(probeCtx).Transaction(func(probe *gorm.DB) error {
		var account accountModel
		if err := probe.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("id = ?", fixture.accountID).Take(&account).Error; err != nil {
			return err
		}
		var session sessionModel
		return probe.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("id = ?", fixture.sessionID).Take(&session).Error
	})
	cancel()
	_ = tx.Rollback().Error
	if mutationErr := <-result; mutationErr != nil {
		t.Fatal("mutation failed after lock release:", mutationErr)
	}
	if err != nil {
		t.Fatal("account/session locked before identity:", err)
	}
	if _, err := fixture.central.State(context.Background(), fixture.binding.Token,
		fixture.now.Add(2*time.Second), false); !errors.Is(err, sessiondomain.ErrInactive) {
		t.Fatalf("revoked identity must reject the old subsystem binding: %v", err)
	}
}

// TestConcurrentFailedLoginsRetainThresholdMySQL 验证等待人员锁不会漏算并发失败次数。
// 参数：t为专用隔离数据库上下文，三次失败必须触发配置为三次的锁定。
// 返回值：无；旧快照漏算失败、操作报错或锁定未撤销旧周期时失败。
func TestConcurrentFailedLoginsRetainThresholdMySQL(t *testing.T) {
	fixture := newCentralLockFixture(t)
	security, err := securitystore.NewGORMRepository(fixture.database)
	if err != nil {
		t.Fatal(err)
	}
	security.EnableCentralSessions()
	tx := fixture.database.Begin()
	t.Cleanup(func() { _ = tx.Rollback().Error })
	var user userModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", fixture.userID).Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{}, 3)
	callback := "qa:failed-login-snapshot-" + fixture.userID
	if err := fixture.database.Callback().Query().Before("gorm:query").Register(callback,
		func(query *gorm.DB) {
			if !query.DryRun && query.Statement.Table == "iam_user" &&
				query.Statement.Clauses["FOR"].Expression != nil {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
		}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.database.Callback().Query().Remove(callback) })
	results := make(chan error, 3)
	for range 3 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := security.RecordFailedLogin(ctx, securityapplication.LoginFailureInput{
				TenantID: fixture.tenantID, AccountID: fixture.accountID,
			}, securitydomain.LoginPolicy{MaxFailedAttempts: 3,
				LockoutDurationSeconds: 900, FailureResetWindowSeconds: 1800},
				fixture.now.Add(time.Second))
			results <- err
		}()
	}
	for range 3 {
		select {
		case <-waiting:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent failures did not reach identity lock")
		}
	}
	_ = tx.Rollback().Error
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal("concurrent failed login:", err)
		}
	}
	var account accountModel
	if err := fixture.database.Where("id = ?", fixture.accountID).Take(&account).Error; err != nil {
		t.Fatal(err)
	}
	if account.LockedUntil == nil {
		t.Fatal("three serialized failures must reach the three-attempt lockout threshold")
	}
}
