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

	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"github.com/J-S-Te/Basic-Platform/migrations"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// parseOIDCSessionIdleTestDSN 限定 M-1 回归只能连接专用回环高位端口测试库。
// 参数：dsn 为调用方显式提供的专用测试连接串，不读取其他配置。
// 返回值：规范化连接配置；任何边界不符都返回不含连接串的错误，避免误连业务库。
func parseOIDCSessionIdleTestDSN(dsn string) (*driver.Config, error) {
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("invalid dedicated OIDC idle test DSN")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || config.Net != "tcp" || host != "127.0.0.1" ||
		port < 32768 || port > 65535 || config.DBName != "platform_oidc_test_idle" {
		return nil, errors.New("OIDC idle test requires dedicated loopback high-port database")
	}
	config.ParseTime = true
	config.Loc = time.UTC
	// 幂等性夹具按真实迁移文件整篇执行，需要多语句支持；仅作用于本测试连接。
	config.MultiStatements = true
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 30 * time.Second
	config.WriteTimeout = 30 * time.Second
	return config, nil
}

// TestOIDCSessionIdleDSNGuard 验证连接边界拒绝业务库、默认端口和非回环地址。
// 参数：t 为测试上下文；所有连接串均为不发起连接的固定样例。
// 返回值：无；边界放宽或误拒合法样例时报告失败。该用例不需要真实 MySQL。
func TestOIDCSessionIdleDSNGuard(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		address, database string
		valid             bool
	}{
		{"127.0.0.1:55002", "platform_oidc_test_idle", true},
		{"127.0.0.1:3306", "platform_oidc_test_idle", false},
		{"127.0.0.1:0", "platform_oidc_test_idle", false},
		{"127.0.0.1:65536", "platform_oidc_test_idle", false},
		{"localhost:55002", "platform_oidc_test_idle", false},
		{"192.0.2.1:55002", "platform_oidc_test_idle", false},
		{"127.0.0.1:55002", "platform", false},
	} {
		dsn := fmt.Sprintf("fixture:fixture@tcp(%s)/%s", test.address, test.database)
		config, err := parseOIDCSessionIdleTestDSN(dsn)
		if (err == nil) != test.valid {
			t.Fatalf("dedicated OIDC idle DSN boundary mismatch for %s/%s", test.address, test.database)
		}
		if err == nil && (!config.ParseTime || config.Loc != time.UTC) {
			t.Fatal("OIDC idle test must read DATETIME as UTC")
		}
	}
}

// oidcSessionIdleMySQLDatabase 只从专用变量建立隔离连接并应用项目真实迁移。
// 参数：t 为测试上下文，ctx 为迁移与后续用例的有限运行上下文。
// 返回值：已迁移的测试连接；缺配置时跳过，非法配置或连接失败时终止。
func oidcSessionIdleMySQLDatabase(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_OIDC_IDLE_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated PLATFORM_OIDC_IDLE_TEST_DSN is not configured")
	}
	config, err := parseOIDCSessionIdleTestDSN(dsn)
	if err != nil {
		t.Fatal("refusing non-isolated OIDC idle test DSN")
	}
	database, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal("open dedicated OIDC idle MySQL database failed")
	}
	sqlDatabase, err := database.DB()
	if err != nil {
		t.Fatal("read dedicated OIDC idle MySQL handle failed")
	}
	sqlDatabase.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	applyOIDCSessionIdleSchema(t, ctx, database)
	return database.WithContext(ctx)
}

// oidcSessionIdleSchemaFiles 只列出本查询直接依赖的真实迁移文件：租户、应用登记、
// 身份（user/account/password_credential/session）、授权（000014 的外键依赖），
// sec_login_policy，以及新增 idle_timeout_seconds 与 last_interactive_at 的两条变更。
// 这里刻意不跑完整 migration.Run：全量迁移中的 000049 在本机 MySQL 8.0.46 上会因既有
// 排序规则混用（utf8mb4_0900_ai_ci 与 utf8mb4_bin）失败，与 M-1 无关，且迁移不在本任务
// 授权范围内修改；本用例只需验证会话空闲门禁，因此按依赖顺序整篇执行相关真实 DDL。
var oidcSessionIdleSchemaFiles = []string{
	"000001_create_tenant.sql",
	"000002_create_application_registry.sql",
	"000003_create_identity.sql",
	"000006_create_authorization.sql",
	"000014_create_login_security.sql",
	"000039_add_login_idle_timeout_policy.sql",
	"000041_add_account_valid_until.sql",
	"000042_add_session_last_interactive_at.sql",
}

