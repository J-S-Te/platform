package backchannel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"github.com/J-S-Te/Basic-Platform/migrations"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// parseBackchannelTestDSN 限定登出恢复测试只能使用专用回环高位端口数据库。
// 参数：dsn 是调用方显式提供的隔离测试连接串，不读取业务配置。
// 返回值：规范化配置；地址或库名不符时返回不含连接串的错误。
func parseBackchannelTestDSN(dsn string) (*driver.Config, error) {
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("invalid dedicated backchannel test DSN")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || config.Net != "tcp" || host != "127.0.0.1" ||
		port < 32768 || port > 65535 || config.DBName != "platform_backchannel_test_recovery" {
		return nil, errors.New("backchannel test requires dedicated loopback high-port database")
	}
	config.ParseTime = true
	config.Loc = time.UTC
	config.MultiStatements = true
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 30 * time.Second
	config.WriteTimeout = 30 * time.Second
	return config, nil
}

// TestBackchannelTestDSNGuard 验证测试入口拒绝默认端口、远端地址和业务库名。
// 参数：t 为测试上下文；样例连接串不会发起连接。
// 返回值：无；隔离边界被放宽或合法样例被拒绝时失败。
func TestBackchannelTestDSNGuard(t *testing.T) {
	for _, sample := range []struct {
		address, schema string
		valid           bool
	}{
		{"127.0.0.1:55003", "platform_backchannel_test_recovery", true},
		{"127.0.0.1:3306", "platform_backchannel_test_recovery", false},
		{"localhost:55003", "platform_backchannel_test_recovery", false},
		{"192.0.2.1:55003", "platform_backchannel_test_recovery", false},
		{"127.0.0.1:55003", "platform", false},
		{"127.0.0.1:65536", "platform_backchannel_test_recovery", false},
	} {
		dsn := fmt.Sprintf("fixture:fixture@tcp(%s)/%s", sample.address, sample.schema)
		config, err := parseBackchannelTestDSN(dsn)
		if (err == nil) != sample.valid {
			t.Fatalf("DSN isolation guard mismatch for %s/%s", sample.address, sample.schema)
		}
		if err == nil && (!config.ParseTime || config.Loc != time.UTC) {
			t.Fatal("test DATETIME values must use UTC")
		}
	}
}

// backchannelMySQLDatabase 仅连接专用测试变量，并应用恢复链路依赖的四份真实迁移。
// 参数：t 为测试上下文，ctx 为连接、迁移与业务操作的有限上下文。
// 返回值：隔离连接；缺配置时跳过，配置不安全或执行失败时终止测试。
func backchannelMySQLDatabase(t *testing.T, ctx context.Context) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("BACKCHANNEL_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated BACKCHANNEL_TEST_DSN is not configured")
	}
	config, err := parseBackchannelTestDSN(dsn)
	if err != nil {
		t.Fatal("refusing non-isolated backchannel test DSN")
	}
	database, err := gorm.Open(mysql.Open(config.FormatDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal("open dedicated backchannel MySQL database failed")
	}
	sqlDatabase, err := database.DB()
	if err != nil {
		t.Fatal("read dedicated backchannel MySQL handle failed")
	}
	sqlDatabase.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDatabase.Close() })
	for _, name := range []string{
		"000001_create_tenant.sql", "000002_create_application_registry.sql",
		"000005_create_oauth_clients.sql", "000100_oidc_backchannel_logout.sql",
	} {
		content, readErr := migrations.Files.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read test migration %s: %v", name, readErr)
		}
		if err := database.WithContext(ctx).Exec(string(content)).Error; err != nil {
			t.Fatalf("apply test migration %s: %v", name, err)
		}
	}
	return database.WithContext(ctx)
}

type backchannelMySQLFixture struct {
	tenant, application, environment, client, message string
}

