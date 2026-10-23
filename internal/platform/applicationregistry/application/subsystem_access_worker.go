package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 持久执行器：由平台 Worker 周期性领取操作并逐阶段推进。
// 语义要点（对应方案 §2.4）：
//   - 领取/续租/收口全部条件更新，过期执行者不能覆盖新结果；
//   - 阶段执行结果未知（executor 返回错误或"进行中"）时**不重复执行**，保留租约交由下一轮核对；
//   - 超过尝试上限才判定失败；"等待处理"（缺批准/许可证/观察期）保留槽位等待用户 resume。

// SubsystemAccessStageOutcome 是单阶段执行结果。
type SubsystemAccessStageOutcome struct {
	// Status 取 SUCCEEDED / WAITING / FAILED；其余值视为"仍在进行中"。
	Status      SubsystemAccessStatus
	ErrorCode   string
	Message     string
	EvidenceRef string
}

// SubsystemAccessStageExecutor 由既有目录/认证/凭据/部署服务实现。
// 实现必须在"结果未知"时先核对实际效果（不重复迁移、不重复创建 Client、不重复分配角色）。
type SubsystemAccessStageExecutor interface {
	ExecuteSubsystemAccessStage(ctx context.Context, operation SubsystemAccessOperation, stage SubsystemAccessStage, run *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error)
}

// SubsystemAccessWorker 领取并推进操作。
type SubsystemAccessWorker struct {
	repository  SubsystemAccessOperationRepository
	executor    SubsystemAccessStageExecutor
	clock       Clock
	workerID    string
	leaseTTL    time.Duration
	maxStages   int
	maxAttempts uint32
}

// NewSubsystemAccessWorker 构造执行器；依赖或标识缺失即失败。
func NewSubsystemAccessWorker(repository SubsystemAccessOperationRepository, executor SubsystemAccessStageExecutor, clock Clock, workerID string, leaseTTL time.Duration, maxStages int, maxAttempts uint32) (*SubsystemAccessWorker, error) {
	if repository == nil || executor == nil || clock == nil {
		return nil, errors.New("subsystem access worker dependencies must not be nil")
	}
	if strings.TrimSpace(workerID) == "" {
		return nil, errors.New("subsystem access worker requires an identity")
	}
	if leaseTTL <= 0 {
		leaseTTL = 2 * time.Minute
	}
	if maxStages <= 0 || maxStages > len(SubsystemAccessStageSequence()) {
		maxStages = len(SubsystemAccessStageSequence())
	}
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	return &SubsystemAccessWorker{repository: repository, executor: executor, clock: clock,
		workerID: strings.TrimSpace(workerID), leaseTTL: leaseTTL, maxStages: maxStages, maxAttempts: maxAttempts}, nil
}

// RunOnce 领取并推进一个操作；handled=false 表示该操作当前不可领取（他人在执行或已终态）。
func (worker *SubsystemAccessWorker) RunOnce(ctx context.Context, tenantID, operationID string) (bool, error) {
	operation, claimed, err := worker.repository.ClaimOperation(ctx, tenantID, operationID, worker.workerID, worker.leaseTTL, worker.clock.Now())
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	return true, worker.drive(ctx, operation)
}

// DriveClaimable 领取并推进当前可领取的操作（有界），返回处理数量。
func (worker *SubsystemAccessWorker) DriveClaimable(ctx context.Context, limit int) (int, error) {
	operations, err := worker.repository.ListClaimableOperations(ctx, limit, worker.clock.Now())
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, operation := range operations {
		ok, err := worker.RunOnce(ctx, operation.TenantID, operation.ID)
		if err != nil {
			return handled, err
		}
		if ok {
			handled++
		}
	}
	return handled, nil
}

