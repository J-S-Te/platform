package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type accessDeploymentRecoveryAgent interface {
	CheckAccessDeploymentRecovery(context.Context, application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error)
	AbandonAccessDeployment(context.Context, application.SubsystemAccessAbandonRequest) (application.SubsystemAccessDeploymentOutcome, error)
}

// accessDeploymentSupersessionReader 只读确认原代次既无成功回执也无终止结果。
// 它与 accessDeploymentRecoveryAgent 分开声明：旧 Agent 不实现该能力时，取代路径
// 必须失败关闭，而不是退化成"凭猜测收口"。
type accessDeploymentSupersessionReader interface {
	ReadAccessDeploymentSupersession(context.Context, application.SubsystemAccessAbandonRequest) (application.SubsystemAccessRecoveryEvidence, error)
}

// accessSupersededMessage 是取代收口的固定说明：只描述事实，不声明成功也不声明回滚。
const accessSupersededMessage = "人工核对确认原代次已被当前审核清单取代：既无成功回执也无终止结果；" +
	"本次只做失败收口，不声明部署成功，也不自动回滚。"

type SubsystemAccessManualRecoveryAdapter struct {
	database     *gorm.DB
	agent        accessDeploymentRecoveryAgent
	supersession accessDeploymentSupersessionReader
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
	supersession, _ := provisioner.(accessDeploymentSupersessionReader)
	return &SubsystemAccessManualRecoveryAdapter{database: database, agent: agent, supersession: supersession}, nil
}

