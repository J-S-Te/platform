package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// workerRepositoryStub 实现操作仓储契约（内存版），带最小租约语义。
type workerRepositoryStub struct {
	operation SubsystemAccessOperation
	stages    []SubsystemAccessOperationStage
	claimable bool
	leaseHeld bool
}

func (stub *workerRepositoryStub) CreateOperation(context.Context, SubsystemAccessOperationCreate) (SubsystemAccessOperation, bool, error) {
	return stub.operation, false, nil
}

func (stub *workerRepositoryStub) GetOperation(context.Context, string, string) (SubsystemAccessOperation, error) {
	return stub.operation, nil
}

func (stub *workerRepositoryStub) FindOperationByTarget(context.Context, string, string, string) (SubsystemAccessOperation, bool, error) {
	return stub.operation, true, nil
}

func (stub *workerRepositoryStub) ListOperationsByTarget(context.Context, string, string, string, int) ([]SubsystemAccessOperation, error) {
	return []SubsystemAccessOperation{stub.operation}, nil
}

func (stub *workerRepositoryStub) ListClaimableOperations(context.Context, int, time.Time) ([]SubsystemAccessOperation, error) {
	if !stub.claimable {
		return nil, nil
	}
	return []SubsystemAccessOperation{stub.operation}, nil
}

func (stub *workerRepositoryStub) ClaimOperation(_ context.Context, _, id, workerID string, _ time.Duration, _ time.Time) (SubsystemAccessOperation, bool, error) {
	if !stub.claimable || id != stub.operation.ID {
		return SubsystemAccessOperation{}, false, nil
	}
	stub.operation.Status = SubsystemAccessStatusRunning
	stub.operation.LeaseOwner = workerID
	stub.operation.AttemptCount++
	stub.claimable = false
	stub.leaseHeld = true
	return stub.operation, true, nil
}

func (stub *workerRepositoryStub) RenewOperationLease(_ context.Context, _, _, workerID string, _ time.Duration, _ time.Time) (bool, error) {
	return stub.leaseHeld && stub.operation.LeaseOwner == workerID, nil
}

func (stub *workerRepositoryStub) AdvanceOperationStage(_ context.Context, _, _, workerID string, stage SubsystemAccessStage, _ time.Time) (bool, error) {
	if !stub.leaseHeld || stub.operation.LeaseOwner != workerID {
		return false, nil
	}
	stub.operation.Stage = stage
	return true, nil
}

func (stub *workerRepositoryStub) CompleteOperation(_ context.Context, completion SubsystemAccessOperationCompletion) (bool, error) {
	if !stub.leaseHeld || stub.operation.LeaseOwner != completion.LeaseOwner {
		return false, nil
	}
	stub.operation.Status = completion.Status
	stub.operation.Stage = completion.Stage
	stub.operation.ErrorCode = completion.ErrorCode
	stub.operation.ResultRef = completion.ResultRef
	stub.operation.LeaseOwner = ""
	stub.leaseHeld = false
	return true, nil
}

func (stub *workerRepositoryStub) RequeueOperation(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}

func (stub *workerRepositoryStub) UpsertOperationStage(_ context.Context, _, _ string, stage SubsystemAccessStage, status SubsystemAccessStatus, errorCode, _, evidenceRef string, _ time.Time) error {
	stub.stages = append(stub.stages, SubsystemAccessOperationStage{Stage: stage, Status: status, ErrorCode: errorCode, EvidenceRef: evidenceRef})
	return nil
}

func (stub *workerRepositoryStub) ListOperationStages(context.Context, string, string) ([]SubsystemAccessOperationStage, error) {
	return stub.stages, nil
}

// scriptedExecutor 按阶段返回预设结果，并记录调用顺序。
type scriptedExecutor struct {
	outcomes map[SubsystemAccessStage]SubsystemAccessStageOutcome
	errors   map[SubsystemAccessStage]error
	calls    []SubsystemAccessStage
}

