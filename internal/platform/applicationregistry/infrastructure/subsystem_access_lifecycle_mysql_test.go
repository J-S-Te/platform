package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/gorm"
)

// lifecycleDatabase 用途：创建显式隔离测试库中的任务、生命周期及代次绑定表。
// 参数：t 为测试上下文。
// 返回值：返回任务仓储、状态读取器、数据库及已领取任务。
func lifecycleDatabase(t *testing.T) (*SubsystemAccessOperationGORMRepository, *SubsystemAccessDeploymentStateGORMReader, *gorm.DB, application.SubsystemAccessOperation) {
	t.Helper()
	repo, db := subsystemAccessRepositoryFor(t)
	for _, model := range []any{&subsystemAccessDeploymentAttempt{}, &subsystemDeploymentStateModel{}} {
		if err := db.Migrator().DropTable(model); err != nil {
			t.Fatal(err)
		}
	}
	sql, err := migrations.Files.ReadFile("000132_subsystem_access_deployment_attempt.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(string(sql)).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Set("gorm:table_options", "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci").AutoMigrate(&subsystemDeploymentStateModel{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	create := accessCreate("tenant-1", "01OP0000000000000000000111", "digest", now)
	create.ApplicationID, create.EnvironmentID = "app-1", "env-1"
	create.Action = application.SubsystemAccessActionUpdate
	create.Stage = application.SubsystemAccessStageDeploy
	op, _, err := repo.CreateOperation(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	op, claimed, err := repo.ClaimOperation(context.Background(), op.TenantID, op.ID, "worker-1", time.Minute, now)
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	state := subsystemDeploymentStateModel{TenantID: op.TenantID, ApplicationID: op.ApplicationID, EnvironmentID: op.EnvironmentID,
		ApplicationCode: op.ApplicationCode, Environment: op.EnvironmentCode, Status: "READY", Operation: "ONBOARD", Generation: 4, AttemptCount: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	reader, err := NewSubsystemAccessDeploymentStateGORMReader(db)
	if err != nil {
		t.Fatal(err)
	}
	return repo, reader, db, op
}

// TestAccessLifecycleRecoveryOnMySQL 用途：验证重启沿用代次及成功收口原子性。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessLifecycleRecoveryOnMySQL(t *testing.T) {
	repo, reader, db, op := lifecycleDatabase(t)
	ctx := context.Background()
	phase, err := reader.PrepareAccessDeployment(ctx, op)
	if err != nil || phase != "PREPARED" {
		t.Fatalf("prepare: %s %v", phase, err)
	}
	var state subsystemDeploymentStateModel
	db.Take(&state)
	if state.Generation != 5 || state.Status != "UPDATING" {
		t.Fatalf("claimed state: %+v", state)
	}
	// 新建读取器代表进程重启；再次准备不能领取第六代。
	restarted, _ := NewSubsystemAccessDeploymentStateGORMReader(db)
	if _, err = restarted.PrepareAccessDeployment(ctx, op); err != nil {
		t.Fatal(err)
	}
	db.Take(&state)
	if state.Generation != 5 {
		t.Fatal("restart claimed another generation")
	}
	if err = reader.DispatchAccessDeployment(ctx, op, "manifest-1", "candidate-1"); err != nil {
		t.Fatal(err)
	}
	if err = reader.DispatchAccessDeployment(ctx, op, "manifest-1", "candidate-1"); !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
		t.Fatal("duplicate dispatch was accepted", err)
	}
	if err = reader.CompleteAccessDeployment(ctx, op, "different-manifest"); err == nil {
		t.Fatal("different candidate was accepted")
	}
	if err = reader.CompleteAccessDeployment(ctx, op, "manifest-1"); err != nil {
		t.Fatal(err)
	}
	db.Take(&state)
	if state.Generation != 5 || state.Status != "READY" || accessDerefString(state.AppliedManifestChecksum) != "manifest-1" {
		t.Fatalf("completion: %+v", state)
	}
	receipt, candidate, receiptErr := restarted.ReadAccessDeploymentReceipt(ctx, op)
	if receiptErr != nil || receipt.Generation != 5 || receipt.ManifestChecksum != "manifest-1" || candidate != "candidate-1" {
		t.Fatalf("receipt binding: %+v %s %v", receipt, candidate, receiptErr)
	}
	if phase, err = restarted.PrepareAccessDeployment(ctx, op); err != nil || phase != "SUCCEEDED" {
		t.Fatalf("completion receipt lost: %s %v", phase, err)
	}
	// 任务租约重新领取后，即使 worker ID 相同，旧领取轮次仍必须失效。
	expired := time.Now().Add(-time.Second)
	db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).Update("lease_expires_at", expired)
	newer, claimed, err := repo.ClaimOperation(ctx, op.TenantID, op.ID, op.LeaseOwner, time.Minute, time.Now())
	if err != nil || !claimed {
		t.Fatalf("reclaim: %v %v", claimed, err)
	}
	if _, err = restarted.PrepareAccessDeployment(ctx, op); !errors.Is(err, application.ErrSubsystemAccessOperationLeaseLost) {
		t.Fatal("old claim retained ownership", err)
	}
	if _, err = restarted.PrepareAccessDeployment(ctx, newer); err != nil {
		t.Fatal(err)
	}
}