// newBackchannelMySQLFixture 创建独立租户和登出任务，并按外键顺序清理本次数据。
// 参数：t 为测试上下文，db 为隔离库，now 为固定业务时刻，attempts 为已用预算。
// 返回值：本轮夹具标识；任何插入失败都终止测试。
func newBackchannelMySQLFixture(t *testing.T, db *gorm.DB, now time.Time,
	attempts int,
) backchannelMySQLFixture {
	t.Helper()
	fixture := backchannelMySQLFixture{}
	for _, target := range []*string{
		&fixture.tenant, &fixture.application, &fixture.environment, &fixture.client, &fixture.message,
	} {
		id, err := ulid.New(now)
		if err != nil {
			t.Fatalf("generate isolated backchannel fixture ID: %v", err)
		}
		*target = id
	}
	t.Cleanup(func() { cleanBackchannelMySQLFixture(t, db, fixture) })
	rows := []struct {
		table string
		data  map[string]any
	}{
		{"iam_tenant", map[string]any{
			"id": fixture.tenant, "code": fixture.tenant, "name": "logout recovery tenant",
			"timezone": "UTC", "locale": "zh-CN",
		}},
		{"platform_application", map[string]any{
			"id": fixture.application, "tenant_id": fixture.tenant, "code": fixture.application,
			"name": "logout recovery app", "application_type": "backend",
		}},
		{"platform_application_environment", map[string]any{
			"id": fixture.environment, "tenant_id": fixture.tenant,
			"application_id": fixture.application, "environment": "test",
		}},
		{"platform_oauth_client", map[string]any{
			"id": fixture.client, "tenant_id": fixture.tenant, "application_id": fixture.application,
			"environment_id": fixture.environment, "client_id": fixture.client,
			"client_name": "logout recovery RP", "client_type": "CONFIDENTIAL",
			"token_auth_method": "client_secret_basic", "access_token_ttl_seconds": 300,
			"refresh_token_ttl_seconds": 600,
		}},
	}
	for _, row := range rows {
		row.data["status"], row.data["created_at"], row.data["updated_at"] = "ACTIVE", now, now
		if err := db.Table(row.table).Create(row.data).Error; err != nil {
			t.Fatalf("seed isolated %s: %v", row.table, err)
		}
	}
	if err := db.Exec(`INSERT INTO platform_oauth_backchannel_logout_uri
		(oauth_client_id, logout_uri, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		fixture.client, "https://rp.example/logout", now, now).Error; err != nil {
		t.Fatalf("seed registered RP endpoint: %v", err)
	}
	if err := db.Exec(`INSERT INTO platform_oauth_backchannel_logout_outbox
		(id, oauth_client_id, jti, status, attempt_count, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, fixture.message, fixture.client, fixture.message,
		StatusPending, attempts, now, now).Error; err != nil {
		t.Fatalf("seed isolated logout message: %v", err)
	}
	return fixture
}

// cleanBackchannelMySQLFixture 仅删除本轮生成的标识，不删除表或其他测试夹具。
// 参数：t 为测试上下文，db 为隔离连接，fixture 为本轮创建的标识集合。
// 返回值：无；清理失败独立记录为测试错误。
func cleanBackchannelMySQLFixture(t *testing.T, db *gorm.DB, fixture backchannelMySQLFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, row := range []struct{ table, key, id string }{
		{"platform_oauth_backchannel_logout_outbox", "id", fixture.message},
		{"platform_oauth_backchannel_logout_uri", "oauth_client_id", fixture.client},
		{"platform_oauth_client", "id", fixture.client},
		{"platform_application_environment", "id", fixture.environment},
		{"platform_application", "id", fixture.application},
		{"iam_tenant", "id", fixture.tenant},
	} {
		if err := db.WithContext(ctx).Table(row.table).Where(row.key+" = ?", row.id).
			Delete(map[string]any{}).Error; err != nil {
			t.Errorf("clean isolated %s: %v", row.table, err)
		}
	}
}

type backchannelStoredState struct {
	Status       string
	AttemptCount int
	LockedUntil  *time.Time
	DeliveredAt  *time.Time
	LastError    *string
}

// readBackchannelState 读取真实 outbox 状态，用于检查恢复是否持久化。
// 参数：t 为测试上下文，db 为隔离连接，id 为当前夹具消息标识。
// 返回值：数据库中的状态、预算和租约；读取失败时终止测试。
func readBackchannelState(t *testing.T, db *gorm.DB, id string) backchannelStoredState {
	t.Helper()
	var state backchannelStoredState
	if err := db.Table("platform_oauth_backchannel_logout_outbox").Where("id = ?", id).
		Take(&state).Error; err != nil {
		t.Fatalf("read isolated logout message: %v", err)
	}
	return state
}

// TestBackchannelFinalAttemptRecoveryMySQL 验证最后一次领取中断及写回失败可有界收敛。
// 参数：t 为测试上下文，仅消费显式配置的 BACKCHANNEL_TEST_DSN。
// 返回值：无；最终预算耗尽仍永久 PROCESSING 或额外投递时失败。
func TestBackchannelFinalAttemptRecoveryMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := backchannelMySQLDatabase(t, ctx)
	now := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	for _, interruption := range []string{"process stopped", "complete write failed", "fail write failed"} {
		t.Run(interruption, func(t *testing.T) {
			fixture := newBackchannelMySQLFixture(t, db, now, 7)
			repo := &GORMRepository{DB: db, Lease: time.Second}
			messages, err := repo.Claim(ctx, now, 1)
			if err != nil || len(messages) != 1 || messages[0].Attempt != 8 {
				t.Fatalf("last claim = %+v, error = %v", messages, err)
			}
			writeCtx, stopWrite := context.WithCancel(ctx)
			stopWrite()
			switch interruption {
			case "complete write failed":
				err = repo.Complete(writeCtx, fixture.message, now)
			case "fail write failed":
				err = repo.Fail(writeCtx, fixture.message, "RP unavailable", now.Add(time.Minute))
			}
			if interruption != "process stopped" && !errors.Is(err, context.Canceled) {
				t.Fatalf("writeback should fail on canceled context, got %v", err)
			}
			for _, recoveryTime := range []time.Time{now.Add(2 * time.Second), now.Add(time.Hour)} {
				messages, err = repo.Claim(ctx, recoveryTime, 1)
				if err != nil || len(messages) != 0 {
					t.Fatalf("exhausted message claimed again = %+v, error = %v", messages, err)
				}
				state := readBackchannelState(t, db, fixture.message)
				if state.Status != StatusFailed || state.AttemptCount != 8 ||
					state.LockedUntil != nil || state.DeliveredAt != nil ||
					state.LastError == nil || !strings.Contains(*state.LastError, "outcome_unknown") {
					t.Fatalf("expired final attempt did not converge to visible failure: %+v", state)
				}
			}
		})
	}
}