func (executor *scriptedExecutor) ExecuteSubsystemAccessStage(_ context.Context, _ SubsystemAccessOperation, stage SubsystemAccessStage, _ *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error) {
	executor.calls = append(executor.calls, stage)
	if err := executor.errors[stage]; err != nil {
		return SubsystemAccessStageOutcome{}, err
	}
	if outcome, ok := executor.outcomes[stage]; ok {
		return outcome, nil
	}
	return SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "evidence:" + string(stage)}, nil
}

func workerFor(t *testing.T, repository *workerRepositoryStub, executor SubsystemAccessStageExecutor) *SubsystemAccessWorker {
	t.Helper()
	worker, err := NewSubsystemAccessWorker(repository, executor, accessClockStub{}, "worker-1", time.Minute, 6, 2)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func operationFixture() SubsystemAccessOperation {
	return SubsystemAccessOperation{ID: "01SAOP000000000000000000001", TenantID: "tenant-1",
		ApplicationCode: "settlement", EnvironmentCode: "dev", Action: SubsystemAccessActionOnboard,
		Stage: SubsystemAccessStageCheck, Status: SubsystemAccessStatusPending, AttemptCount: 0}
}

// 正常路径：六个阶段依次执行，最后一步成功后操作成功并释放槽位。
func TestSubsystemAccessWorkerDrivesAllStages(t *testing.T) {
	repository := &workerRepositoryStub{operation: operationFixture(), claimable: true}
	executor := &scriptedExecutor{}
	worker := workerFor(t, repository, executor)

	handled, err := worker.RunOnce(context.Background(), "tenant-1", repository.operation.ID)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if repository.operation.Status != SubsystemAccessStatusSucceeded {
		t.Fatalf("status = %s, want SUCCEEDED", repository.operation.Status)
	}
	if len(executor.calls) != 6 {
		t.Fatalf("executor calls = %v, want all six stages", executor.calls)
	}
	for index, stage := range SubsystemAccessStageSequence() {
		if executor.calls[index] != stage {
			t.Fatalf("call[%d] = %s, want %s", index, executor.calls[index], stage)
		}
	}
}

// 等待处理：AUTH 缺批准 → 操作转 WAITING，后续阶段不执行。
func TestSubsystemAccessWorkerWaitsForMissingApproval(t *testing.T) {
	repository := &workerRepositoryStub{operation: operationFixture(), claimable: true}
	executor := &scriptedExecutor{outcomes: map[SubsystemAccessStage]SubsystemAccessStageOutcome{
		SubsystemAccessStageAuth: {Status: SubsystemAccessStatusWaiting, ErrorCode: "RUNTIME_APPROVAL_MISSING", Message: "缺少运行批准"}}}
	worker := workerFor(t, repository, executor)

	if _, err := worker.RunOnce(context.Background(), "tenant-1", repository.operation.ID); err != nil {
		t.Fatal(err)
	}
	if repository.operation.Status != SubsystemAccessStatusWaiting || repository.operation.ErrorCode != "RUNTIME_APPROVAL_MISSING" {
		t.Fatalf("operation = %+v", repository.operation)
	}
	if len(executor.calls) != 3 {
		t.Fatalf("executor calls = %v, want stop at AUTH", executor.calls)
	}
}

// 失败：DEPLOY 返回失败 → 操作 FAILED（终态，释放槽位）。
func TestSubsystemAccessWorkerFailsTerminally(t *testing.T) {
	repository := &workerRepositoryStub{operation: operationFixture(), claimable: true}
	executor := &scriptedExecutor{outcomes: map[SubsystemAccessStage]SubsystemAccessStageOutcome{
		SubsystemAccessStageDeploy: {Status: SubsystemAccessStatusFailed, ErrorCode: "IMAGE_MISSING", Message: "缺少镜像"}}}
	worker := workerFor(t, repository, executor)

	if _, err := worker.RunOnce(context.Background(), "tenant-1", repository.operation.ID); err != nil {
		t.Fatal(err)
	}
	if repository.operation.Status != SubsystemAccessStatusFailed || repository.operation.ErrorCode != "IMAGE_MISSING" {
		t.Fatalf("operation = %+v", repository.operation)
	}
}

// 结果未知：不重复执行；达到尝试上限才判定失败。
func TestSubsystemAccessWorkerDoesNotRepeatUnknownStage(t *testing.T) {
	repository := &workerRepositoryStub{operation: operationFixture(), claimable: true}
	executor := &scriptedExecutor{errors: map[SubsystemAccessStage]error{SubsystemAccessStageCheck: errors.New("agent timeout")}}
	worker := workerFor(t, repository, executor)

	if _, err := worker.RunOnce(context.Background(), "tenant-1", repository.operation.ID); err != nil {
		t.Fatalf("unknown result must not fail the operation: %v", err)
	}
	if repository.operation.Status != SubsystemAccessStatusRunning || len(executor.calls) != 1 {
		t.Fatalf("operation=%s calls=%v", repository.operation.Status, executor.calls)
	}
	if len(repository.stages) != 1 || repository.stages[0].Status != SubsystemAccessStatusRunning || repository.stages[0].ErrorCode != "STAGE_EXECUTION_UNKNOWN" {
		t.Fatalf("stages = %+v", repository.stages)
	}

	// 第二次领取：尝试次数达到上限（maxAttempts=2）→ 失败。
	repository.claimable = true
	executor.calls = nil
	if _, err := worker.RunOnce(context.Background(), "tenant-1", repository.operation.ID); err == nil {
		t.Fatal("exceeding max attempts must fail the operation")
	}
	if repository.operation.Status != SubsystemAccessStatusFailed || repository.operation.ErrorCode != "STAGE_EXECUTION_FAILED" {
		t.Fatalf("operation = %+v", repository.operation)
	}
}

// TestSubsystemAccessWorkerStopsWhenLeaseLost 用途：验证旧领取快照不能续租或覆盖新执行者。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSubsystemAccessWorkerStopsWhenLeaseLost(t *testing.T) {
	repository := &workerRepositoryStub{operation: operationFixture(), claimable: true}
	executor := &scriptedExecutor{}
	worker := workerFor(t, repository, executor)
	worker.maxStages = 1

	if _, err := worker.RunOnce(context.Background(), "tenant-1", repository.operation.ID); err != nil {
		t.Fatal(err)
	}
	original := repository.operation
	// 模拟另一次执行接管：旧执行者仍持有接管前快照。
	repository.leaseHeld = true
	repository.operation.LeaseOwner = "worker-2"
	repository.operation.Status = SubsystemAccessStatusRunning
	err := worker.drive(context.Background(), original)
	if !errors.Is(err, ErrSubsystemAccessOperationLeaseLost) {
		t.Fatalf("err = %v, want lease lost", err)
	}
}

