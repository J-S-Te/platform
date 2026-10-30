package infrastructure

import (
	"context"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type accessDeploymentOutcomeReader interface {
	ReadAccessDeploymentOutcome(context.Context, application.SubsystemAccessDeploymentReceipt) (
		application.SubsystemAccessDeploymentOutcome, bool, error)
}

// recoverAccessDeploymentRejection 仅按真实且精确的Agent拒绝事实收口失败。
// 参数：ctx为领取上下文，operation为任务，receipt为原派发代次。
// 返回值：无证明返回found=false；证明或代次异常返回错误，不授权重放。
func (adapter *SubsystemAccessDeploymentAdapter) recoverAccessDeploymentRejection(ctx context.Context,
	operation application.SubsystemAccessOperation, receipt application.SubsystemAccessDeploymentReceipt) (
	application.SubsystemAccessStageOutcome, bool, error) {
	agent, ok := adapter.provisioner.(accessDeploymentOutcomeReader)
	if !ok {
		return application.SubsystemAccessStageOutcome{}, false, nil
	}
	value, found, err := agent.ReadAccessDeploymentOutcome(ctx, receipt)
	if err != nil || !found {
		return application.SubsystemAccessStageOutcome{}, false, err
	}
	if err = validateAccessOutcome(value, receipt); err != nil {
		return application.SubsystemAccessStageOutcome{}, false, err
	}
	if value.State != application.AccessDeploymentRejected {
		return application.SubsystemAccessStageOutcome{}, false, nil
	}
	store, ok := adapter.state.(interface {
		RejectAccessDeployment(context.Context, application.SubsystemAccessOperation,
			application.SubsystemAccessDeploymentOutcome) error
	})
	if !ok {
		return application.SubsystemAccessStageOutcome{}, true, application.ErrSubsystemAccessStageNotWired
	}
	if err = store.RejectAccessDeployment(ctx, operation, value); err != nil {
		return application.SubsystemAccessStageOutcome{}, true, err
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
		ErrorCode:   application.AccessCredentialRetryRequired,
		Message:     "部署开始前凭据校验拒绝；请显式继续恢复，按受控RETRY重建机器凭据",
		EvidenceRef: accessDeploymentOutcomeReference(value)}, true, nil
}

// RejectAccessDeployment 校验原代次真实拒绝后事务收口，不写READY或替换applied清单。
// 参数：ctx及operation限定有效租约，value为受信Agent已持久的拒绝事实。
// 返回值：成功或同代已收口返回空，租约、代次及事实冲突返回错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) RejectAccessDeployment(ctx context.Context,
	operation application.SubsystemAccessOperation, value application.SubsystemAccessDeploymentOutcome) error {
	if value.State != application.AccessDeploymentRejected ||
		validateAccessOutcome(value, value.Receipt) != nil {
		return application.ErrValidation
	}
	return reader.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockAccessOperation(ctx, tx, operation, time.Now().UTC()); err != nil {
			return err
		}
		var attempt subsystemAccessDeploymentAttempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).
			Take(&attempt).Error; err != nil {
			return err
		}
		expected := accessAttemptReceipt(operation, attempt)
		if expected != value.Receipt || (attempt.Phase != "DISPATCHED" && attempt.Phase != "FAILED") {
			return application.ErrSubsystemAccessOperationConflict
		}
		if err := verifyAccessAttempt(ctx, tx, operation, attempt); err != nil {
			return err
		}
		if attempt.Phase == "FAILED" {
			return nil
		}
		repo := &SubsystemOnboardingGORMRepository{database: tx}
		if err := repo.CompleteSubsystemDeployment(ctx, operation.TenantID, operation.ApplicationCode,
			operation.EnvironmentCode, attempt.Generation, application.SubsystemDeploymentStatusFailed,
			string(operation.Action), value.Code, "Agent已持久确认部署前凭据拒绝", time.Now().UTC()); err != nil {
			return err
		}
		return tx.Model(&attempt).Updates(map[string]any{"phase": "FAILED", "updated_at": time.Now().UTC()}).Error
	})
}

// accessAttemptReceipt 从原任务与原代次生成精确回执绑定。
// 参数：operation为原任务，attempt为数据库中的同租户代次。
// 返回值：不含秘密的原绑定，不采用当前清单反推。
func accessAttemptReceipt(operation application.SubsystemAccessOperation,
	attempt subsystemAccessDeploymentAttempt) application.SubsystemAccessDeploymentReceipt {
	return application.SubsystemAccessDeploymentReceipt{TenantID: operation.TenantID,
		OperationID: operation.ID, ApplicationCode: operation.ApplicationCode,
		Environment: operation.EnvironmentCode, Generation: attempt.Generation,
		ManifestChecksum: attempt.ManifestChecksum}
}
