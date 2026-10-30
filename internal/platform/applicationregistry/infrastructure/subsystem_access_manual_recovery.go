package infrastructure

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type accessDeploymentRecoveryAgent interface {
	CheckAccessDeploymentRecovery(context.Context, application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error)
	AbandonAccessDeployment(context.Context, application.SubsystemAccessAbandonRequest) (application.SubsystemAccessDeploymentOutcome, error)
}

type SubsystemAccessManualRecoveryAdapter struct {
	database *gorm.DB
	agent    accessDeploymentRecoveryAgent
}

// NewSubsystemAccessManualRecoveryAdapter 接线正式人工恢复，不为旧Agent伪造能力。
// 参数：database为平台库，provisioner为受控部署Agent客户端。
// 返回值：适配器；基础依赖缺失返回错误，旧端按调用时不可用处理。
func NewSubsystemAccessManualRecoveryAdapter(database *gorm.DB,
	provisioner application.SubsystemProvisioner) (*SubsystemAccessManualRecoveryAdapter, error) {
	if database == nil || provisioner == nil {
		return nil, errors.New("manual recovery requires database and deployment agent")
	}
	agent, _ := provisioner.(accessDeploymentRecoveryAgent)
	return &SubsystemAccessManualRecoveryAdapter{database: database, agent: agent}, nil
}

// manualRecoveryBinding 只读取同租户原任务的尚未收口派发代次。
// 参数：ctx控制查询，operation为应用服务授权的原任务。
// 返回值：原始回执；目录身份、当前代次或状态漂移时拒绝。
func (a *SubsystemAccessManualRecoveryAdapter) manualRecoveryBinding(ctx context.Context,
	operation application.SubsystemAccessOperation) (application.SubsystemAccessDeploymentReceipt, error) {
	var receipt application.SubsystemAccessDeploymentReceipt
	if a.agent == nil {
		return receipt, application.ErrSubsystemAccessStageNotWired
	}
	var attempt subsystemAccessDeploymentAttempt
	if err := a.database.WithContext(ctx).Where("tenant_id = ? AND operation_id = ?",
		operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
		return receipt, err
	}
	var state subsystemDeploymentStateModel
	if err := a.database.WithContext(ctx).Where("tenant_id = ? AND application_code = ? AND environment_code = ?",
		operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode).Take(&state).Error; err != nil {
		return receipt, err
	}
	if attempt.Phase != "DISPATCHED" || attempt.ApplicationID != operation.ApplicationID ||
		attempt.EnvironmentID != operation.EnvironmentID || state.ApplicationID != operation.ApplicationID ||
		state.EnvironmentID != operation.EnvironmentID || state.Generation != attempt.Generation ||
		(state.Status != string(application.SubsystemDeploymentStatusUpdating) && state.Status != string(application.SubsystemDeploymentStatusProvisioning)) {
		return receipt, application.ErrSubsystemAccessOperationConflict
	}
	return accessAttemptReceipt(operation, attempt), nil
}

// validateRecoveryEvidence 拒绝跨目标或与真实事实不符的预检摘要。
// 参数：value为受限Agent应答，receipt为平台原绑定。
// 返回值：完全匹配时为空，否则失败关闭。
func validateRecoveryEvidence(value application.SubsystemAccessRecoveryEvidence,
	receipt application.SubsystemAccessDeploymentReceipt) error {
	digest, err := accessRecoveryEvidenceDigest(receipt, value.RuntimeFiles, value.Containers)
	if err != nil || value.Receipt != receipt || value.EvidenceDigest != digest ||
		len(value.RuntimeFiles) == 0 || len(value.Containers) == 0 {
		return application.ErrSubsystemAccessOperationConflict
	}
	return nil
}

// PreviewSubsystemAccessRecovery 从原代次取数并调用Agent锁内真实预检。
// 参数：ctx为请求上下文，operation为同租户原未知任务。
// 返回值：可审查事实；不支持、漂移或不可观察时返回错误。
func (a *SubsystemAccessManualRecoveryAdapter) PreviewSubsystemAccessRecovery(ctx context.Context,
	operation application.SubsystemAccessOperation) (application.SubsystemAccessRecoveryEvidence, error) {
	var evidence application.SubsystemAccessRecoveryEvidence
	receipt, err := a.manualRecoveryBinding(ctx, operation)
	if err != nil {
		return evidence, err
	}
	evidence, err = a.agent.CheckAccessDeploymentRecovery(ctx,
		application.SubsystemAccessAbandonRequest{Receipt: receipt})
	if err != nil {
		return evidence, err
	}
	return evidence, validateRecoveryEvidence(evidence, receipt)
}

