package infrastructure

import (
	"context"
	"errors"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SubsystemAccessDeploymentLifecycle 将外部派发与持久领取代次关联，不保存凭据。
type SubsystemAccessDeploymentLifecycle interface {
	PrepareAccessDeployment(context.Context, application.SubsystemAccessOperation) (string, error)
	DispatchAccessDeployment(context.Context, application.SubsystemAccessOperation, string, string) error
	CompleteAccessDeployment(context.Context, application.SubsystemAccessOperation, string) error
}

type subsystemAccessDeploymentAttempt struct {
	TenantID                   string    `gorm:"column:tenant_id;primaryKey"`
	OperationID                string    `gorm:"column:operation_id;primaryKey"`
	ApplicationID              string    `gorm:"column:application_id"`
	EnvironmentID              string    `gorm:"column:environment_id"`
	Generation                 uint64    `gorm:"column:generation"`
	Phase                      string    `gorm:"column:phase"`
	ManifestChecksum           string    `gorm:"column:manifest_checksum"`
	InitialRuntimeProvisioning bool      `gorm:"column:initial_runtime_provisioning"`
	RuntimeExpectations        []byte    `gorm:"column:runtime_expectations"`
	CandidateSnapshot          string    `gorm:"column:candidate_snapshot"`
	CreatedAt                  time.Time `gorm:"column:created_at"`
	UpdatedAt                  time.Time `gorm:"column:updated_at"`
}

// TableName 用途：指定代次绑定表。
// 参数：无。
// 返回值：返回固定表名。
func (subsystemAccessDeploymentAttempt) TableName() string {
	return "subsystem_access_deployment_attempt"
}

// lockAccessOperation 用途：锁定并核验当前任务、租约、领取轮次及完整目标。
// 参数：ctx 为上下文，tx 为事务，operation 为执行者持有的任务，now 为当前时间。
// 返回值：返回持久任务；失去所有权或目标不符时返回错误。
func lockAccessOperation(ctx context.Context, tx *gorm.DB, operation application.SubsystemAccessOperation, now time.Time) (subsystemAccessOperationModel, error) {
	var row subsystemAccessOperationModel
	err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND id = ?", operation.TenantID, operation.ID).Take(&row).Error
	if err != nil {
		return row, err
	}
	if row.Status != string(application.SubsystemAccessStatusRunning) || row.LeaseOwner == nil ||
		*row.LeaseOwner != operation.LeaseOwner || operation.LeaseOwner == "" ||
		row.AttemptCount != operation.AttemptCount || row.LeaseExpiresAt == nil || !row.LeaseExpiresAt.After(now) {
		return row, application.ErrSubsystemAccessOperationLeaseLost
	}
	if row.ApplicationCode != operation.ApplicationCode || row.EnvironmentCode != operation.EnvironmentCode ||
		row.Action != string(operation.Action) || row.OperatorID != operation.OperatorID ||
		(operation.ApplicationID != "" && accessDerefString(row.ApplicationID) != operation.ApplicationID) ||
		(operation.EnvironmentID != "" && accessDerefString(row.EnvironmentID) != operation.EnvironmentID) {
		return row, application.ErrSubsystemAccessOperationConflict
	}
	return row, nil
}

// PrepareAccessDeployment 用途：原子领取接管/更新代次；首次接入必须沿用认证事务的绑定。
// 参数：ctx 为上下文，operation 为持有有效租约的向导任务。
// 返回值：返回持久派发阶段；旧任务、历史歧义或代次冲突返回错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) PrepareAccessDeployment(ctx context.Context, operation application.SubsystemAccessOperation) (string, error) {
	phase := ""
	err := reader.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockAccessOperation(ctx, tx, operation, time.Now().UTC()); err != nil {
			return err
		}
		var attempt subsystemAccessDeploymentAttempt
		err := tx.Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Take(&attempt).Error
		repo := &SubsystemOnboardingGORMRepository{database: tx}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if operation.Action == application.SubsystemAccessActionOnboard {
				return application.ErrSubsystemAccessOperationConflict
			}
			var priorRow subsystemDeploymentStateModel
			priorErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_code = ? AND environment_code = ?", operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode).Take(&priorRow).Error
			if errors.Is(priorErr, gorm.ErrRecordNotFound) {
				priorErr = application.ErrNotFound
			}
			prior := deploymentStateFromModel(priorRow)
			if priorErr != nil && !errors.Is(priorErr, application.ErrNotFound) {
				return priorErr
			}
			initial := (operation.Action == application.SubsystemAccessActionAdopt ||
				(operation.Action == application.SubsystemAccessActionRetry && prior.Status == application.SubsystemDeploymentStatusFailed)) &&
				prior.AppliedManifestChecksum == "" && prior.ManifestLastAppliedAt == nil
			generation, err := repo.ClaimSubsystemDeployment(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, string(operation.Action), time.Now().UTC())
			if err != nil {
				return err
			}
			state, err := repo.GetSubsystemDeploymentState(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
			if err != nil {
				return err
			}
			if state.ApplicationID != operation.ApplicationID || state.EnvironmentID != operation.EnvironmentID {
				return application.ErrSubsystemAccessOperationConflict
			}
			attempt = subsystemAccessDeploymentAttempt{TenantID: operation.TenantID, OperationID: operation.ID,
				ApplicationID: state.ApplicationID, EnvironmentID: state.EnvironmentID, Generation: generation, InitialRuntimeProvisioning: initial, Phase: "PREPARED", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			if err := tx.Create(&attempt).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := verifyAccessAttempt(ctx, tx, operation, attempt); err != nil {
			return err
		}
		if attempt.Phase == "PREPARED" {
			var current subsystemDeploymentStateModel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_code = ? AND environment_code = ?", operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode).Take(&current).Error; err != nil {
				return err
			}
			if current.Status == application.SubsystemDeploymentStatusFailed {
				attempt.Phase = "FAILED"
			}
		}
		if attempt.Phase == "FAILED" || attempt.Phase == "RETRY_REQUESTED" {
			generation, err := repo.ClaimSubsystemDeployment(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, string(operation.Action), time.Now().UTC())
			if err != nil {
				return err
			}
			if err := tx.Model(&attempt).Updates(map[string]any{"generation": generation, "phase": "PREPARED", "manifest_checksum": "", "candidate_snapshot": "", "runtime_expectations": nil, "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
			attempt.Phase = "PREPARED"
		}
		phase = attempt.Phase
		return nil
	})
	return phase, err
}

// verifyAccessAttempt 用途：确认绑定未被其他生命周期取代。
// 参数：ctx 为上下文，tx 为事务，operation 为任务，attempt 为已绑定代次。
// 返回值：身份或代次不一致时返回冲突。
func verifyAccessAttempt(ctx context.Context, tx *gorm.DB, operation application.SubsystemAccessOperation, attempt subsystemAccessDeploymentAttempt) error {
	var state subsystemDeploymentStateModel
	if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND application_code = ? AND environment_code = ?", operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode).Take(&state).Error; err != nil {
		return err
	}
	if state.Generation != attempt.Generation || state.ApplicationID != attempt.ApplicationID || state.EnvironmentID != attempt.EnvironmentID ||
		(operation.ApplicationID != "" && operation.ApplicationID != attempt.ApplicationID) ||
		(operation.EnvironmentID != "" && operation.EnvironmentID != attempt.EnvironmentID) {
		return application.ErrSubsystemDeploymentTransition
	}
	if attempt.Phase == "RETRY_REQUESTED" {
		if state.Status != application.SubsystemDeploymentStatusReady && state.Status != application.SubsystemDeploymentStatusFailed {
			return application.ErrSubsystemDeploymentTransition
		}
	} else if attempt.Phase == "PREPARED" && state.Status == application.SubsystemDeploymentStatusFailed {
		// 未派发且原代次已由看护收口，可经原任务显式恢复领取新代次。
		return nil
	} else if attempt.Phase == "FAILED" {
		if state.Status != application.SubsystemDeploymentStatusFailed {
			return application.ErrSubsystemDeploymentTransition
		}
	} else if attempt.Phase == "SUCCEEDED" {
		if state.Status != application.SubsystemDeploymentStatusReady {
			return application.ErrSubsystemDeploymentTransition
		}
	} else if state.Status != application.SubsystemDeploymentStatusProvisioning && state.Status != application.SubsystemDeploymentStatusUpdating {
		return application.ErrSubsystemDeploymentTransition
	}
	return nil
}