// recoveryBinding 读取同租户原任务尚未收口的派发代次与部署状态。
// 参数：ctx控制查询，operation为应用服务授权的原任务。
// 返回值：原代次与状态行；目录身份、当前代次或状态漂移时拒绝。
func (a *SubsystemAccessManualRecoveryAdapter) recoveryBinding(ctx context.Context,
	operation application.SubsystemAccessOperation) (subsystemAccessDeploymentAttempt, subsystemDeploymentStateModel, error) {
	var attempt subsystemAccessDeploymentAttempt
	var state subsystemDeploymentStateModel
	if a.agent == nil && a.supersession == nil {
		return attempt, state, application.ErrSubsystemAccessStageNotWired
	}
	if err := a.database.WithContext(ctx).Where("tenant_id = ? AND operation_id = ?",
		operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
		return attempt, state, err
	}
	if err := a.database.WithContext(ctx).Where("tenant_id = ? AND application_code = ? AND environment_code = ?",
		operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode).Take(&state).Error; err != nil {
		return attempt, state, err
	}
	if attempt.Phase != "DISPATCHED" || attempt.ApplicationID != operation.ApplicationID ||
		attempt.EnvironmentID != operation.EnvironmentID || state.ApplicationID != operation.ApplicationID ||
		state.EnvironmentID != operation.EnvironmentID || state.Generation != attempt.Generation ||
		(state.Status != string(application.SubsystemDeploymentStatusUpdating) && state.Status != string(application.SubsystemDeploymentStatusProvisioning)) {
		return attempt, state, application.ErrSubsystemAccessOperationConflict
	}
	return attempt, state, nil
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
	attempt, _, err := a.recoveryBinding(ctx, operation)
	if err != nil {
		return receipt, err
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

// PreviewSubsystemAccessRecovery 判定审核基准是否仍然一致，并返回可审查事实。
// 参数：ctx为请求上下文，operation为同租户原未知任务。
// 返回值：fenced 模式的运行事实，或 superseded 模式的否定证明与当前清单摘要。
//
// 模式判定由 Agent 报告的当前清单摘要决定：与原回执一致走原有 fenced 路径（仍要求
// 运行事实与摘要），不一致则只能只读证明"无成功回执 ∧ 无终止结果"。
func (a *SubsystemAccessManualRecoveryAdapter) PreviewSubsystemAccessRecovery(ctx context.Context,
	operation application.SubsystemAccessOperation) (application.SubsystemAccessRecoveryEvidence, error) {
	var evidence application.SubsystemAccessRecoveryEvidence
	receipt, err := a.manualRecoveryBinding(ctx, operation)
	if err != nil {
		return evidence, err
	}
	if a.supersession == nil {
		// 旧 Agent 不认识取代证明：保持原行为，清单漂移时继续由 fenced 预检拒绝。
		// fenced 预检成功本身已经证明原回执清单等于 Agent 当前清单，可据此补齐摘要字段，
		// 让旧 Agent 的人工恢复响应也带上同一组公共字段。
		evidence, err := a.agent.CheckAccessDeploymentRecovery(ctx,
			application.SubsystemAccessAbandonRequest{Receipt: receipt})
		if err != nil {
			return evidence, err
		}
		evidence.CurrentProfileChecksum = evidence.Receipt.ManifestChecksum
		return evidence, nil
	}
	proof, err := a.supersession.ReadAccessDeploymentSupersession(ctx,
		application.SubsystemAccessAbandonRequest{Receipt: receipt})
	if err != nil {
		return evidence, err
	}
	if proof.Receipt != receipt || !application.ValidSubsystemAccessChecksum(proof.CurrentProfileChecksum) {
		return evidence, application.ErrSubsystemAccessOperationConflict
	}
	if proof.CurrentProfileChecksum == receipt.ManifestChecksum {
		// 清单未漂移：沿用原有 fenced 核对（含运行事实与发布完整性校验）。
		return a.agent.CheckAccessDeploymentRecovery(ctx,
			application.SubsystemAccessAbandonRequest{Receipt: receipt})
	}
	if proof.SuccessReceipt || proof.TerminalOutcome {
		// 已有成功回执或终止结果：不是"取代收口"能处理的事实，交回正常恢复路径。
		return evidence, application.ErrSubsystemAccessOperationConflict
	}
	if len(proof.RuntimeFiles) != 0 || len(proof.Containers) != 0 || proof.EvidenceDigest != "" {
		return evidence, application.ErrSubsystemAccessOperationConflict
	}
	return proof, nil
}

// validateSupersededRecovery 逐条核验"原代次已被取代"的恢复前提。
// 参数：operation为原任务，attempt/state为平台持久绑定，proof为Agent否定证明，
// input为操作者携带的版本/代次/双摘要。
// 返回值：全部满足时为空，任一条件不满足都失败关闭。
//
// 前提清单（与设计评审逐条对应）：
//  1. operation.status == WAITING
//  2. operation.stage == DEPLOY
//  3. operation.error_code == DEPLOYMENT_EFFECT_UNKNOWN
//  4. operation 无活动租约（lease_owner 为空）
//  5. attempt.phase == DISPATCHED
//  6. operation.application/environment 与 attempt、state 一致
//  7. state.generation == attempt.generation
//  8. state.status ∈ {UPDATING, PROVISIONING}
//  9. attempt.manifest_checksum != 当前清单摘要（相等必须走原 fenced 路径）
//  10. Agent 确认"无成功回执 ∧ 无终止结果"
func validateSupersededRecovery(operation application.SubsystemAccessOperation,
	attempt subsystemAccessDeploymentAttempt, state subsystemDeploymentStateModel,
	proof application.SubsystemAccessRecoveryEvidence,
	input application.SubsystemAccessManualRecoveryInput) error {
	conflict := application.ErrSubsystemAccessOperationConflict
	if operation.Status != application.SubsystemAccessStatusWaiting ||
		operation.Stage != application.SubsystemAccessStageDeploy ||
		operation.ErrorCode != application.AccessDeploymentEffectUnknown || operation.LeaseOwner != "" {
		return conflict
	}
	if attempt.Phase != "DISPATCHED" {
		return conflict
	}
	if attempt.TenantID != operation.TenantID || attempt.ApplicationID != operation.ApplicationID ||
		attempt.EnvironmentID != operation.EnvironmentID || state.TenantID != operation.TenantID ||
		state.ApplicationID != operation.ApplicationID || state.EnvironmentID != operation.EnvironmentID {
		return conflict
	}
	if state.Generation != attempt.Generation {
		return conflict
	}
	if state.Status != string(application.SubsystemDeploymentStatusProvisioning) &&
		state.Status != string(application.SubsystemDeploymentStatusUpdating) {
		return conflict
	}
	receipt := accessAttemptReceipt(operation, attempt)
	current := strings.TrimSpace(proof.CurrentProfileChecksum)
	if proof.Receipt != receipt || proof.SuccessReceipt || proof.TerminalOutcome ||
		!application.ValidSubsystemAccessChecksum(receipt.ManifestChecksum) ||
		!application.ValidSubsystemAccessChecksum(current) {
		return conflict
	}
	if input.Generation != attempt.Generation ||
		receipt.ManifestChecksum != strings.TrimSpace(input.ReceiptManifestChecksum) ||
		current != strings.TrimSpace(input.CurrentProfileChecksum) {
		return conflict
	}
	// 清单相等说明原代次没有被取代：必须回到原有 fenced 路径，不允许在此收口。
	if receipt.ManifestChecksum == current {
		return conflict
	}
	return nil
}

// AbandonSubsystemAccess 按预检模式收口原未知代次：fenced 先取 Agent 栅栏，superseded 只读证明。
// 参数：operation为原任务，input含人工核对的版本/代次、模式与原因。
// 返回值：仅正式失败收口后为空；栅栏成功而CAS失败显式返回冲突。
func (a *SubsystemAccessManualRecoveryAdapter) AbandonSubsystemAccess(ctx context.Context,
	operation application.SubsystemAccessOperation, input application.SubsystemAccessManualRecoveryInput) error {
	switch strings.TrimSpace(input.Mode) {
	case "", application.SubsystemAccessRecoveryModeFenced:
		return a.abandonFencedRecovery(ctx, operation, input)
	case application.SubsystemAccessRecoveryModeSuperseded:
		return a.abandonSupersededRecovery(ctx, operation, input)
	default:
		return application.ErrSubsystemAccessInvalidRequest
	}
}

// abandonFencedRecovery 在审核基准未变时先取得 Agent 真实终止栅栏，再事务CAS原任务。
// 参数：operation为原任务，input含人工核对版本/代次与可信操作者。
// 返回值：仅正式失败收口后为空；栅栏成功而CAS失败显式返回冲突。
func (a *SubsystemAccessManualRecoveryAdapter) abandonFencedRecovery(ctx context.Context,
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

// abandonSupersededRecovery 只读取 Agent 的否定证明后失败收口已被取代的原代次。
// 参数：operation为原任务，input含版本/代次、双清单摘要与人工原因。
// 返回值：证明或持久事实任一变化都返回冲突，不授权重放部署。
//
// 本路径**不写** Agent 栅栏：Agent 仍是唯一终止写者，此处只消费它的否定证明；
// 因此清单漂移后不依赖任何"当前清单必须等于原清单"的旧条件。
func (a *SubsystemAccessManualRecoveryAdapter) abandonSupersededRecovery(ctx context.Context,
	operation application.SubsystemAccessOperation, input application.SubsystemAccessManualRecoveryInput) error {
	if a.supersession == nil {
		return application.ErrSubsystemAccessStageNotWired
	}
	if input.TenantID != operation.TenantID || input.OperationID != operation.ID ||
		input.ExpectedVersion != operation.Version {
		return application.ErrSubsystemAccessOperationConflict
	}
	attempt, state, err := a.recoveryBinding(ctx, operation)
	if err != nil {
		return err
	}
	if input.Generation != attempt.Generation {
		return application.ErrSubsystemAccessOperationConflict
	}
	proof, err := a.supersession.ReadAccessDeploymentSupersession(ctx,
		application.SubsystemAccessAbandonRequest{Receipt: accessAttemptReceipt(operation, attempt)})
	if err != nil {
		return err
	}
	if err = validateSupersededRecovery(operation, attempt, state, proof, input); err != nil {
		return err
	}
	return a.persistSupersededRecovery(ctx, operation, input, proof)
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
		row.Stage != string(application.SubsystemAccessStageDeploy) || accessDerefString(row.ErrorCode) != application.AccessDeploymentEffectUnknown ||
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
		if err := a.closeAccessRecovery(ctx, tx, operation, attempt, version, value.Code, message, reference, now); err != nil {
			return err
		}
		return nil
	})
}

// persistSupersededRecovery 在单个事务内按"原代次已被取代"失败收口并写追加式审计。
// 参数：ctx控制事务，operation为原任务，input为已核验的人工决定，proof为否定证明。
// 返回值：代次、版本或状态漂移整体回滚；不修改 generation。
func (a *SubsystemAccessManualRecoveryAdapter) persistSupersededRecovery(ctx context.Context,
	operation application.SubsystemAccessOperation, input application.SubsystemAccessManualRecoveryInput,
	proof application.SubsystemAccessRecoveryEvidence) error {
	return a.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockManualRecoveryOperation(tx, operation, input.ExpectedVersion); err != nil {
			return err
		}
		var attempt subsystemAccessDeploymentAttempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND operation_id = ?",
			operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
			return err
		}
		receipt := accessAttemptReceipt(operation, attempt)
		if attempt.Phase != "DISPATCHED" || receipt != proof.Receipt ||
			attempt.ManifestChecksum != strings.TrimSpace(input.ReceiptManifestChecksum) ||
			attempt.ManifestChecksum == strings.TrimSpace(input.CurrentProfileChecksum) {
			return application.ErrSubsystemAccessOperationConflict
		}
		if err := verifyAccessAttempt(ctx, tx, operation, attempt); err != nil {
			if errors.Is(err, application.ErrSubsystemDeploymentTransition) {
				return application.ErrSubsystemAccessOperationConflict
			}
			return err
		}
		// 审计快照取收口前的状态：这是操作者实际审核过的代次事实。
		var snapshot subsystemDeploymentStateModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_code = ? AND environment_code = ?",
			operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode).Take(&snapshot).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		reference := accessSupersededReference(receipt.ManifestChecksum, proof.CurrentProfileChecksum)
		if err := a.closeAccessRecovery(ctx, tx, operation, attempt, input.ExpectedVersion,
			application.AccessDeploymentSupersededUnknown, accessSupersededMessage, reference, now); err != nil {
			return err
		}
		return tx.Create(&subsystemAccessRecoveryAuditModel{
			TenantID: operation.TenantID, ApplicationCode: operation.ApplicationCode,
			EnvironmentCode: operation.EnvironmentCode, OperationID: operation.ID,
			Generation: attempt.Generation, ReceiptManifestChecksum: attempt.ManifestChecksum,
			CurrentProfileChecksum: strings.TrimSpace(proof.CurrentProfileChecksum),
			StateStatus:            snapshot.Status, StateGeneration: snapshot.Generation,
			StateDesiredManifest: accessDerefString(snapshot.DesiredManifestChecksum),
			StateAppliedManifest: accessDerefString(snapshot.AppliedManifestChecksum),
			OperatorID:           input.OperatorID, Reason: input.Reason,
			CreatedAt: now,
		}).Error
	})
}