// 未接线/请求不合法属于"已知不可重试"：立即失败并给出稳定错误码，不进入"结果未知"重试循环。
func TestSubsystemAccessWorkerFailsImmediatelyOnNotWiredStage(t *testing.T) {
	repository := &workerRepositoryStub{operation: operationFixture(), claimable: true}
	executor := &scriptedExecutor{errors: map[SubsystemAccessStage]error{
		SubsystemAccessStageCheck: fmt.Errorf("update path is not wired yet: %w", ErrSubsystemAccessStageNotWired)}}
	worker := workerFor(t, repository, executor)

	if _, err := worker.RunOnce(context.Background(), "tenant-1", repository.operation.ID); err != nil {
		t.Fatalf("not-wired stage must not return an error to the scheduler: %v", err)
	}
	if repository.operation.Status != SubsystemAccessStatusFailed || repository.operation.ErrorCode != "STAGE_NOT_WIRED" {
		t.Fatalf("operation = %+v", repository.operation)
	}
	if repository.operation.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1（不可重试不应再次领取）", repository.operation.AttemptCount)
	}
	if len(repository.stages) != 1 || repository.stages[0].Status != SubsystemAccessStatusFailed || repository.stages[0].ErrorCode != "STAGE_NOT_WIRED" {
		t.Fatalf("stages = %+v", repository.stages)
	}
}