// DispatchAccessDeployment 用途：在外部副作用前持久记录派发意图，重复派发被拒绝。
// 参数：ctx 为上下文，operation 为任务，checksum 为清单摘要，candidate 为配置指纹。
// 返回值：失租、代次漂移或重复派发返回错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) DispatchAccessDeployment(ctx context.Context, operation application.SubsystemAccessOperation, checksum, candidate string) error {
	return reader.changeAccessDeployment(ctx, operation, checksum, candidate, false)
}

// CompleteAccessDeployment 用途：在同一事务内写入实际清单、READY 和成功回执。
// 参数：ctx 为上下文，operation 为任务，checksum 为同步 Agent 成功的清单摘要。
// 返回值：原代次或当前租约不再有效时拒绝收口。
func (reader *SubsystemAccessDeploymentStateGORMReader) CompleteAccessDeployment(ctx context.Context, operation application.SubsystemAccessOperation, checksum string) error {
	return reader.changeAccessDeployment(ctx, operation, checksum, "", true)
}

// changeAccessDeployment 用途：对派发意图或成功回执执行受租约保护的原子变更。
// 参数：ctx 为上下文，operation 为任务，checksum 为清单摘要，candidate 为配置指纹，complete 表示成功收口。
// 返回值：返回事务错误，不隐式重试外部部署。
func (reader *SubsystemAccessDeploymentStateGORMReader) changeAccessDeployment(ctx context.Context, operation application.SubsystemAccessOperation, checksum, candidate string, complete bool) error {
	return reader.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if _, err := lockAccessOperation(ctx, tx, operation, now); err != nil {
			return err
		}
		var attempt subsystemAccessDeploymentAttempt
		if err := tx.Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
			return err
		}
		if err := verifyAccessAttempt(ctx, tx, operation, attempt); err != nil {
			return err
		}
		phase := "DISPATCHED"
		if !complete && attempt.Phase != "PREPARED" {
			return application.ErrSubsystemAccessOperationConflict
		}
		if complete {
			if attempt.Phase != "DISPATCHED" || attempt.ManifestChecksum != checksum {
				return application.ErrSubsystemAccessOperationConflict
			}
			repo := &SubsystemOnboardingGORMRepository{database: tx}
			if err := repo.SetSubsystemDesiredManifest(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, checksum, now); err != nil {
				return err
			}
			if err := repo.MarkSubsystemManifestApplied(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, checksum, now); err != nil {
				return err
			}
			if err := repo.CompleteSubsystemDeployment(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, attempt.Generation, application.SubsystemDeploymentStatusReady, string(operation.Action), "", "", now); err != nil {
				return err
			}
			phase = "SUCCEEDED"
		}
		updates := map[string]any{"phase": phase, "manifest_checksum": checksum, "updated_at": now}
		if !complete {
			updates["candidate_snapshot"] = candidate
		}
		return tx.Model(&attempt).Updates(updates).Error
	})
}