// AbandonSubsystemAccess 先取得Agent真实终止栅栏，再事务CAS原任务。
// 参数：operation为原任务，input含人工核对版本/代次与可信操作者。
// 返回值：仅正式失败收口后为空；栅栏成功而CAS失败显式返回冲突。
func (a *SubsystemAccessManualRecoveryAdapter) AbandonSubsystemAccess(ctx context.Context,
	operation application.SubsystemAccessOperation, input application.SubsystemAccessManualRecoveryInput) error {
	if input.TenantID != operation.TenantID || input.OperationID != operation.ID ||
		input.ExpectedVersion != operation.Version {
		return application.ErrSubsystemAccessOperationConflict
	}
	receipt, err := a.manualRecoveryBinding(ctx, operation)
	if err != nil {
		return err
	}
	if input.Generation != receipt.Generation {
		return application.ErrSubsystemAccessOperationConflict
	}
	value, err := a.agent.AbandonAccessDeployment(ctx, application.SubsystemAccessAbandonRequest{
		Receipt: receipt, OperatorID: input.OperatorID, Reason: input.Reason, EvidenceDigest: input.EvidenceDigest})
	if err != nil {
		return err
	}
	if err = validateAccessOutcome(value, receipt); err != nil {
		return err
	}
	if value.State != application.AccessDeploymentAbandoned || value.EvidenceDigest != input.EvidenceDigest {
		return application.ErrSubsystemAccessOperationConflict
	}
	return a.persistManualRecovery(ctx, operation, input.ExpectedVersion, value)
}

// lockManualRecoveryOperation 锁定尚未被继续处理的原未知任务。
// 参数：tx为事务，operation为原目标，version为操作员预检版本。
// 返回值：状态、版本、租约或目标变化时返回冲突。
func lockManualRecoveryOperation(tx *gorm.DB, operation application.SubsystemAccessOperation, version uint64) error {
	var row subsystemAccessOperationModel
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?",
		operation.TenantID, operation.ID).Take(&row).Error; err != nil {
		return err
	}
	if row.Version != version || row.Status != string(application.SubsystemAccessStatusWaiting) ||
		row.Stage != string(application.SubsystemAccessStageDeploy) || accessDerefString(row.ErrorCode) != "DEPLOYMENT_EFFECT_UNKNOWN" ||
		row.LeaseOwner != nil || row.ApplicationCode != operation.ApplicationCode || row.EnvironmentCode != operation.EnvironmentCode ||
		accessDerefString(row.ApplicationID) != operation.ApplicationID || accessDerefString(row.EnvironmentID) != operation.EnvironmentID ||
		row.Action != string(operation.Action) {
		return application.ErrSubsystemAccessOperationConflict
	}
	return nil
}

// persistManualRecovery 保留原应用事实并原子收口操作、阶段及部署生命周期。
// 参数：ctx控制事务，operation/version限定原操作，value为已落盘终止证明。
// 返回值：任一代次、版本或写入异常整体回滚，Agent栅栏仍保留。
func (a *SubsystemAccessManualRecoveryAdapter) persistManualRecovery(ctx context.Context,
	operation application.SubsystemAccessOperation, version uint64, value application.SubsystemAccessDeploymentOutcome) error {
	return a.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockManualRecoveryOperation(tx, operation, version); err != nil {
			return err
		}
		var attempt subsystemAccessDeploymentAttempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND operation_id = ?",
			operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
			return err
		}
		if attempt.Phase != "DISPATCHED" || accessAttemptReceipt(operation, attempt) != value.Receipt {
			return application.ErrSubsystemAccessOperationConflict
		}
		if err := verifyAccessAttempt(ctx, tx, operation, attempt); err != nil {
			if errors.Is(err, application.ErrSubsystemDeploymentTransition) {
				return application.ErrSubsystemAccessOperationConflict
			}
			return err
		}
		now := time.Now().UTC()
		message := "人工核对后终止原代次；未声明部署成功或回滚。" + value.Reason
		reference := accessDeploymentOutcomeReference(value)
		repo := &SubsystemOnboardingGORMRepository{database: tx}
		if err := repo.CompleteSubsystemDeployment(ctx, operation.TenantID, operation.ApplicationCode,
			operation.EnvironmentCode, attempt.Generation, application.SubsystemDeploymentStatusFailed,
			string(operation.Action), value.Code, message, now); err != nil {
			return err
		}
		if err := tx.Model(&attempt).Updates(map[string]any{"phase": "FAILED", "updated_at": now}).Error; err != nil {
			return err
		}
		stage := tx.Model(&subsystemAccessOperationStageModel{}).Where("tenant_id = ? AND operation_id = ? AND stage = ?",
			operation.TenantID, operation.ID, string(application.SubsystemAccessStageDeploy)).Updates(map[string]any{
			"status": "FAILED", "error_code": value.Code, "error_message": message, "evidence_ref": reference,
			"finished_at": now, "updated_at": now, "version": gorm.Expr("version + 1")})
		if stage.Error != nil {
			return stage.Error
		}
		if stage.RowsAffected != 1 {
			return application.ErrSubsystemAccessOperationConflict
		}
		result := tx.Model(&subsystemAccessOperationModel{}).Where("tenant_id = ? AND id = ? AND version = ?",
			operation.TenantID, operation.ID, version).Updates(map[string]any{"status": "FAILED",
			"error_code": value.Code, "error_message": message, "result_ref": reference,
			"active_slot": nil, "lease_owner": nil, "lease_expires_at": nil,
			"updated_at": now, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return application.ErrSubsystemAccessOperationConflict
		}
		return nil
	})
}
