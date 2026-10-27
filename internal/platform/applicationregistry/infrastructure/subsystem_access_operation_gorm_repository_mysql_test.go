package infrastructure

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// subsystemAccessRepositoryFor 用途：仅在显式隔离 DSN 上建立持久任务及代次表。
// 参数：t 为隔离测试上下文。
// 返回值：返回任务仓储和专用测试数据库；无 DSN 时跳过，不探测业务数据库。
func subsystemAccessRepositoryFor(t *testing.T) (*SubsystemAccessOperationGORMRepository, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	statements := []string{
		"DROP TABLE IF EXISTS subsystem_access_deployment_attempt",
		"DROP TABLE IF EXISTS subsystem_access_operation_stage",
		"DROP TABLE IF EXISTS subsystem_access_operation",
		`CREATE TABLE subsystem_access_operation (
		  id CHAR(26) NOT NULL, tenant_id CHAR(26) NOT NULL, application_id CHAR(26) NULL, environment_id CHAR(26) NULL,
		  application_code VARCHAR(64) NOT NULL, environment_code VARCHAR(16) NOT NULL,
		  action VARCHAR(16) NOT NULL, stage VARCHAR(32) NOT NULL, status VARCHAR(16) NOT NULL,
		  preview_version VARCHAR(64) NOT NULL, request_digest CHAR(64) NOT NULL, idempotency_key VARCHAR(160) NOT NULL,
		  public_origin VARCHAR(255) NOT NULL DEFAULT '', auth_provider VARCHAR(64) NOT NULL DEFAULT '', initial_admin_id CHAR(26) NULL,
		  operator_id CHAR(26) NOT NULL, lease_owner VARCHAR(64) NULL, lease_expires_at DATETIME(3) NULL,
		  attempt_count INT UNSIGNED NOT NULL DEFAULT 0, error_code VARCHAR(128) NULL, error_message VARCHAR(1000) NULL,
		  result_ref VARCHAR(255) NULL, active_slot VARCHAR(160) NULL, version BIGINT UNSIGNED NOT NULL DEFAULT 1,
		  created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
		  PRIMARY KEY (id),
		  UNIQUE KEY uq_subsystem_access_operation_idempotency (tenant_id, idempotency_key),
		  UNIQUE KEY uq_subsystem_access_operation_active (active_slot)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE subsystem_access_operation_stage (
		  id CHAR(26) NOT NULL, tenant_id CHAR(26) NOT NULL, operation_id CHAR(26) NOT NULL,
		  stage VARCHAR(32) NOT NULL, status VARCHAR(16) NOT NULL, attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
		  error_code VARCHAR(128) NULL, error_message VARCHAR(1000) NULL, evidence_ref VARCHAR(255) NULL,
		  started_at DATETIME(3) NULL, finished_at DATETIME(3) NULL, version BIGINT UNSIGNED NOT NULL DEFAULT 1,
		  created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
		  PRIMARY KEY (id), UNIQUE KEY uq_subsystem_access_operation_stage (tenant_id, operation_id, stage)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	migrationSQL, err := migrations.Files.ReadFile("000132_subsystem_access_deployment_attempt.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(migrationSQL)).Error; err != nil {
		t.Fatal(err)
	}
	repository, err := NewSubsystemAccessOperationGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository, db
}

func accessCreate(tenant, id, digest string, now time.Time) application.SubsystemAccessOperationCreate {
	return application.SubsystemAccessOperationCreate{
		ID: id, TenantID: tenant, ApplicationCode: "settlement", EnvironmentCode: "dev",
		Action: application.SubsystemAccessActionOnboard, Stage: application.SubsystemAccessStageCheck,
		PreviewVersion: "pv-1", RequestDigest: digest, IdempotencyKey: "idem-" + id, OperatorID: "operator-1",
		PublicOrigin: "https://settlement.example.com", AuthProvider: "platform", InitialAdminID: "01USR000000000000000000001", Now: now,
	}
}

func TestSubsystemAccessOperationIdempotencyAndSlotOnMySQL(t *testing.T) {
	repository, _ := subsystemAccessRepositoryFor(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, created, err := repository.CreateOperation(ctx, accessCreate("tenant-1", "01OP0000000000000000000001", "digest-a", now))
	if err != nil || !created || first.Status != application.SubsystemAccessStatusPending {
		t.Fatalf("create: %+v created=%v err=%v", first, created, err)
	}
	// 执行参数必须原样持久化（Worker 据此执行；均为非敏感字段）。
	if first.PublicOrigin != "https://settlement.example.com" || first.AuthProvider != "platform" || first.InitialAdminID != "01USR000000000000000000001" {
		t.Fatalf("execution parameters lost: %+v", first)
	}
	// 同键同摘要：返回原操作，不新建。
	replay := accessCreate("tenant-1", "01OP0000000000000000000099", "digest-a", now)
	replay.IdempotencyKey = first.IdempotencyKey
	same, created, err := repository.CreateOperation(ctx, replay)
	if err != nil || created || same.ID != first.ID {
		t.Fatalf("idempotent replay: %+v created=%v err=%v", same, created, err)
	}
	// 同键不同摘要：冲突。
	drifted := replay
	drifted.RequestDigest = "digest-b"
	if _, _, err := repository.CreateOperation(ctx, drifted); !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
		t.Fatalf("same key different digest must conflict, got %v", err)
	}
	// 同目标、不同键：执行中槽位冲突。
	if _, _, err := repository.CreateOperation(ctx, accessCreate("tenant-1", "01OP0000000000000000000002", "digest-c", now)); !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
		t.Fatalf("running operation must block the same target, got %v", err)
	}
	// 其它租户同目标：不受影响。
	if _, created, err := repository.CreateOperation(ctx, accessCreate("tenant-2", "01OP0000000000000000000003", "digest-d", now)); err != nil || !created {
		t.Fatalf("other tenant must not be blocked: created=%v err=%v", created, err)
	}
}

func TestSubsystemAccessOperationLeaseAndStaleExecutorOnMySQL(t *testing.T) {
	repository, _ := subsystemAccessRepositoryFor(t)
	ctx := context.Background()
	now := time.Now().UTC()
	operation, _, err := repository.CreateOperation(ctx, accessCreate("tenant-1", "01OP0000000000000000000011", "digest-a", now))
	if err != nil {
		t.Fatal(err)
	}
	// 领取：worker A 持有 1 秒租约。
	claimed, ok, err := repository.ClaimOperation(ctx, "tenant-1", operation.ID, "worker-a", time.Second, now)
	if err != nil || !ok || claimed.Status != application.SubsystemAccessStatusRunning || claimed.AttemptCount != 1 {
		t.Fatalf("claim: %+v ok=%v err=%v", claimed, ok, err)
	}
	// 租约未过期：worker B 抢不到。
	if _, ok, err := repository.ClaimOperation(ctx, "tenant-1", operation.ID, "worker-b", time.Second, now.Add(500*time.Millisecond)); err != nil || ok {
		t.Fatalf("live lease must not be stolen: ok=%v err=%v", ok, err)
	}
	// 租约过期：worker B 可领取，尝试次数累加。
	expired := now.Add(2 * time.Second)
	reclaimed, ok, err := repository.ClaimOperation(ctx, "tenant-1", operation.ID, "worker-b", time.Minute, expired)
	if err != nil || !ok || reclaimed.LeaseOwner != "worker-b" || reclaimed.AttemptCount != 2 {
		t.Fatalf("expired lease must be reclaimable: %+v ok=%v err=%v", reclaimed, ok, err)
	}
	// 过期执行者（worker A）收口：必须被拒，且不覆盖新结果。
	_, err = repository.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{
		TenantID: "tenant-1", ID: operation.ID, LeaseOwner: "worker-a", Status: application.SubsystemAccessStatusSucceeded,
		Stage: application.SubsystemAccessStageResult, ResultRef: "stale", Now: expired.Add(time.Second)})
	if !errors.Is(err, application.ErrSubsystemAccessOperationLeaseLost) {
		t.Fatalf("stale executor must be rejected, got %v", err)
	}
	current, err := repository.GetOperation(ctx, "tenant-1", operation.ID)
	if err != nil || current.Status != application.SubsystemAccessStatusRunning || current.ResultRef != "" {
		t.Fatalf("stale completion must not change the operation: %+v err=%v", current, err)
	}
	// 新执行者收口成功；终态释放槽位，允许同目标新操作。
	if ok, err := repository.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{
		TenantID: "tenant-1", ID: operation.ID, LeaseOwner: "worker-b", Status: application.SubsystemAccessStatusSucceeded,
		Stage: application.SubsystemAccessStageResult, ResultRef: "evidence:op-11", Now: expired.Add(2 * time.Second)}); err != nil || !ok {
		t.Fatalf("current executor must complete: ok=%v err=%v", ok, err)
	}
	if _, created, err := repository.CreateOperation(ctx, accessCreate("tenant-1", "01OP0000000000000000000012", "digest-e", expired.Add(3*time.Second))); err != nil || !created {
		t.Fatalf("terminal state must release the slot: created=%v err=%v", created, err)
	}
}

func TestSubsystemAccessOperationWaitingKeepsSlotAndStagesOnMySQL(t *testing.T) {
	repository, _ := subsystemAccessRepositoryFor(t)
	ctx := context.Background()
	now := time.Now().UTC()
	operation, _, err := repository.CreateOperation(ctx, accessCreate("tenant-1", "01OP0000000000000000000021", "digest-a", now))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repository.ClaimOperation(ctx, "tenant-1", operation.ID, "worker-a", time.Minute, now); err != nil || !ok {
		t.Fatalf("claim failed: %v", err)
	}
	// WAITING：保留槽位供 resume 继续。
	if ok, err := repository.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{
		TenantID: "tenant-1", ID: operation.ID, LeaseOwner: "worker-a", Status: application.SubsystemAccessStatusWaiting,
		Stage: application.SubsystemAccessStageAuth, ErrorCode: "LICENSE_APPROVAL_PENDING", Now: now}); err != nil || !ok {
		t.Fatalf("waiting transition failed: ok=%v err=%v", ok, err)
	}
	if _, _, err := repository.CreateOperation(ctx, accessCreate("tenant-1", "01OP0000000000000000000022", "digest-b", now)); !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
		t.Fatalf("WAITING must keep the slot, got %v", err)
	}
	// 阶段 upsert：同阶段重复写入累加尝试次数，且按固定顺序返回。
	for attempt := 0; attempt < 2; attempt++ {
		if err := repository.UpsertOperationStage(ctx, "tenant-1", operation.ID, application.SubsystemAccessStageCheck,
			application.SubsystemAccessStatusSucceeded, "", "", "evidence:check-1", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.UpsertOperationStage(ctx, "tenant-1", operation.ID, application.SubsystemAccessStageAuth,
		application.SubsystemAccessStatusWaiting, "LICENSE_APPROVAL_PENDING", "等待运行批准", "", now); err != nil {
		t.Fatal(err)
	}
	stages, err := repository.ListOperationStages(ctx, "tenant-1", operation.ID)
	if err != nil || len(stages) != 2 {
		t.Fatalf("stages = %+v err=%v", stages, err)
	}
	if stages[0].Stage != application.SubsystemAccessStageCheck || stages[0].AttemptCount != 2 || stages[0].EvidenceRef != "evidence:check-1" {
		t.Fatalf("check stage = %+v", stages[0])
	}
	if stages[1].Stage != application.SubsystemAccessStageAuth || stages[1].Status != application.SubsystemAccessStatusWaiting {
		t.Fatalf("auth stage = %+v", stages[1])
	}
	// 跨租户读取被拒（视为不存在）。
	if _, err := repository.GetOperation(ctx, "tenant-2", operation.ID); !errors.Is(err, application.ErrSubsystemAccessOperationNotFound) {
		t.Fatalf("cross-tenant read must be rejected, got %v", err)
	}
}

// RequeueOperation：等待中的操作被放回可领取状态，且重新领取时尝试次数继续累加。
func TestSubsystemAccessOperationRequeueOnMySQL(t *testing.T) {
	repository, _ := subsystemAccessRepositoryFor(t)
	ctx := context.Background()
	now := time.Now().UTC()
	operation, _, err := repository.CreateOperation(ctx, accessCreate("tenant-1", "01OP0000000000000000000031", "digest-a", now))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repository.ClaimOperation(ctx, "tenant-1", operation.ID, "worker-a", time.Minute, now); err != nil || !ok {
		t.Fatalf("claim failed: %v", err)
	}
	if ok, err := repository.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{
		TenantID: "tenant-1", ID: operation.ID, LeaseOwner: "worker-a", Status: application.SubsystemAccessStatusWaiting,
		Stage: application.SubsystemAccessStageAuth, ErrorCode: "RUNTIME_APPROVAL_MISSING", ErrorMessage: "等待运行批准", Now: now}); err != nil || !ok {
		t.Fatalf("waiting transition failed: ok=%v err=%v", ok, err)
	}
	// WAITING → 放回可领取状态（清空租约与错误码）。
	requeued, err := repository.RequeueOperation(ctx, "tenant-1", operation.ID, now)
	if err != nil || !requeued {
		t.Fatalf("waiting operation must be requeued: ok=%v err=%v", requeued, err)
	}
	// 重复 Requeue 是空操作（条件更新不命中），不报错。
	if ok, err := repository.RequeueOperation(ctx, "tenant-1", operation.ID, now); err != nil || ok {
		t.Fatalf("repeated requeue must be a no-op: ok=%v err=%v", ok, err)
	}
	current, err := repository.GetOperation(ctx, "tenant-1", operation.ID)
	if err != nil || current.Status != application.SubsystemAccessStatusPending || current.LeaseOwner != "" || current.ErrorCode != "" {
		t.Fatalf("requeued operation = %+v err=%v", current, err)
	}
	// 重新领取：尝试次数继续累加（不重置）。
	reclaimed, ok, err := repository.ClaimOperation(ctx, "tenant-1", operation.ID, "worker-b", time.Minute, now.Add(time.Second))
	if err != nil || !ok || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaim after requeue: %+v ok=%v err=%v", reclaimed, ok, err)
	}
	// 终态后 Requeue 必须是空操作（条件更新不命中）。
	if ok, err := repository.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{
		TenantID: "tenant-1", ID: operation.ID, LeaseOwner: "worker-b", Status: application.SubsystemAccessStatusSucceeded,
		Stage: application.SubsystemAccessStageResult, Now: now.Add(2 * time.Second)}); err != nil || !ok {
		t.Fatalf("terminal completion failed: ok=%v err=%v", ok, err)
	}
	if ok, err := repository.RequeueOperation(ctx, "tenant-1", operation.ID, now.Add(3*time.Second)); err != nil || ok {
		t.Fatalf("terminal operation must not be requeued: ok=%v err=%v", ok, err)
	}
}

// TestSubsystemAccessOperationManifestRetryOnMySQL 用途：确保清单不一致从部署恢复且不重复登记认证。
// 参数：t 为显式隔离 MySQL 测试上下文。
// 返回值：无。
func TestSubsystemAccessOperationManifestRetryOnMySQL(t *testing.T) {
	repository, _ := subsystemAccessRepositoryFor(t)
	ctx := context.Background()
	now := time.Now().UTC()
	operation, _, err := repository.CreateOperation(ctx,
		accessCreate("tenant-1", "01OP0000000000000000000039", "digest-retry", now))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repository.ClaimOperation(ctx, "tenant-1", operation.ID, "worker-a", time.Minute, now); err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	if ok, err := repository.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{
		TenantID: "tenant-1", ID: operation.ID, LeaseOwner: "worker-a", Status: application.SubsystemAccessStatusFailed,
		Stage: application.SubsystemAccessStageReadiness, ErrorCode: "MANIFEST_MISMATCH", Now: now,
	}); err != nil || !ok {
		t.Fatalf("fail readiness: %v", err)
	}
	if ok, err := repository.RequeueOperation(ctx, "tenant-1", operation.ID, now); err != nil || !ok {
		t.Fatalf("resume: %v", err)
	}
	current, err := repository.GetOperation(ctx, "tenant-1", operation.ID)
	if err != nil || current.Stage != application.SubsystemAccessStageDeploy || current.Status != application.SubsystemAccessStatusPending {
		t.Fatalf("retry did not return to deployment: %+v %v", current, err)
	}
}