// bindInitialAccessDeployment 用途：在初次登记事务中持久绑定原代次和权威目标。
// 参数：ctx 为上下文，tx 为事务，operation 为任务，appID/envID 为实际登记身份，now 为时间。
// 返回值：绑定或续租核验失败时返回错误，调用方回滚整个登记。
func bindInitialAccessDeployment(ctx context.Context, tx *gorm.DB, operation application.SubsystemAccessOperation, appID, envID string, now time.Time) error {
	if _, err := lockAccessOperation(ctx, tx, operation, time.Now().UTC()); err != nil {
		return err
	}
	attempt := subsystemAccessDeploymentAttempt{TenantID: operation.TenantID, OperationID: operation.ID,
		ApplicationID: appID, EnvironmentID: envID, Generation: 1, Phase: "PREPARED", CreatedAt: now, UpdatedAt: now}
	if err := tx.Create(&attempt).Error; err != nil {
		return err
	}
	return tx.Model(&subsystemAccessOperationModel{}).Where("tenant_id = ? AND id = ?", operation.TenantID, operation.ID).
		Updates(map[string]any{"application_id": appID, "environment_id": envID}).Error
}

// ReadAccessDeploymentBinding 用途：恢复 AUTH 时先定位原事务绑定，避免重复创建客户端。
// 参数：ctx 为上下文，operation 为当前租约任务。
// 返回值：返回是否存在绑定；失租或读取失败返回错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) ReadAccessDeploymentBinding(ctx context.Context, operation application.SubsystemAccessOperation) (bool, error) {
	found := false
	err := reader.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockAccessOperation(ctx, tx, operation, time.Now().UTC()); err != nil {
			return err
		}
		var count int64
		if err := tx.Model(&subsystemAccessDeploymentAttempt{}).Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Count(&count).Error; err != nil {
			return err
		}
		found = count == 1
		return nil
	})
	return found, err
}