// TestAccessLifecycleGenerationConflictOnMySQL 用途：拒绝旧任务覆盖新的部署代次。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessLifecycleGenerationConflictOnMySQL(t *testing.T) {
	_, reader, db, op := lifecycleDatabase(t)
	ctx := context.Background()
	if _, err := reader.PrepareAccessDeployment(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := reader.DispatchAccessDeployment(ctx, op, "manifest-1", "candidate-1"); err != nil {
		t.Fatal(err)
	}
	db.Model(&subsystemDeploymentStateModel{}).Where("tenant_id = ?", op.TenantID).Update("generation", 6)
	if err := reader.CompleteAccessDeployment(ctx, op, "manifest-1"); !errors.Is(err, application.ErrSubsystemDeploymentTransition) {
		t.Fatal("old generation completed new deployment", err)
	}
	var state subsystemDeploymentStateModel
	db.Take(&state)
	if state.Status != "UPDATING" || state.AppliedManifestChecksum != nil {
		t.Fatal("rejected completion leaked state")
	}
}

// TestAccessInitialBindingAtomicityOnMySQL 用途：验证首次登记与原代次绑定同成同败。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessInitialBindingAtomicityOnMySQL(t *testing.T) {
	_, _, db, op := lifecycleDatabase(t)
	op.Action = application.SubsystemAccessActionOnboard
	db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).Update("action", "ONBOARD")
	sentinel := errors.New("simulate process failure before commit")
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := bindInitialAccessDeployment(context.Background(), tx, op, "app-1", "env-1", time.Now()); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	var count int64
	db.Model(&subsystemAccessDeploymentAttempt{}).Count(&count)
	if count != 0 {
		t.Fatal("rolled-back registration retained binding")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return bindInitialAccessDeployment(context.Background(), tx, op, "app-1", "env-1", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	var attempt subsystemAccessDeploymentAttempt
	if err := db.Take(&attempt).Error; err != nil || attempt.Generation != 1 || attempt.Phase != "PREPARED" {
		t.Fatalf("binding: %+v %v", attempt, err)
	}
}

// TestAccessRetryPreservesInitialProvisioningOnMySQL 用途：验证首次失败后的重试不受领取改状态影响。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessRetryPreservesInitialProvisioningOnMySQL(t *testing.T) {
	_, reader, db, op := lifecycleDatabase(t)
	op.Action = application.SubsystemAccessActionRetry
	db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).Update("action", "RETRY")
	db.Model(&subsystemDeploymentStateModel{}).Where("tenant_id = ?", op.TenantID).Update("status", application.SubsystemDeploymentStatusFailed)
	if _, err := reader.PrepareAccessDeployment(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	initial, err := reader.ReadAccessInitialRuntimeProvisioning(context.Background(), op)
	if err != nil || !initial {
		t.Fatalf("lost original failed state: %v %v", initial, err)
	}
	var state subsystemDeploymentStateModel
	db.Take(&state)
	if state.Status != "UPDATING" {
		t.Fatal("test did not exercise post-claim state")
	}
	if err := reader.FailAccessDeployment(context.Background(), op, "INVALID_INPUT", "已知输入失败"); err != nil {
		t.Fatal(err)
	}
	db.Take(&state)
	if state.Status != application.SubsystemDeploymentStatusFailed {
		t.Fatal("known failure left lifecycle running")
	}
	if _, err := reader.PrepareAccessDeployment(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	db.Take(&state)
	if state.Generation != 6 {
		t.Fatal("explicit retry did not claim next generation")
	}
	initial, err = reader.ReadAccessInitialRuntimeProvisioning(context.Background(), op)
	if err != nil || !initial {
		t.Fatal("retry lost persisted initial flag")
	}
}

// TestAccessRuntimeExpectationsExcludeSecretsOnMySQL 用途：验证恢复确认只保存非秘密身份。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessRuntimeExpectationsExcludeSecretsOnMySQL(t *testing.T) {
	_, reader, db, op := lifecycleDatabase(t)
	fixedUpdateTime := time.Now().UTC()
	reader.database = db.Session(&gorm.Session{NowFunc: func() time.Time { return fixedUpdateTime }})
	ctx := context.Background()
	if _, err := reader.PrepareAccessDeployment(ctx, op); err != nil {
		t.Fatal(err)
	}
	list := []application.RuntimeLicenseCredential{{Approval: application.RuntimeLicenseApproval{ServiceID: "api"}, ClientID: "runtime-client", ClientSecret: "must-never-persist", RegistrationProtocol: 2, CatalogVersion: 1}}
	if err := reader.SaveAccessRuntimeExpectations(ctx, op, list); err != nil {
		t.Fatal(err)
	}
	if err := reader.SaveAccessRuntimeExpectations(ctx, op, list); err != nil {
		t.Fatal("pre-dispatch restart cannot save unchanged expectations", err)
	}
	actual, err := reader.ReadAccessRuntimeExpectations(ctx, op)
	if err != nil || len(actual) != 1 || actual[0].ClientID != "runtime-client" || actual[0].ClientSecret != "" {
		t.Fatal("unsafe confirmation identity persistence", err)
	}
	var row subsystemAccessDeploymentAttempt
	db.Take(&row)
	if strings.Contains(string(row.RuntimeExpectations), "must-never-persist") {
		t.Fatal("plaintext credential persisted")
	}
}

