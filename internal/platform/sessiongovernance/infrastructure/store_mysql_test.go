package infrastructure

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// openSessionTestStore 创建本轮专用容器中的中央会话测试表。
// 参数：t为当前用例；连接仅从显式SESSION_GOVERNANCE_TEST_DSN读取。
// 返回值：存储；未配置时跳过，迁移或连接错误时终止用例。
func openSessionTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("SESSION_GOVERNANCE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated SESSION_GOVERNANCE_TEST_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS sec_login_policy (
		tenant_id VARCHAR(64) PRIMARY KEY, idle_timeout_seconds INT UNSIGNED NOT NULL,
		version BIGINT UNSIGNED NOT NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	items, err := migration.Load(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Version != 133 {
			continue
		}
		statements, err := migration.SplitStatements(item.SQL)
		if err != nil {
			t.Fatal(err)
		}
		for _, sql := range statements {
			if err := db.Exec(sql).Error; err != nil {
				t.Fatal("apply session migration:", err)
			}
		}
	}
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	seedCentralIdentity(t, store)
	return store
}

// seedCentralIdentity 在已迁移的隔离库创建独立租户、人员和登录账号。
// 参数：t为测试上下文，store为本轮显式容器中的存储。
// 返回值：无；没有工程真实IAM表或夹具创建失败时终止。
func seedCentralIdentity(t *testing.T, store *Store) {
	t.Helper()
	now := time.Now().UTC()
	rows := []struct {
		table  string
		values map[string]any
	}{
		{"iam_tenant", map[string]any{"id": "tenant-session-qa", "code": "tenant-session-qa", "name": "session isolated QA",
			"timezone": "UTC", "locale": "zh-CN", "status": "ACTIVE", "created_at": now, "updated_at": now}},
		{"iam_user", map[string]any{"id": "identity-session-qa", "tenant_id": "tenant-session-qa", "display_name": "isolated QA",
			"employment_status": "ACTIVE", "status": "ACTIVE", "created_at": now, "updated_at": now}},
		{"iam_account", map[string]any{"id": "account-session-qa", "tenant_id": "tenant-session-qa", "user_id": "identity-session-qa",
			"account_type": "EMPLOYEE", "auth_source": "KEYCLOAK", "status": "ACTIVE", "created_at": now, "updated_at": now}},
	}
	for _, row := range rows {
		if err := store.db.Table(row.table).Clauses(clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"id"})}).Create(row.values).Error; err != nil {
			t.Fatal("create isolated IAM fixture:", err)
		}
	}
}