// oidcSessionIdleSeedSQL 补插 000014 种子数据外键引用的平台租户与内建应用（取自
// 000011_seed_platform_defaults.sql 的同名两行）。本用例不执行配置、审计、组织等与
// 会话空闲无关的迁移，因此只保留这两行最小种子的语义。
const oidcSessionIdleSeedSQL = `
INSERT INTO iam_tenant (id, code, name, timezone, locale, status, version, created_at, updated_at)
VALUES ('01J00000000000000000000000', 'default', '默认租户', 'Asia/Shanghai', 'zh-CN', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;
INSERT INTO platform_application (id, tenant_id, code, name, application_type, description, status, version, created_at, updated_at)
VALUES ('01J00000000000000000000001', '01J00000000000000000000000', 'platform', '基础能力平台', 'backend', '基础能力平台内建应用', 'ACTIVE', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))
ON DUPLICATE KEY UPDATE id = id;
`

// applyOIDCSessionIdleSchema 按依赖顺序执行相关真实迁移文件，并在 000014 之前补齐它
// 依赖的两行最小种子。该 DSN 必须指向专用空数据库；迁移文件本身不是幂等的，因此对
// 已应用过本用例 schema 的库只做探测后跳过，避免重复 ALTER 直接失败。
// 参数：t 为测试上下文，ctx 为执行上下文，database 为已连接的专用测试库。
// 返回值：无；文件缺失或 DDL/DML 失败都终止测试。
func applyOIDCSessionIdleSchema(t *testing.T, ctx context.Context, database *gorm.DB) {
	t.Helper()
	var probe struct{ Count int }
	if err := database.WithContext(ctx).Raw(`SELECT COUNT(*) AS count FROM information_schema.columns
		WHERE table_schema = DATABASE() AND ((table_name = 'iam_session' AND column_name = 'last_interactive_at')
		OR (table_name = 'sec_login_policy' AND column_name = 'idle_timeout_seconds'))`).Scan(&probe).Error; err != nil {
		t.Fatalf("probe dedicated OIDC idle schema: %v", err)
	}
	if probe.Count == 2 {
		return
	}
	for _, name := range oidcSessionIdleSchemaFiles {
		if name == "000014_create_login_security.sql" {
			if err := database.WithContext(ctx).Exec(oidcSessionIdleSeedSQL).Error; err != nil {
				t.Fatalf("seed minimal platform tenant/application: %v", err)
			}
		}
		content, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := database.WithContext(ctx).Exec(string(content)).Error; err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
}

// TestResolveSessionSubjectIdlePolicyMySQL 在真实迁移与真实 MySQL 上验证 M-1 的空闲
// 语义与身份侧一致：策略行缺失用默认 1800 秒、策略为 0 失败关闭、租户策略更短时按
// 租户值判定、恰好等于截止时刻视为超窗。
// 参数：t 为测试上下文，仅消费显式配置的专用隔离 DSN。
// 返回值：无；未配置则跳过，任一语义偏差都报告失败。
func TestResolveSessionSubjectIdlePolicyMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database := oidcSessionIdleMySQLDatabase(t, ctx)
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatal("create dedicated OIDC repository failed")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)

	for _, test := range []struct {
		name string
		// policy 为 nil 时表示该租户没有 sec_login_policy 行。
		policy          *uint
		lastInteractive time.Duration
		wantSubject     bool
	}{
		{name: "policy row missing uses 1800 second default", lastInteractive: -29 * time.Minute, wantSubject: true},
		{name: "policy row missing beyond default rejects", lastInteractive: -31 * time.Minute},
		{name: "explicit zero policy fails closed", policy: pointerTo[uint](0), lastInteractive: -time.Minute},
		{name: "tenant policy shorter than default rejects", policy: pointerTo[uint](600), lastInteractive: -11 * time.Minute},
		{name: "tenant policy shorter than default accepts within window", policy: pointerTo[uint](600), lastInteractive: -5 * time.Minute, wantSubject: true},
		{name: "exact idle cutoff is outside the window", policy: pointerTo[uint](600), lastInteractive: -600 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOIDCSessionIdleFixture(t, database, now, test.lastInteractive, test.policy)
			subject, resolveErr := repository.ResolveSessionSubject(ctx, fixture.sessionID, now)
			if !test.wantSubject {
				if resolveErr == nil {
					t.Fatalf("ResolveSessionSubject() = %+v, want rejection", subject)
				}
				return
			}
			if resolveErr != nil {
				t.Fatalf("ResolveSessionSubject() error = %v", resolveErr)
			}
			if subject.TenantID != fixture.tenantID || subject.SessionID != fixture.sessionID ||
				subject.AccountID != fixture.accountID || subject.UserID != fixture.userID {
				t.Fatalf("subject = %+v", subject)
			}
		})
	}
}