func (worker *SubsystemAccessWorker) drive(ctx context.Context, operation SubsystemAccessOperation) error {
	// 运行内共享状态：目录/凭据阶段的产物只在本次执行内交接，不落库。
	run := &SubsystemAccessRunState{}
	stage := operation.Stage
	if !ValidSubsystemAccessStage(string(stage)) {
		stage = SubsystemAccessStageCheck
	}
	for step := 0; step < worker.maxStages; step++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := worker.clock.Now().UTC()
		// 阶段执行前续租：租约丢失说明已被接管，立即停止（不覆盖新结果）。
		renewed, err := worker.repository.RenewOperationLease(ctx, operation.TenantID, operation.ID, worker.workerID, worker.leaseTTL, now)
		if err != nil {
			return err
		}
		if !renewed {
			return ErrSubsystemAccessOperationLeaseLost
		}
		outcome, executeErr := worker.executor.ExecuteSubsystemAccessStage(ctx, operation, stage, run)
		if executeErr != nil {
			return worker.recordUnknown(ctx, operation, stage, executeErr)
		}
		switch outcome.Status {
		case SubsystemAccessStatusSucceeded:
			if err := worker.repository.UpsertOperationStage(ctx, operation.TenantID, operation.ID, stage,
				SubsystemAccessStatusSucceeded, "", "", outcome.EvidenceRef, now); err != nil {
				return err
			}
			next, hasNext := NextSubsystemAccessStage(stage)
			if !hasNext {
				_, err := worker.repository.CompleteOperation(ctx, SubsystemAccessOperationCompletion{
					TenantID: operation.TenantID, ID: operation.ID, LeaseOwner: worker.workerID,
					Status: SubsystemAccessStatusSucceeded, Stage: stage, ResultRef: outcome.EvidenceRef, Now: now})
				return err
			}
			advanced, err := worker.repository.AdvanceOperationStage(ctx, operation.TenantID, operation.ID, worker.workerID, next, now)
			if err != nil {
				return err
			}
			if !advanced {
				return ErrSubsystemAccessOperationLeaseLost
			}
			operation.Stage = next
			stage = next
		case SubsystemAccessStatusWaiting, SubsystemAccessStatusFailed:
			if err := worker.repository.UpsertOperationStage(ctx, operation.TenantID, operation.ID, stage,
				outcome.Status, outcome.ErrorCode, outcome.Message, outcome.EvidenceRef, now); err != nil {
				return err
			}
			_, err := worker.repository.CompleteOperation(ctx, SubsystemAccessOperationCompletion{
				TenantID: operation.TenantID, ID: operation.ID, LeaseOwner: worker.workerID, Status: outcome.Status,
				Stage: stage, ErrorCode: outcome.ErrorCode, ErrorMessage: outcome.Message, ResultRef: outcome.EvidenceRef, Now: now})
			return err
		default:
			// 仍在进行中（例如已提交但结果未知）：记录状态后保留租约，交由下一轮核对。
			return worker.repository.UpsertOperationStage(ctx, operation.TenantID, operation.ID, stage,
				SubsystemAccessStatusRunning, outcome.ErrorCode, outcome.Message, outcome.EvidenceRef, now)
		}
	}
	// 达到单次可推进阶段上限：保留租约，下一轮继续。
	return nil
}

// recordUnknown 处理"阶段执行结果未知"：不重复执行；超过尝试上限才判定失败。
func (worker *SubsystemAccessWorker) recordUnknown(ctx context.Context, operation SubsystemAccessOperation, stage SubsystemAccessStage, executeErr error) error {
	now := worker.clock.Now().UTC()
	message := executeErr.Error()
	// 已知不可重试（未接线、请求不合法）：立即判定失败，不做无意义重试。
	if errors.Is(executeErr, ErrSubsystemAccessStageNotWired) || errors.Is(executeErr, ErrSubsystemAccessInvalidRequest) {
		if err := worker.repository.UpsertOperationStage(ctx, operation.TenantID, operation.ID, stage,
			SubsystemAccessStatusFailed, "STAGE_NOT_WIRED", message, "", now); err != nil {
			return err
		}
		_, err := worker.repository.CompleteOperation(ctx, SubsystemAccessOperationCompletion{
			TenantID: operation.TenantID, ID: operation.ID, LeaseOwner: worker.workerID,
			Status: SubsystemAccessStatusFailed, Stage: stage, ErrorCode: "STAGE_NOT_WIRED", ErrorMessage: message, Now: now})
		return err
	}
	if err := worker.repository.UpsertOperationStage(ctx, operation.TenantID, operation.ID, stage,
		SubsystemAccessStatusRunning, "STAGE_EXECUTION_UNKNOWN", message, "", now); err != nil {
		return err
	}
	if operation.AttemptCount >= worker.maxAttempts {
		_, err := worker.repository.CompleteOperation(ctx, SubsystemAccessOperationCompletion{
			TenantID: operation.TenantID, ID: operation.ID, LeaseOwner: worker.workerID,
			Status: SubsystemAccessStatusFailed, Stage: stage, ErrorCode: "STAGE_EXECUTION_FAILED", ErrorMessage: message, Now: now})
		if err != nil {
			return err
		}
		return fmt.Errorf("subsystem access operation %s failed at %s: %w", operation.ID, stage, executeErr)
	}
	return nil
}