// TestAccessSweeperKeepsOwnedGenerationOnMySQL 用途：避免看护线程抢先终结仍可恢复的持久向导。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessSweeperKeepsOwnedGenerationOnMySQL(t *testing.T) {
	_, reader, db, op := lifecycleDatabase(t)
	ctx := context.Background()
	if _, err := reader.PrepareAccessDeployment(ctx, op); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	db.Model(&subsystemDeploymentStateModel{}).Where("tenant_id = ?", op.TenantID).Update("started_at", old)
	repo := &SubsystemOnboardingGORMRepository{database: db}
	state, err := repo.GetSubsystemDeploymentState(ctx, op.TenantID, op.ApplicationCode, op.EnvironmentCode)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := repo.RecoverStaleSubsystemDeployment(ctx, state, time.Now().Add(-20*time.Minute), time.Now())
	if err != nil || changed {
		t.Fatalf("owned generation swept: %v %v", changed, err)
	}
	db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).Updates(map[string]any{"active_slot": nil, "status": "FAILED"})
	changed, err = repo.RecoverStaleSubsystemDeployment(ctx, state, time.Now().Add(-20*time.Minute), time.Now())
	if err != nil || !changed {
		t.Fatalf("abandoned generation not recoverable: %v %v", changed, err)
	}
}

// TestAccessStaleStageRejectedOnMySQL 用途：拒绝同名 Worker 旧领取轮次迟到写入。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessStaleStageRejectedOnMySQL(t *testing.T) {
	repo, _, db, op := lifecycleDatabase(t)
	db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).Update("attempt_count", op.AttemptCount+1)
	err := repo.UpsertClaimedOperationStage(context.Background(), op, application.SubsystemAccessStageDeploy, application.SubsystemAccessStatusSucceeded, "", "", "old-result", time.Now())
	if !errors.Is(err, application.ErrSubsystemAccessOperationLeaseLost) {
		t.Fatal("old stage accepted", err)
	}
	var count int64
	db.Model(&subsystemAccessOperationStageModel{}).Count(&count)
	if count != 0 {
		t.Fatal("old stage wrote before lease check")
	}
}