type oidcSessionIdleFixture struct {
	tenantID, userID, accountID, sessionID string
}

// newOIDCSessionIdleFixture 建立独立最小租户身份图与一个活跃会话，并按外键顺序登记清理。
// 参数：t 为测试上下文，database 为专用测试库，now 为固定判定时间，
// lastInteractive 为相对 now 的最后交互偏移，policy 为 nil 时不写入策略行。
// 返回值：本次夹具的标识集合。
func newOIDCSessionIdleFixture(t *testing.T, database *gorm.DB, now time.Time, lastInteractive time.Duration, policy *uint) oidcSessionIdleFixture {
	t.Helper()
	fixture := oidcSessionIdleFixture{}
	for _, id := range []*string{&fixture.tenantID, &fixture.userID, &fixture.accountID, &fixture.sessionID} {
		value, err := ulid.New(now)
		if err != nil {
			t.Fatal("create dedicated OIDC idle fixture identifier failed")
		}
		*id = value
	}
	t.Cleanup(func() {
		for _, table := range []string{"iam_session", "iam_account", "iam_user", "sec_login_policy", "iam_tenant"} {
			column, id := "tenant_id", fixture.tenantID
			if table == "iam_tenant" {
				column = "id"
			}
			if err := database.Table(table).Where(column+" = ?", id).Delete(map[string]any{}).Error; err != nil {
				t.Errorf("clean dedicated OIDC idle fixture %s: %v", table, err)
			}
		}
	})
	for _, row := range []struct {
		table  string
		values map[string]any
	}{
		{"iam_tenant", map[string]any{
			"id": fixture.tenantID, "code": "oidc-idle-" + fixture.tenantID, "name": "OIDC idle regression tenant",
			"timezone": "UTC", "locale": "zh-CN",
		}},
		{"iam_user", map[string]any{
			"id": fixture.userID, "tenant_id": fixture.tenantID,
			"display_name": "OIDC idle regression user", "employment_status": "ACTIVE",
		}},
		{"iam_account", map[string]any{
			"id": fixture.accountID, "tenant_id": fixture.tenantID, "user_id": fixture.userID,
			"username": "oidc-idle-" + fixture.accountID, "account_type": "HUMAN", "auth_source": "LOCAL",
		}},
	} {
		row.values["status"] = activeStatus
		row.values["version"] = 1
		row.values["created_at"] = now
		row.values["updated_at"] = now
		if err := database.Table(row.table).Create(row.values).Error; err != nil {
			t.Fatalf("create dedicated OIDC idle identity %s: %v", row.table, err)
		}
	}
	if policy != nil {
		if err := database.Table("sec_login_policy").Create(map[string]any{
			"tenant_id": fixture.tenantID, "max_failed_attempts": 5, "lockout_duration_seconds": 900,
			"failure_reset_window_seconds": 1800, "idle_timeout_seconds": *policy, "version": 1,
			"created_at": now, "updated_at": now,
		}).Error; err != nil {
			t.Fatalf("create dedicated OIDC idle login policy: %v", err)
		}
	}
	if err := database.Table("iam_session").Create(map[string]any{
		"id": fixture.sessionID, "tenant_id": fixture.tenantID, "account_id": fixture.accountID,
		"created_at": now.Add(-time.Hour), "last_seen_at": now.Add(lastInteractive),
		"last_interactive_at": now.Add(lastInteractive), "expires_at": now.Add(time.Hour), "status": activeStatus,
	}).Error; err != nil {
		t.Fatalf("create dedicated OIDC idle session: %v", err)
	}
	return fixture
}

func pointerTo[T any](value T) *T { return &value }
