package infrastructure

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/migrations"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type accessLifecycleIDs struct{ next int }

// New 用途：为隔离接入创建确定且互不重复的资源身份。
// 参数：at 为创建时间。
// 返回值：返回符合现有 ID 长度约束的测试身份。
func (ids *accessLifecycleIDs) New(at time.Time) (string, error) {
	ids.next++
	return fmt.Sprintf("01K10C%020d", ids.next), nil
}

// accessFullMigrationDatabase 用途：在显式测试 MySQL 中新建独立 schema，执行真实全量迁移。
// 参数：t 为测试上下文。
// 返回值：返回迁移后的数据库，测试结束仅删除本次创建的 schema。
func accessFullMigrationDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("no explicit isolated MySQL DSN")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	admin, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated MySQL")
	}
	schema := fmt.Sprintf("access_lifecycle_%x", time.Now().UnixNano())
	if err := admin.Exec("CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec("DROP DATABASE `" + schema + "`").Error; err != nil {
			t.Error("owned schema cleanup failed", err)
		}
		if pool, e := admin.DB(); e == nil {
			pool.Close()
		}
	})
	cfg.DBName = schema
	cfg.Collation = "utf8mb4_unicode_ci"
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["charset"] = "utf8mb4"
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open owned schema")
	}
	t.Cleanup(func() {
		if pool, e := db.DB(); e == nil {
			pool.Close()
		}
	})
	if _, err := migration.Run(context.Background(), db, migrations.Files); err != nil {
		t.Fatal("apply real migrations", err)
	}
	return db
}

// TestAccessFirstOnboardLifecycleOnMySQL 用途：验证真实初次登记事务到 READY 的完整数据库调用链。
// 参数：t 为隔离测试上下文；外部 Agent 使用明确替身，不代表真实容器部署验收。
// 返回值：无。
func TestAccessFirstOnboardLifecycleOnMySQL(t *testing.T) {
	db := accessFullMigrationDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	tenant, user := "01K10A00000000000000000001", "01K10B00000000000000000001"
	if err := db.Exec("INSERT INTO iam_tenant(id,code,name,timezone,locale,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)", tenant, "lifecycle-test", "隔离测试", "Asia/Shanghai", "zh-CN", "ACTIVE", now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO iam_user(id,tenant_id,display_name,employment_status,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", user, tenant, "隔离操作者", "ACTIVE", "ACTIVE", now, now).Error; err != nil {
		t.Fatal(err)
	}
	operations, _ := NewSubsystemAccessOperationGORMRepository(db)
	create := accessCreate(tenant, "01OP0000000000000000000222", "digest-first", now)
	create.OperatorID, create.InitialAdminID = user, user
	create.Stage = application.SubsystemAccessStageAuth
	op, _, err := operations.CreateOperation(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	op, claimed, err := operations.ClaimOperation(ctx, tenant, op.ID, "isolated-worker", time.Minute, now)
	if err != nil || !claimed {
		t.Fatal("claim", err)
	}
	repo, _ := NewSubsystemOnboardingGORMRepository(db)
	service, err := application.NewSubsystemOnboardingService(repo, &accessLifecycleIDs{}, application.SystemClock{}, application.RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := NewSubsystemAccessDeploymentStateGORMReader(db)
	agent := &deploymentProvisionerStub{}
	adapter, err := NewSubsystemAccessDeploymentAdapter(service, agent, accessCatalogWithTarget(), relatedClientsStub{}, reader, issuerStub{issuer: "https://sso.example.com"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := NewSubsystemAccessRegistryGORMReader(db)
	adapter.SetIdentityReaders(registry, nil)
	run := &application.SubsystemAccessRunState{}
	// 模拟绑定写入失败：认证资源和 PROVISIONING 必须与绑定一起回滚。
	if err := db.Exec("CREATE TRIGGER reject_access_binding BEFORE INSERT ON subsystem_access_deployment_attempt FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'isolated binding failure'").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ConfigureSubsystemAccessAuthentication(ctx, op, run); err == nil {
		t.Fatal("binding failure did not abort onboarding")
	}
	var applications, clients int64
	db.Table("platform_application").Where("tenant_id = ?", tenant).Count(&applications)
	db.Table("oauth_client").Where("tenant_id = ?", tenant).Count(&clients)
	if applications != 0 || clients != 0 {
		t.Fatal("atomic onboarding rollback left resources")
	}
	if err := db.Exec("DROP TRIGGER reject_access_binding").Error; err != nil {
		t.Fatal(err)
	}
	outcome, err := adapter.ConfigureSubsystemAccessAuthentication(ctx, op, run)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("first AUTH: %+v %v", outcome, err)
	}
	bound, err := reader.ReadAccessDeploymentBinding(ctx, op)
	if err != nil || !bound {
		t.Fatal("AUTH committed without original generation", err)
	}
	state, err := repo.GetSubsystemDeploymentState(ctx, tenant, "settlement", "dev")
	if err != nil || state.Status != "PROVISIONING" || state.Generation != 1 {
		t.Fatalf("initial state: %+v %v", state, err)
	}
	outcome, err = adapter.ProvisionSubsystemAccess(ctx, op, run)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("DEPLOY: %+v %v", outcome, err)
	}
	outcome, err = adapter.SubsystemAccessDeploymentReady(ctx, op)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("READINESS: %+v %v", outcome, err)
	}
	state, err = repo.GetSubsystemDeploymentState(ctx, tenant, "settlement", "dev")
	if err != nil || state.Status != "READY" || state.Generation != 1 || state.AppliedManifestChecksum != "manifest-aaa" {
		t.Fatalf("terminal: %+v %v", state, err)
	}
	// 数据库成功回执先于 Worker 阶段推进持久化：重启从 DEPLOY 恢复不能再次发 Agent。
	outcome, err = adapter.ProvisionSubsystemAccess(ctx, op, nil)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded || agent.provisionCalls != 1 {
		t.Fatal("post-success crash replayed deployment", err)
	}
	// ADOPT 已有应用/环境但无部署状态时必须走正常 Claim 建档。
	if err := db.Where("tenant_id = ? AND operation_id = ?", tenant, op.ID).Delete(&subsystemAccessDeploymentAttempt{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("tenant_id = ?", tenant).Delete(&subsystemDeploymentStateModel{}).Error; err != nil {
		t.Fatal(err)
	}
	current, err := operations.GetOperation(ctx, tenant, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Action = application.SubsystemAccessActionAdopt
	if err := db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).Update("action", "ADOPT").Error; err != nil {
		t.Fatal(err)
	}
	if phase, err := reader.PrepareAccessDeployment(ctx, current); err != nil || phase != "PREPARED" {
		t.Fatalf("adopt without deployment state: %s %v", phase, err)
	}
}