// TestAccessManifestRetryClaimsNewGenerationOnMySQL 用途：防止核验失败恢复反复复用旧成功回执。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessManifestRetryClaimsNewGenerationOnMySQL(t *testing.T) {
	repo, reader, db, op := lifecycleDatabase(t)
	ctx := context.Background()
	if _, err := reader.PrepareAccessDeployment(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := reader.DispatchAccessDeployment(ctx, op, "manifest-1", "candidate-1"); err != nil {
		t.Fatal(err)
	}
	if err := reader.CompleteAccessDeployment(ctx, op, "manifest-1"); err != nil {
		t.Fatal(err)
	}
	_, err := repo.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{TenantID: op.TenantID, ID: op.ID, LeaseOwner: op.LeaseOwner, Status: application.SubsystemAccessStatusFailed, Stage: application.SubsystemAccessStageReadiness, ErrorCode: "MANIFEST_MISMATCH", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.RequeueOperation(ctx, op.TenantID, op.ID, time.Now()); err != nil || !ok {
		t.Fatalf("resume: %v %v", ok, err)
	}
	next, ok, err := repo.ClaimOperation(ctx, op.TenantID, op.ID, "new-worker", time.Minute, time.Now())
	if err != nil || !ok {
		t.Fatal("reclaim", err)
	}
	if next.Stage != application.SubsystemAccessStageDeploy {
		t.Fatal("manifest retry did not return to deploy")
	}
	if phase, err := reader.PrepareAccessDeployment(ctx, next); err != nil || phase != "PREPARED" {
		t.Fatalf("old success reused: %s %v", phase, err)
	}
	var state subsystemDeploymentStateModel
	db.Take(&state)
	if state.Generation != 6 || state.Status != "UPDATING" {
		t.Fatalf("retry did not own a new generation: %+v", state)
	}
}

// TestAccessExhaustedAttemptRecoveryOnMySQL 用途：覆盖错误耗尽、看护和原任务恢复的连续边界。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessExhaustedAttemptRecoveryOnMySQL(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(fmt.Sprint(dispatched), func(t *testing.T) {
			repo, reader, db, op := lifecycleDatabase(t)
			ctx := context.Background()
			if _, err := reader.PrepareAccessDeployment(ctx, op); err != nil {
				t.Fatal(err)
			}
			if dispatched {
				if err := reader.DispatchAccessDeployment(ctx, op, "manifest-1", "candidate-1"); err != nil {
					t.Fatal(err)
				}
			}
			_, err := repo.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{TenantID: op.TenantID, ID: op.ID, LeaseOwner: op.LeaseOwner, Status: application.SubsystemAccessStatusFailed, Stage: application.SubsystemAccessStageDeploy, ErrorCode: "STAGE_EXECUTION_FAILED", Now: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			db.Model(&subsystemDeploymentStateModel{}).Where("tenant_id = ?", op.TenantID).Update("started_at", old)
			stateRepo := &SubsystemOnboardingGORMRepository{database: db}
			state, err := stateRepo.GetSubsystemDeploymentState(ctx, op.TenantID, op.ApplicationCode, op.EnvironmentCode)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := stateRepo.RecoverStaleSubsystemDeployment(ctx, state, time.Now().Add(-20*time.Minute), time.Now())
			if err != nil || changed == dispatched {
				t.Fatalf("sweep dispatched=%v changed=%v err=%v", dispatched, changed, err)
			}
			if ok, err := repo.RequeueOperation(ctx, op.TenantID, op.ID, time.Now()); err != nil || !ok {
				t.Fatal("resume", err)
			}
			next, ok, err := repo.ClaimOperation(ctx, op.TenantID, op.ID, "recover-worker", time.Minute, time.Now())
			if err != nil || !ok {
				t.Fatal("claim", err)
			}
			phase, err := reader.PrepareAccessDeployment(ctx, next)
			if err != nil {
				t.Fatal("recovery lost original binding", err)
			}
			if dispatched {
				if phase != "DISPATCHED" {
					t.Fatal("unknown effect became replayable")
				}
				if err := reader.CompleteAccessDeployment(ctx, next, "manifest-1"); err != nil {
					t.Fatal("original receipt cannot close original generation", err)
				}
			} else if phase != "PREPARED" {
				t.Fatal("unexecuted attempt cannot recover")
			}
		})
	}
}

// TestAccessCompletionRollbackOnMySQL 用途：验证成功回执保存失败不会泄漏 READY 或实际清单。
// 参数：t 为隔离测试上下文。
// 返回值：无。
func TestAccessCompletionRollbackOnMySQL(t *testing.T) {
	_, reader, db, op := lifecycleDatabase(t)
	ctx := context.Background()
	if _, err := reader.PrepareAccessDeployment(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := reader.DispatchAccessDeployment(ctx, op, "manifest-1", "candidate-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_attempt_completion BEFORE UPDATE ON subsystem_access_deployment_attempt FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'isolated completion failure'").Error; err != nil {
		t.Fatal(err)
	}
	if err := reader.CompleteAccessDeployment(ctx, op, "manifest-1"); err == nil {
		t.Fatal("completion write failure ignored")
	}
	var state subsystemDeploymentStateModel
	if err := db.Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Status != "UPDATING" || state.AppliedManifestChecksum != nil {
		t.Fatal("partial completion escaped transaction")
	}
}
