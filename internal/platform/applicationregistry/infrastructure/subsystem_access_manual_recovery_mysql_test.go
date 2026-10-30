package infrastructure

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
)

type manualRecoveryMySQLAgent struct {
	deploymentProvisionerStub
	value   *application.SubsystemAccessDeploymentOutcome
	onFence func()
}

// CheckAccessDeploymentRecovery 提供事务测试的固定Agent事实替身。
// 参数：request限定测试原代次；此替身不操作真实服务器。
// 返回值：可校验摘要，不代表外部系统验收。
func (a *manualRecoveryMySQLAgent) CheckAccessDeploymentRecovery(_ context.Context,
	request application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error) {
	files := []application.SubsystemAccessRecoveryFile{{Path: "runtime/contract.env", Present: true, SHA256: strings.Repeat("b", 64)}}
	containers := []application.SubsystemAccessRecoveryContainer{{ServiceID: "contract-api", Present: false}}
	digest, err := accessRecoveryEvidenceDigest(request.Receipt, files, containers)
	return application.SubsystemAccessRecoveryEvidence{Receipt: request.Receipt,
		RuntimeFiles: files, Containers: containers, EvidenceDigest: digest}, err
}

// AbandonAccessDeployment 模拟Agent已保存决定后发生平台并发变更的边界。
// 参数：request为平台组装的原绑定及人工原因。
// 返回值：测试中保存的真实对象；文件持久化由独立Agent测试验证。
func (a *manualRecoveryMySQLAgent) AbandonAccessDeployment(ctx context.Context,
	request application.SubsystemAccessAbandonRequest) (application.SubsystemAccessDeploymentOutcome, error) {
	if a.value != nil {
		return *a.value, nil
	}
	evidence, err := a.CheckAccessDeploymentRecovery(ctx, request)
	if err != nil {
		return application.SubsystemAccessDeploymentOutcome{}, err
	}
	a.value = &application.SubsystemAccessDeploymentOutcome{Receipt: request.Receipt,
		State: application.AccessDeploymentAbandoned, Code: application.AccessOperatorAbandoned,
		OperatorID: request.OperatorID, Reason: request.Reason, EvidenceDigest: evidence.EvidenceDigest,
		RuntimeFiles: evidence.RuntimeFiles, Containers: evidence.Containers, RecordedAt: time.Now().UTC()}
	if a.onFence != nil {
		a.onFence()
	}
	return *a.value, nil
}