// ReadAccessDeploymentReceipt 用途：读取当前任务原代次和清单的非秘密回执绑定。
// 参数：ctx 为上下文，operation 为当前租约任务。
// 返回值：返回回执目标及派发时配置摘要；读取失败返回错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) ReadAccessDeploymentReceipt(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemAccessDeploymentReceipt, string, error) {
	var attempt subsystemAccessDeploymentAttempt
	err := reader.database.WithContext(ctx).Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Take(&attempt).Error
	return application.SubsystemAccessDeploymentReceipt{TenantID: operation.TenantID, OperationID: operation.ID,
		ApplicationCode: operation.ApplicationCode, Environment: operation.EnvironmentCode, Generation: attempt.Generation, ManifestChecksum: attempt.ManifestChecksum}, attempt.CandidateSnapshot, err
}

// ReadAccessInitialRuntimeProvisioning 用途：恢复领取前的首次部署判断，不从已改变的状态反推。
// 参数：ctx 为上下文，operation 为任务。
// 返回值：返回持久属性或读取错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) ReadAccessInitialRuntimeProvisioning(ctx context.Context, operation application.SubsystemAccessOperation) (bool, error) {
	var attempt subsystemAccessDeploymentAttempt
	err := reader.database.WithContext(ctx).Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Take(&attempt).Error
	return attempt.InitialRuntimeProvisioning, err
}

// FailAccessDeployment 用途：仅收口已知未派发的失败，不把未知外部结果判为失败。
// 参数：ctx 为上下文，operation 为任务，code/message 为非秘密错误摘要。
// 返回值：租约、代次或事务失败返回错误。
func (reader *SubsystemAccessDeploymentStateGORMReader) FailAccessDeployment(ctx context.Context, operation application.SubsystemAccessOperation, code, message string) error {
	return reader.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockAccessOperation(ctx, tx, operation, time.Now().UTC()); err != nil {
			return err
		}
		var attempt subsystemAccessDeploymentAttempt
		if err := tx.Where("tenant_id = ? AND operation_id = ?", operation.TenantID, operation.ID).Take(&attempt).Error; err != nil {
			return err
		}
		if attempt.Phase != "PREPARED" && attempt.Phase != "FAILED" {
			return application.ErrSubsystemAccessOperationConflict
		}
		if err := verifyAccessAttempt(ctx, tx, operation, attempt); err != nil {
			return err
		}
		if attempt.Phase == "FAILED" {
			return nil
		}
		repo := &SubsystemOnboardingGORMRepository{database: tx}
		if err := repo.CompleteSubsystemDeployment(ctx, operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode, attempt.Generation, application.SubsystemDeploymentStatusFailed, string(operation.Action), code, message, time.Now().UTC()); err != nil {
			return err
		}
		return tx.Model(&attempt).Updates(map[string]any{"phase": "FAILED", "updated_at": time.Now().UTC()}).Error
	})
}

// UpsertClaimedOperationStage 用途：在任务锁内验证领取轮次后写入阶段，避免迟到结果污染新任务。
// 参数：ctx 为上下文，operation 为原领取快照，stage/status 为结果，code/message/ref 为说明，now 为结果时间。
// 返回值：返回租约或写入错误。
func (repository *SubsystemAccessOperationGORMRepository) UpsertClaimedOperationStage(ctx context.Context, operation application.SubsystemAccessOperation, stage application.SubsystemAccessStage, status application.SubsystemAccessStatus, code, message, ref string, now time.Time) error {
	return repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockAccessOperation(ctx, tx, operation, now); err != nil {
			return err
		}
		return (&SubsystemAccessOperationGORMRepository{database: tx}).UpsertOperationStage(ctx, operation.TenantID, operation.ID, stage, status, code, message, ref, now)
	})
}
