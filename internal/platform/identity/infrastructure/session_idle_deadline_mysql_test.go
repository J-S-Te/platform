package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"github.com/J-S-Te/Basic-Platform/migrations"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// parseSessionIdleTestDSN 限定本回归只能连接专用回环高位端口测试库。
// 参数：dsn 为调用方显式提供的专用测试连接串，不读取其他配置。
// 返回值：规范化连接配置；任何边界不符均返回不含连接串的错误。
func parseSessionIdleTestDSN(dsn string) (*driver.Config, error) {
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("invalid dedicated idle test DSN")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || config.Net != "tcp" || host != "127.0.0.1" ||
		port < 32768 || port > 65535 || config.DBName != "platform_test_extpdf_idle" {
		return nil, errors.New("idle test requires dedicated loopback high-port database")
	}
	config.ParseTime = true
	config.Loc = time.UTC
	config.ClientFoundRows = false // 必须观察真实 MySQL no-op 的零变更行语义。
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 30 * time.Second
	config.WriteTimeout = 30 * time.Second
	return config, nil
}

// TestSessionIdleDSNGuard 验证连接边界拒绝业务库、默认端口和非回环地址。
// 参数：t 为测试上下文；所有连接串均为不发起连接的固定测试样例。
// 返回值：无；边界放宽或误拒合法样例时报告失败。
func TestSessionIdleDSNGuard(t *testing.T) {
	for _, tc := range []struct {
		address, database string
		valid             bool
	}{
		{"127.0.0.1:55001", "platform_test_extpdf_idle", true},
		{"127.0.0.1:3306", "platform_test_extpdf_idle", false},
		{"127.0.0.1:0", "platform_test_extpdf_idle", false},
		{"127.0.0.1:65536", "platform_test_extpdf_idle", false},
		{"localhost:55001", "platform_test_extpdf_idle", false},
		{"[::1]:55001", "platform_test_extpdf_idle", false},
		{"192.0.2.1:55001", "platform_test_extpdf_idle", false},
		{"127.0.0.1:55001", "platform", false},
	} {
		dsn := fmt.Sprintf("fixture:fixture@tcp(%s)/%s", tc.address, tc.database)
		config, err := parseSessionIdleTestDSN(dsn)
		if (err == nil) != tc.valid {
			t.Fatal("dedicated idle DSN boundary mismatch")
		}
		if err == nil && (!config.ParseTime || config.ClientFoundRows || config.Loc != time.UTC) {
			t.Fatal("idle test must retain UTC DATETIME and zero changed-row semantics")
		}
	}
}

// sessionIdleMySQLDatabase 仅从专用变量建立隔离连接并应用项目真实迁移。
// 参数：t 为测试上下文，ctx 为迁移和后续测试的有限运行上下文。
// 返回值：已迁移的测试连接；缺配置时跳过，非法配置或连接失败时终止。
func sessionIdleMySQLDatabase(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_IDENTITY_IDLE_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated PLATFORM_IDENTITY_IDLE_TEST_DSN is not configured")
	}
	config, err := parseSessionIdleTestDSN(dsn)
	if err != nil {
		t.Fatal("refusing non-isolated idle test DSN")
	}
	database, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal("open dedicated idle MySQL database failed")
	}
	sqlDatabase, err := database.DB()
	if err != nil {
		t.Fatal("read dedicated idle MySQL handle failed")
	}
	sqlDatabase.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	if _, err := migration.Run(ctx, database, migrations.Files); err != nil {
		t.Fatalf("apply dedicated idle test migrations: %v", err)
	}
	return database.WithContext(ctx)
}

type sessionIdleMySQLFixture struct {
	tenantID, userID, accountID, sessionID string
	now                                    time.Time
	repository                             *GORMRepository
	database                               *gorm.DB
}

// newSessionIdleMySQLFixture 创建独立最小租户身份图并登记按外键顺序清理。
// 参数：t 为测试上下文，database 为专用测试库，fixed 为固定会话有效时长。
// 返回值：已提交的真实仓储夹具；提交使并发读取使用不同连接且能看到同一会话。
func newSessionIdleMySQLFixture(t *testing.T, database *gorm.DB, fixed time.Duration) sessionIdleMySQLFixture {
	t.Helper()
	fixture := sessionIdleMySQLFixture{now: time.Now().UTC().Truncate(time.Millisecond), database: database}
	for _, id := range []*string{&fixture.tenantID, &fixture.userID, &fixture.accountID, &fixture.sessionID} {
		value, err := ulid.New(fixture.now)
		if err != nil {
			t.Fatal("create dedicated idle fixture identifier failed")
		}
		*id = value
	}
	t.Cleanup(func() {
		for _, table := range []string{"iam_session", "iam_account", "iam_user", "iam_tenant"} {
			column, id := "tenant_id", fixture.tenantID
			if table == "iam_tenant" {
				column = "id"
			}
			if err := database.Table(table).Where(column+" = ?", id).Delete(map[string]any{}).Error; err != nil {
				t.Errorf("clean dedicated idle fixture %s: %v", table, err)
			}
		}
	})
	createSessionIdleMySQLIdentity(t, database, fixture)
	session := sessionModel{
		ID: fixture.sessionID, TenantID: fixture.tenantID, AccountID: fixture.accountID,
		CreatedAt: fixture.now.Add(-time.Minute), LastSeenAt: fixture.now.Add(-time.Minute),
		LastInteractiveAt: fixture.now.Add(-time.Minute), ExpiresAt: fixture.now.Add(fixed),
		Status: domain.StatusActive,
	}
	if err := database.Create(&session).Error; err != nil {
		t.Fatalf("create dedicated idle session: %v", err)
	}
	repository, err := NewGORMRepository(database)
	if err != nil {
		t.Fatal("create dedicated idle repository failed")
	}
	fixture.repository = repository
	return fixture
}