// waitingRecoveryDatabase 建立明确隔离测试库中的原未知派发任务。
// 参数：t为需显式DSN的测试上下文。
// 返回值：仓储、代次读取器、数据库和已转WAITING的任务。
func waitingRecoveryDatabase(t *testing.T) (*SubsystemAccessOperationGORMRepository,
	*SubsystemAccessDeploymentStateGORMReader, *gorm.DB, application.SubsystemAccessOperation) {
	t.Helper()
	repo, reader, db, op := lifecycleDatabase(t)
	ctx := context.Background()
	if err := db.Model(&subsystemDeploymentStateModel{}).Where("tenant_id = ?", op.TenantID).
		Update("applied_manifest_checksum", "previous-applied").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := reader.PrepareAccessDeployment(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := reader.DispatchAccessDeployment(ctx, op, "manifest-1", "candidate-1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertOperationStage(ctx, op.TenantID, op.ID, application.SubsystemAccessStageDeploy,
		application.SubsystemAccessStatusWaiting, "DEPLOYMENT_EFFECT_UNKNOWN", "unknown", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CompleteOperation(ctx, application.SubsystemAccessOperationCompletion{
		TenantID: op.TenantID, ID: op.ID, LeaseOwner: op.LeaseOwner,
		Status: application.SubsystemAccessStatusWaiting, Stage: application.SubsystemAccessStageDeploy,
		ErrorCode: "DEPLOYMENT_EFFECT_UNKNOWN", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	op, err := repo.GetOperation(ctx, op.TenantID, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return repo, reader, db, op
}

// TestAccessManualRecoveryCASOnMySQL 验证真实事务、并发版本与旧代次隔离。
// 参数：t为本轮专用可销毁MySQL库的测试上下文。
// 返回值：无；Agent替身与真实数据库边界分别记录，不冒充现场验收。
func TestAccessManualRecoveryCASOnMySQL(t *testing.T) {
	for _, mode := range []string{"success", "version_drift_after_fence", "wrong_generation"} {
		t.Run(mode, func(t *testing.T) {
			repo, reader, db, op := waitingRecoveryDatabase(t)
			ctx := context.Background()
			agent := &manualRecoveryMySQLAgent{}
			adapter, err := NewSubsystemAccessManualRecoveryAdapter(db, agent)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := adapter.PreviewSubsystemAccessRecovery(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			input := application.SubsystemAccessManualRecoveryInput{TenantID: op.TenantID,
				OperationID: op.ID, OperatorID: "operator-1", ExpectedVersion: op.Version,
				Generation: evidence.Receipt.Generation, EvidenceDigest: evidence.EvidenceDigest,
				Reason: "verified original failure and current isolated runtime facts"}
			if mode == "wrong_generation" {
				input.Generation++
			}
			if mode == "version_drift_after_fence" {
				agent.onFence = func() {
					if err := db.Model(&subsystemAccessOperationModel{}).Where("id = ?", op.ID).
						Update("version", gorm.Expr("version + 1")).Error; err != nil {
						t.Error(err)
					}
				}
			}
			err = adapter.AbandonSubsystemAccess(ctx, op, input)
			if mode != "success" {
				if !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
					t.Fatal("stale recovery accepted", err)
				}
				current, _ := repo.GetOperation(ctx, op.TenantID, op.ID)
				if current.Status != application.SubsystemAccessStatusWaiting {
					t.Fatal("conflict changed operation")
				}
				if mode == "wrong_generation" && agent.value != nil {
					t.Fatal("wrong generation reached agent")
				}
				if mode == "version_drift_after_fence" && agent.value == nil {
					t.Fatal("fence proof was lost")
				}
				if mode == "wrong_generation" {
					return
				}
				// 原决定已经落盘，刷新版本后只能继续收口原代次，不能重放部署。
				input.ExpectedVersion = current.Version
				err = adapter.AbandonSubsystemAccess(ctx, current, input)
			}
			if err != nil {
				t.Fatal(err)
			}
			var row subsystemAccessOperationModel
			db.Where("id = ?", op.ID).Take(&row)
			if row.Status != "FAILED" || row.ActiveSlot != nil || row.LeaseOwner != nil {
				t.Fatalf("operation not closed: %s", row.Status)
			}
			var state subsystemDeploymentStateModel
			db.Where("tenant_id = ?", op.TenantID).Take(&state)
			if state.Status != string(application.SubsystemDeploymentStatusFailed) || accessDerefString(state.AppliedManifestChecksum) != "previous-applied" {
				t.Fatal("manual decision falsely completed deployment")
			}
			if ok, err := repo.RequeueOperation(ctx, op.TenantID, op.ID, time.Now()); err != nil || !ok {
				t.Fatal("explicit resume blocked", err)
			}
			retried, ok, err := repo.ClaimOperation(ctx, op.TenantID, op.ID, "retry-worker", time.Minute, time.Now())
			if err != nil || !ok {
				t.Fatal("retry claim", err)
			}
			if _, err := reader.PrepareAccessDeployment(ctx, retried); err != nil {
				t.Fatal(err)
			}
			if err := reader.DispatchAccessDeployment(ctx, retried, "manifest-1", "candidate-2"); err != nil {
				t.Fatal(err)
			}
			receipt, _, _ := reader.ReadAccessDeploymentReceipt(ctx, retried)
			if receipt.Generation != input.Generation+1 || accessCredentialOperation(retried) != "RETRY" {
				t.Fatal("retry reused old generation or credentials")
			}
		})
	}
}

// TestAccessManualRecoveryRollbackOnMySQL 验证阶段缺失时整个平台事务回滚。
// 参数：t为本轮专用可销毁MySQL库的测试上下文。
// 返回值：无，Agent栅栏必须保留而生命周期及原任务不得部分收口。
func TestAccessManualRecoveryRollbackOnMySQL(t *testing.T) {
	repo, _, db, op := waitingRecoveryDatabase(t)
	ctx := context.Background()
	agent := &manualRecoveryMySQLAgent{}
	adapter, err := NewSubsystemAccessManualRecoveryAdapter(db, agent)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := adapter.PreviewSubsystemAccessRecovery(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Where("operation_id = ?", op.ID).Delete(&subsystemAccessOperationStageModel{}).Error; err != nil {
		t.Fatal(err)
	}
	err = adapter.AbandonSubsystemAccess(ctx, op, application.SubsystemAccessManualRecoveryInput{
		TenantID: op.TenantID, OperationID: op.ID, OperatorID: "operator-1", ExpectedVersion: op.Version,
		Generation: evidence.Receipt.Generation, EvidenceDigest: evidence.EvidenceDigest,
		Reason: "verified isolated facts; missing stage must roll back the platform transaction"})
	if !errors.Is(err, application.ErrSubsystemAccessOperationConflict) || agent.value == nil {
		t.Fatal("missing stage must preserve fence and reject transaction", err)
	}
	current, err := repo.GetOperation(ctx, op.TenantID, op.ID)
	if err != nil || current.Status != application.SubsystemAccessStatusWaiting || current.Version != op.Version {
		t.Fatal("operation partially committed", err)
	}
	var attempt subsystemAccessDeploymentAttempt
	if err = db.Where("operation_id = ?", op.ID).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	var state subsystemDeploymentStateModel
	if err = db.Where("tenant_id = ?", op.TenantID).Take(&state).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Phase != "DISPATCHED" || state.Status != string(application.SubsystemDeploymentStatusUpdating) ||
		accessDerefString(state.AppliedManifestChecksum) != "previous-applied" {
		t.Fatal("lifecycle partially committed")
	}
}