// closeAccessRecovery 是两种人工恢复共用的收口动作：部署生命周期、代次、阶段与操作。
// 参数：ctx控制事务，operation/attempt/version限定原代次，code/message/reference为失败语义。
// 返回值：任一写入未命中预期行数都返回冲突，调用方整体回滚。
func (a *SubsystemAccessManualRecoveryAdapter) closeAccessRecovery(ctx context.Context, tx *gorm.DB,
	operation application.SubsystemAccessOperation, attempt subsystemAccessDeploymentAttempt,
	version uint64, code, message, reference string, now time.Time) error {
	repo := &SubsystemOnboardingGORMRepository{database: tx}
	if err := repo.CompleteSubsystemDeployment(ctx, operation.TenantID, operation.ApplicationCode,
		operation.EnvironmentCode, attempt.Generation, application.SubsystemDeploymentStatusFailed,
		string(operation.Action), code, message, now); err != nil {
		return err
	}
	if err := tx.Model(&attempt).Updates(map[string]any{"phase": "FAILED", "updated_at": now}).Error; err != nil {
		return err
	}
	stage := tx.Model(&subsystemAccessOperationStageModel{}).Where("tenant_id = ? AND operation_id = ? AND stage = ?",
		operation.TenantID, operation.ID, string(application.SubsystemAccessStageDeploy)).Updates(map[string]any{
		"status": "FAILED", "error_code": code, "error_message": message, "evidence_ref": reference,
		"finished_at": now, "updated_at": now, "version": gorm.Expr("version + 1")})
	if stage.Error != nil {
		return stage.Error
	}
	if stage.RowsAffected != 1 {
		return application.ErrSubsystemAccessOperationConflict
	}
	result := tx.Model(&subsystemAccessOperationModel{}).Where("tenant_id = ? AND id = ? AND version = ?",
		operation.TenantID, operation.ID, version).Updates(map[string]any{"status": "FAILED",
		"error_code": code, "error_message": message, "result_ref": reference,
		"active_slot": nil, "lease_owner": nil, "lease_expires_at": nil,
		"updated_at": now, "version": gorm.Expr("version + 1")})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return application.ErrSubsystemAccessOperationConflict
	}
	return nil
}

// accessSupersededReference 生成有界的证据引用；完整摘要保存在追加式审计表。
// 参数：receipt为原回执清单摘要，current为当前审核清单摘要。
// 返回值：不超过既有 evidence_ref/result_ref 列宽的可读引用。
func accessSupersededReference(receipt, current string) string {
	return "superseded:" + accessChecksumPrefix(receipt, 48) + "->" + accessChecksumPrefix(current, 48)
}

// accessChecksumPrefix 截断超长摘要，避免溢出既有列宽。
// 参数：value为摘要，limit为保留前缀长度。
// 返回值：不超过 limit 的前缀。
func accessChecksumPrefix(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