// createSessionIdleMySQLIdentity 插入不含密码或令牌的测试租户、用户和账号。
// 参数：t 为测试上下文，database 为专用测试连接，fixture 提供独立标识和时间。
// 返回值：无；任一插入失败时终止测试，由预登记清理处理部分夹具。
func createSessionIdleMySQLIdentity(t *testing.T, database *gorm.DB, fixture sessionIdleMySQLFixture) {
	t.Helper()
	rows := []struct {
		table  string
		values map[string]any
	}{
		{"iam_tenant", map[string]any{
			"id": fixture.tenantID, "code": "idle-" + fixture.tenantID,
			"name": "Idle regression tenant", "timezone": "UTC", "locale": "zh-CN",
		}},
		{"iam_user", map[string]any{
			"id": fixture.userID, "tenant_id": fixture.tenantID,
			"display_name": "Idle regression user", "employment_status": domain.StatusActive,
		}},
		{"iam_account", map[string]any{
			"id": fixture.accountID, "tenant_id": fixture.tenantID, "user_id": fixture.userID,
			"username": "idle-" + fixture.accountID, "account_type": "HUMAN", "auth_source": "LOCAL",
		}},
	}
	for _, row := range rows {
		row.values["status"] = domain.StatusActive
		row.values["version"] = 1
		row.values["created_at"] = fixture.now
		row.values["updated_at"] = fixture.now
		if err := database.Table(row.table).Create(row.values).Error; err != nil {
			t.Fatalf("create dedicated idle identity %s: %v", row.table, err)
		}
	}
}

// assertSessionIdleMySQLTimes 验证读取或续 Cookie 没有推进交互和固定到期。
// 参数：t 为测试上下文，fixture 为会话夹具，interactive 和 expires 为独立预期时间。
// 返回值：持久化会话状态；时间边界不符时报告失败。
func assertSessionIdleMySQLTimes(t *testing.T, fixture sessionIdleMySQLFixture, interactive, expires time.Time) sessionModel {
	t.Helper()
	var stored sessionModel
	if err := fixture.database.Where("id = ?", fixture.sessionID).Take(&stored).Error; err != nil {
		t.Fatalf("read dedicated idle session timestamps: %v", err)
	}
	if !stored.LastInteractiveAt.Equal(interactive) || !stored.ExpiresAt.Equal(expires) {
		t.Fatal("ordinary read or cookie refresh changed interactive/fixed expiry timestamps")
	}
	return stored
}

// TestSessionIdleDeadlineMySQL 验证真实迁移和仓储下的共享空闲期限及并发读取。
// 参数：t 为测试上下文，仅消费显式配置的专用隔离 DSN。
// 返回值：无；未配置则跳过，任一实际存储或时间边界不符时报告失败。
func TestSessionIdleDeadlineMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database := sessionIdleMySQLDatabase(t, ctx)
	for _, tc := range []struct {
		name                   string
		idle, fixed, remaining time.Duration
		unauthenticated        bool
	}{
		{"remaining 29 minutes", 30 * time.Minute, time.Hour, 29 * time.Minute, false},
		{"fixed five minute cap", 30 * time.Minute, 5 * time.Minute, 5 * time.Minute, false},
		{"shortened two minute policy", 2 * time.Minute, time.Hour, time.Minute, false},
		{"shortened one minute expires", time.Minute, time.Hour, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSessionIdleMySQLFixture(t, database, tc.fixed)
			principal, err := fixture.repository.FindPrincipalBySession(ctx, fixture.sessionID, fixture.now, tc.idle)
			if tc.unauthenticated {
				if !errors.Is(err, application.ErrUnauthenticated) {
					t.Fatalf("shortened idle policy error = %v", err)
				}
			} else if err != nil || !principal.SessionIdleDeadline.Equal(fixture.now.Add(tc.remaining)) {
				t.Fatalf("authoritative remaining deadline mismatch; error = %v", err)
			}
			stored := assertSessionIdleMySQLTimes(t, fixture, fixture.now.Add(-time.Minute), fixture.now.Add(tc.fixed))
			if !tc.unauthenticated && !stored.LastSeenAt.Equal(fixture.now) {
				t.Fatal("ordinary read did not update last_seen_at")
			}
			if tc.unauthenticated && (stored.Status != "REVOKED" || stored.RevokedAt == nil) {
				t.Fatal("expired idle policy did not follow authoritative revocation")
			}
		})
	}
	t.Run("two concurrent same millisecond no-op reads", func(t *testing.T) {
		fixture := newSessionIdleMySQLFixture(t, database, time.Hour)
		assertSessionIdleMySQLConcurrentReads(t, ctx, fixture)
	})
	t.Run("real interaction advances only idle window", func(t *testing.T) {
		fixture := newSessionIdleMySQLFixture(t, database, time.Hour)
		at := fixture.now.Add(time.Minute)
		if err := fixture.repository.RecordSessionInteraction(ctx, fixture.sessionID, at, 30*time.Minute); err != nil {
			t.Fatalf("record real session interaction: %v", err)
		}
		principal, err := fixture.repository.FindPrincipalBySession(ctx, fixture.sessionID, at, 30*time.Minute)
		if err != nil || !principal.SessionIdleDeadline.Equal(fixture.now.Add(31*time.Minute)) {
			t.Fatalf("real interaction did not produce new 30 minute window; error = %v", err)
		}
		assertSessionIdleMySQLTimes(t, fixture, at, fixture.now.Add(time.Hour))
	})
	t.Run("fixed expiry cannot be renewed", func(t *testing.T) {
		fixture := newSessionIdleMySQLFixture(t, database, 5*time.Minute)
		assertSessionIdleMySQLFixedExpiry(t, ctx, fixture)
	})
}