// TestBackchannelLeaseAndBudgetMySQL 验证活跃租约不抢占，过期重试仍受原预算约束。
// 参数：t 为测试上下文，仅消费专用隔离数据库变量。
// 返回值：无；提前抢占、预算重置或超过两次实际领取时失败。
func TestBackchannelLeaseAndBudgetMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := backchannelMySQLDatabase(t, ctx)
	now := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	fixture := newBackchannelMySQLFixture(t, db, now, 0)
	repo := &GORMRepository{DB: db, Lease: time.Second, MaxAttempts: 2}
	for _, step := range []struct {
		offset time.Duration
		count  int
	}{
		{0, 1}, {500 * time.Millisecond, 0}, {2 * time.Second, 1},
		{2500 * time.Millisecond, 0}, {4 * time.Second, 0}, {time.Hour, 0},
	} {
		messages, err := repo.Claim(ctx, now.Add(step.offset), 1)
		if err != nil || len(messages) != step.count {
			t.Fatalf("Claim at %s = %+v, error = %v", step.offset, messages, err)
		}
		state := readBackchannelState(t, db, fixture.message)
		if step.offset < 2*time.Second && (state.Status != StatusProcessing || state.AttemptCount != 1) {
			t.Fatalf("active first lease changed: %+v", state)
		}
		if step.offset >= 2*time.Second && state.AttemptCount != 2 {
			t.Fatalf("retry budget was reset or exceeded: %+v", state)
		}
		if step.offset < 4*time.Second &&
			(state.Status != StatusProcessing || state.LockedUntil == nil) {
			t.Fatalf("unexpired lease was prematurely reclaimed: %+v", state)
		}
		if step.offset >= 4*time.Second &&
			(state.Status != StatusFailed || state.LockedUntil != nil) {
			t.Fatalf("exhausted second lease did not reach terminal state: %+v", state)
		}
	}
}

// TestBackchannelClaimEligibilityMySQL 验证恢复逻辑仍保留登记端点与活跃客户端门禁。
// 参数：t 为测试上下文，仅操作当前夹具的端点与客户端状态。
// 返回值：无；无端点或停用客户端被领取投递时失败。
func TestBackchannelClaimEligibilityMySQL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := backchannelMySQLDatabase(t, ctx)
	now := time.Date(2026, 11, 8, 3, 0, 0, 0, time.UTC)
	for _, condition := range []string{"missing endpoint", "inactive client"} {
		t.Run(condition, func(t *testing.T) {
			fixture := newBackchannelMySQLFixture(t, db, now, 0)
			statement := "DELETE FROM platform_oauth_backchannel_logout_uri WHERE oauth_client_id=?"
			if condition == "inactive client" {
				statement = "UPDATE platform_oauth_client SET status='DISABLED' WHERE id=?"
			}
			if err := db.Exec(statement, fixture.client).Error; err != nil {
				t.Fatal(err)
			}
			repo := &GORMRepository{DB: db}
			messages, err := repo.Claim(ctx, now, 1)
			if err != nil || len(messages) != 0 {
				t.Fatalf("ineligible message was claimed: %+v, error = %v", messages, err)
			}
			state := readBackchannelState(t, db, fixture.message)
			wantStatus := StatusPending
			if condition == "missing endpoint" {
				wantStatus = StatusNoEndpoint
			}
			if state.Status != wantStatus || state.AttemptCount != 0 || state.LockedUntil != nil {
				t.Fatalf("ineligible message consumed retry budget: %+v", state)
			}
		})
	}
}
