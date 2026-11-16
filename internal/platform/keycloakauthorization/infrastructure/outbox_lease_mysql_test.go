package infrastructure

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestOutboxLeaseRejectsLateOutcomes 验证接管后旧领取不能完成、重试或终止新领取。
// 参数：t 为当前测试上下文，仅使用显式提供的本地隔离库。
// 返回值：无；并发代次或环境就绪状态错误时测试失败。
func TestOutboxLeaseRejectsLateOutcomes(t *testing.T) {
	db := localOutboxLeaseDatabase(t)
	queue, err := NewOutboxQueue(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, sameOwner := range []bool{false, true} {
		for _, outcome := range []string{"complete", "retry", "fail"} {
			t.Run(outcome+map[bool]string{false: "_new_owner", true: "_same_owner"}[sameOwner], func(t *testing.T) {
				seedLocalOutboxLease(t, db)
				now := time.Now().UTC().Truncate(time.Millisecond)
				old, ok, err := queue.Claim(context.Background(), "worker-old", now)
				if err != nil || !ok {
					t.Fatalf("first claim: %v %v", ok, err)
				}
				if err := queue.RecoverStale(context.Background(), now.Add(time.Second), now); err != nil {
					t.Fatal(err)
				}
				owner := "worker-new"
				if sameOwner {
					owner = "worker-old"
				}
				current, ok, err := queue.Claim(context.Background(), owner, now)
				if err != nil || !ok || current.Attempts != old.Attempts+1 {
					t.Fatalf("reclaim: %v %v", ok, err)
				}
				if err := finishLocalOutboxLease(queue, old, outcome, now); err == nil {
					t.Fatalf("late %s accepted after lease takeover", outcome)
				}
				var status string
				db.Table("keycloak_authorization_outbox").Select("status").Where("id = ?", current.ID).Scan(&status)
				var ready bool
				db.Table("keycloak_switch_readiness").Select("user_projection_completed").Scan(&ready)
				if status != "RUNNING" || ready {
					t.Fatalf("late result changed state: %s ready=%v", status, ready)
				}
				if err := queue.Complete(context.Background(), current); err != nil {
					t.Fatal(err)
				}
				if err := db.Table("keycloak_switch_readiness").
					Select("user_projection_completed").Scan(&ready).Error; err != nil || !ready {
					t.Fatalf("current owner did not finish readiness: ready=%v err=%v", ready, err)
				}
			})
		}
	}
}

// TestOutboxLeaseRejectsForgedIdentity 验证空凭据或跨环境身份不能借事件 ID 提交。
// 参数：t 为只连接本任务隔离库的测试上下文。
// 返回值：无；无属主、零代次或身份伪造被接受时失败。
func TestOutboxLeaseRejectsForgedIdentity(t *testing.T) {
	db := localOutboxLeaseDatabase(t)
	seedLocalOutboxLease(t, db)
	queue, _ := NewOutboxQueue(db)
	event, ok, err := queue.Claim(context.Background(), "owner", time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	for _, change := range []func(*projectionworker.Event){
		func(e *projectionworker.Event) { e.WorkerID = "" },
		func(e *projectionworker.Event) { e.Attempts = 0 },
		func(e *projectionworker.Event) { e.TenantID = "other" },
		func(e *projectionworker.Event) { e.EnvironmentID = "other" },
		func(e *projectionworker.Event) { e.ApplicationID = "other" },
		func(e *projectionworker.Event) { e.IdentityID = "other" },
	} {
		for _, outcome := range []string{"complete", "retry", "fail"} {
			forged := event
			change(&forged)
			if err := finishLocalOutboxLease(queue, forged, outcome, time.Now()); err == nil {
				t.Fatalf("forged identity allowed %s", outcome)
			}
		}
	}
	if err := queue.Complete(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

// finishLocalOutboxLease 调用指定的任务终态接口，模拟旧处理者迟到。
// 参数：queue 为真实适配器，event 为领取凭据，outcome 为动作，now 为重试时间。
// 返回值：动作的实际错误；不吞掉租约冲突。
func finishLocalOutboxLease(queue *OutboxQueue, event projectionworker.Event, outcome string, now time.Time) error {
	switch outcome {
	case "complete":
		return queue.Complete(context.Background(), event)
	case "retry":
		return queue.Retry(context.Background(), event, "TEST_RETRY", "retry", now)
	default:
		return queue.Fail(context.Background(), event, "TEST_FAIL", "failed")
	}
}

// localOutboxLeaseDatabase 打开严格限定地址和库名的独立 MySQL 测试库。
// 参数：t 为测试上下文；凭据仅从环境变量指定的私有文件读取。
// 返回值：测试数据库；没有显式文件时跳过，任何不安全目标均失败。
func localOutboxLeaseDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	path := os.Getenv("PLATFORM_LOCAL_FIX_MYSQL_DSN_FILE")
	if path == "" {
		t.Skip("explicit isolated MySQL DSN file required")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read isolated DSN file")
	}
	cfg, err := mysqldriver.ParseDSN(strings.TrimSpace(string(value)))
	if err != nil || cfg.DBName != "local_fix_platform" || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("isolated MySQL address/schema guard rejected target")
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated MySQL")
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS keycloak_authorization_outbox (
		 id varchar(26) PRIMARY KEY, tenant_id varchar(26), identity_id varchar(26),
		 application_id varchar(26), environment_id varchar(26), event_type varchar(64),
		 status varchar(32), attempts int unsigned NOT NULL DEFAULT 0,
		 available_at datetime(3), created_at datetime(3), locked_by varchar(128),
		 locked_at datetime(3), completed_at datetime(3), last_error_code varchar(128),
		 last_error_message varchar(1000)) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS keycloak_switch_readiness (
		 tenant_id varchar(26), application_id varchar(26), environment_id varchar(26),
		 user_projection_completed bool NOT NULL DEFAULT FALSE, updated_at datetime(3),
		 PRIMARY KEY(tenant_id, application_id, environment_id)) ENGINE=InnoDB`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// seedLocalOutboxLease 写入本组独占的最小事件和环境就绪记录。
// 参数：t 为测试上下文，db 必须已经过隔离地址与库名校验。
// 返回值：无；插入失败立即终止当前用例。
func seedLocalOutboxLease(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{"keycloak_authorization_outbox", "keycloak_switch_readiness"} {
		if err := db.Table(table).Where("tenant_id = ?", "lease-test-tenant").Delete(nil).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Add(-time.Minute)
	if err := db.Table("keycloak_authorization_outbox").Create(map[string]any{
		"id": "lease-test-event", "tenant_id": "lease-test-tenant", "identity_id": "identity",
		"application_id": "app", "environment_id": "env", "event_type": "AUTHORIZATION_CHANGED",
		"status": "PENDING", "attempts": 0, "available_at": now, "created_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("keycloak_switch_readiness").Create(map[string]any{
		"tenant_id": "lease-test-tenant", "application_id": "app", "environment_id": "env",
		"user_projection_completed": false, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}