// assertSessionIdleMySQLConcurrentReads 验证两次并发 no-op 身份读取均能通过持久化复核。
// 参数：t 为测试上下文，ctx 为运行期限，fixture 为已提交的独立会话图。
// 返回值：无；MySQL 行数语义、身份读取或不可变时间异常时报告失败。
func assertSessionIdleMySQLConcurrentReads(t *testing.T, ctx context.Context, fixture sessionIdleMySQLFixture) {
	t.Helper()
	update := fixture.database.Model(&sessionModel{}).Where("id = ?", fixture.sessionID)
	if err := update.Update("last_seen_at", fixture.now).Error; err != nil {
		t.Fatalf("prepare same millisecond last_seen: %v", err)
	}
	noOp := update.Update("last_seen_at", fixture.now)
	if noOp.Error != nil || noOp.RowsAffected != 0 {
		t.Fatal("dedicated MySQL must report zero changed rows for same millisecond no-op")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index := 0; index < 2; index++ {
		go func() {
			<-start
			principal, err := fixture.repository.FindPrincipalBySession(ctx, fixture.sessionID, fixture.now, 30*time.Minute)
			if err == nil && !principal.SessionIdleDeadline.Equal(fixture.now.Add(29*time.Minute)) {
				err = errors.New("concurrent authoritative idle deadline mismatch")
			}
			results <- err
		}()
	}
	close(start)
	for index := 0; index < 2; index++ {
		if err := <-results; err != nil {
			t.Errorf("concurrent same millisecond read failed: %v", err)
		}
	}
	assertSessionIdleMySQLTimes(t, fixture, fixture.now.Add(-time.Minute), fixture.now.Add(time.Hour))
}

// assertSessionIdleMySQLFixedExpiry 验证交互和刷新 Cookie 永远不能越过固定到期。
// 参数：t 为测试上下文，ctx 为运行期限，fixture 为固定五分钟的会话图。
// 返回值：无；固定期限延长或到期仍被接受时报告失败。
func assertSessionIdleMySQLFixedExpiry(t *testing.T, ctx context.Context, fixture sessionIdleMySQLFixture) {
	t.Helper()
	at := fixture.now.Add(4 * time.Minute)
	expires := fixture.now.Add(5 * time.Minute)
	if err := fixture.repository.RecordSessionInteraction(ctx, fixture.sessionID, at, 30*time.Minute); err != nil {
		t.Fatalf("record pre-expiry interaction: %v", err)
	}
	principal, err := fixture.repository.FindPrincipalBySession(ctx, fixture.sessionID, at, 30*time.Minute)
	if err != nil || !principal.SessionIdleDeadline.Equal(expires) {
		t.Fatalf("fixed expiry cap lost after interaction; error = %v", err)
	}
	refreshed, err := fixture.repository.RefreshSession(ctx, fixture.sessionID, at.Add(time.Millisecond))
	if err != nil || !refreshed.Equal(expires) {
		t.Fatalf("refresh changed fixed expiry; error = %v", err)
	}
	if err := fixture.repository.RecordSessionInteraction(ctx, fixture.sessionID, expires, 30*time.Minute); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("interaction at fixed expiry error = %v", err)
	}
	if _, err := fixture.repository.FindPrincipalBySession(ctx, fixture.sessionID, expires, 30*time.Minute); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("read at fixed expiry error = %v", err)
	}
	if _, err := fixture.repository.RefreshSession(ctx, fixture.sessionID, expires); !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("refresh at fixed expiry error = %v", err)
	}
	assertSessionIdleMySQLTimes(t, fixture, at, expires)
}