// TestCentralBindingMySQL 验证跨RP共享窗口、固定截止与到期不可复活。
// 参数：t为测试上下文。
// 返回值：无；任一可信绑定或期限不符即失败。
func TestCentralBindingMySQL(t *testing.T) {
	s := openSessionTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	identity := VerifiedIdentity{Issuer: "https://issuer.test", SID: "sid-" + now.Format("150405.000"),
		TenantID: "tenant-session-qa", IdentityID: "identity-session-qa", Subject: "native-subject", AuthenticatedAt: now}
	client := Client{ID: "contract-session-qa", TenantID: identity.TenantID,
		ApplicationCode: "contract", EnvironmentCode: "test"}
	ticket, err := s.Begin(ctx, client, "nonce-hash", "", now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Complete(ctx, ticket, "nonce-hash", client.ID, identity, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(ctx, ticket, "nonce-hash", client.ID, identity, now); !errors.Is(err, domain.ErrInactive) {
		t.Fatalf("ticket replay must fail: %v", err)
	}
	client.ID, client.ApplicationCode = "crm-session-qa", "crm"
	ticket, err = s.Begin(ctx, client, "nonce-hash", "", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Complete(ctx, ticket, "nonce-hash", client.ID, identity, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.Context.SessionID != second.Context.SessionID {
		t.Fatal("same issuer/sid must share one anchor")
	}
	if _, err = s.State(ctx, first.Token, now.Add(29*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx, second.Token, now.Add(58*time.Minute), false)
	if err != nil || state.IdleRemainingMS != time.Minute.Milliseconds() {
		t.Fatalf("cross app activity missing: %+v %v", state, err)
	}
	if _, err = s.State(ctx, second.Token, now.Add(59*time.Minute), true); !errors.Is(err, domain.ErrInactive) {
		t.Fatal("expired anchor resurrected", err)
	}
	if _, err = s.State(ctx, first.Token, now.Add(8*time.Hour), true); !errors.Is(err, domain.ErrInactive) {
		t.Fatal("absolute boundary accepted", err)
	}
}

// TestCentralTicketConcurrencyMySQL 验证真实数据库并发只允许一次票据消费。
// 参数：t为测试上下文。
// 返回值：无；消费数量、身份错位或事务错误不符即失败。
func TestCentralTicketConcurrencyMySQL(t *testing.T) {
	s := openSessionTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	client := Client{ID: "concurrent-client", TenantID: "tenant-session-qa", ApplicationCode: "contract", EnvironmentCode: "test"}
	identity := VerifiedIdentity{Issuer: "https://issuer.test", SID: "concurrent-" + now.Format("150405.000"),
		TenantID: client.TenantID, IdentityID: "identity-session-qa", Subject: "native-subject", AuthenticatedAt: now}
	ticket, err := s.Begin(ctx, client, "nonce-hash", "", now)
	if err != nil {
		t.Fatal(err)
	}
	bad := identity
	bad.TenantID = "other-tenant"
	if _, err = s.Complete(ctx, ticket, "nonce-hash", client.ID, bad, now); !errors.Is(err, domain.ErrBindingConflict) {
		t.Fatal("tenant mismatch accepted", err)
	}
	var successes atomic.Int32
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := s.Complete(ctx, ticket, "nonce-hash", client.ID, identity, now)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, domain.ErrInactive) {
				t.Error("unexpected transaction error:", err)
			}
		}()
	}
	group.Wait()
	if successes.Load() != 1 {
		t.Fatalf("ticket committed %d times", successes.Load())
	}
}

// TestCentralParentRejectsReplacementSIDMySQL 验证同一浏览器锚点不会偷换同issuer的sid。
// 参数：t为真实隔离MySQL测试上下文。
// 返回值：无；第二个不同sid能附着原周期时失败。
func TestCentralParentRejectsReplacementSIDMySQL(t *testing.T) {
	s := openSessionTestStore(t)
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	identity := VerifiedIdentity{Issuer: "https://issuer.test", SID: "parent-" + now.Format("150405.000"),
		TenantID: "tenant-session-qa", IdentityID: "identity-session-qa", Subject: "native-subject", AuthenticatedAt: now}
	first, err := s.ResolveOIDC(ctx, identity, "", now)
	if err != nil {
		t.Fatal(err)
	}
	identity.SID += "-replacement"
	if _, err = s.ResolveOIDC(ctx, identity, first.ID, now); !errors.Is(err, domain.ErrBindingConflict) {
		t.Fatalf("replacement SID must conflict, got %v", err)
	}
}

// TestCentralIdentityDisableMySQL 验证尚未创建平台Cookie的周期也在线检查人员停用。
// 参数：t为真实隔离MySQL测试上下文。
// 返回值：无；停用人员仍能用服务器绑定查询或续期时失败。
func TestCentralIdentityDisableMySQL(t *testing.T) {
	s := openSessionTestStore(t)
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	client := Client{ID: "contract-session-qa", TenantID: "tenant-session-qa", ApplicationCode: "contract", EnvironmentCode: "test"}
	identity := VerifiedIdentity{Issuer: "https://issuer.test", SID: "disabled-" + now.Format("150405.000"),
		TenantID: client.TenantID, IdentityID: "identity-session-qa", Subject: "native-subject", AuthenticatedAt: now}
	ticket, err := s.Begin(ctx, client, "nonce-hash", "", now)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := s.Complete(ctx, ticket, "nonce-hash", client.ID, identity, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.db.Table("iam_user").Where("id = ?", identity.IdentityID).Update("status", "ACTIVE").Error
	})
	if err := s.db.Table("iam_user").Where("id = ?", identity.IdentityID).Update("status", "DISABLED").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.State(ctx, bound.Token, now.Add(time.Second), true); !errors.Is(err, domain.ErrInactive) {
		t.Fatalf("disabled identity must be inactive, got %v", err)
	}
}

// TestExpiredUpstreamWithNewParentMySQL 验证重新登录平台后旧上游仍触发精确失效处理。
// 参数：t为隔离MySQL测试上下文。
// 返回值：无；过期上游被误报为两个有效周期冲突，或得到新八小时即失败。
func TestExpiredUpstreamWithNewParentMySQL(t *testing.T) {
	s := openSessionTestStore(t)
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	identity := VerifiedIdentity{Issuer: "https://issuer.test", SID: "expired-parent-" + now.Format("150405.000"), TenantID: "tenant-session-qa", IdentityID: "identity-session-qa", Subject: "native-subject", AuthenticatedAt: now}
	first, err := s.ResolveOIDC(ctx, identity, "", now)
	if err != nil {
		t.Fatal(err)
	}
	newer := identity
	newer.SID += "-fresh"
	newer.AuthenticatedAt = now.Add(31 * time.Minute)
	parent, err := s.ResolveOIDC(ctx, newer, "", newer.AuthenticatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == parent.ID {
		t.Fatal("independent logins were merged")
	}
	_, err = s.ResolveOIDC(ctx, identity, parent.ID, newer.AuthenticatedAt)
	if !errors.Is(err, domain.ErrUpstreamInactive) {
		t.Fatalf("expired old upstream must request precise termination: %v", err)
	}
}
