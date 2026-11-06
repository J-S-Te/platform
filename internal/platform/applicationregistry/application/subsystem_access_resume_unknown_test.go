package application

import (
	"context"
	"errors"
	"testing"
)

// unknownAccessDeploymentOperation 构造一次派发后结果未知的等待操作。
// 参数：无，取固定租户与目标，便于同一测试内比较版本与领取轮次。
// 返回值：尚未进入仓储的操作实体。
func unknownAccessDeploymentOperation() SubsystemAccessOperation {
	return SubsystemAccessOperation{
		ID: "01SAOP000000000000000000031", TenantID: "tenant-1",
		ApplicationCode: "contract", EnvironmentCode: "dev",
		Action: SubsystemAccessActionUpdate, Stage: SubsystemAccessStageDeploy,
		Status: SubsystemAccessStatusWaiting, ErrorCode: AccessDeploymentEffectUnknown,
		AttemptCount: 3, Version: 7,
	}
}

// TestSubsystemAccessResumeRejectsUnknownDeploymentEffect 验证派发效果未知时恢复只拒绝、不重排。
// 参数：t为应用服务及仓储替身上下文。
// 返回值：无，attempt_count 与 version 必须保持原值，HTTP 才能据此给出人工核验提示。
func TestSubsystemAccessResumeRejectsUnknownDeploymentEffect(t *testing.T) {
	facts := &accessFactsStub{version: "pv-1", facts: SubsystemAccessFacts{InScope: true, Registered: true,
		ControlledDeployment: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true}}
	for _, status := range []SubsystemAccessStatus{SubsystemAccessStatusWaiting, SubsystemAccessStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			repository := newAccessRepositoryStub()
			service := accessServiceFor(t, facts, repository)
			operation := unknownAccessDeploymentOperation()
			operation.Status = status
			repository.operations[operation.ID] = operation

			view, err := service.Resume(context.Background(), operation.TenantID, operation.ID)
			if !errors.Is(err, ErrSubsystemAccessRecoveryRequired) {
				t.Fatalf("unknown effect must require manual recovery, got %v", err)
			}
			// 必须与普通冲突区分：HTTP 层只能对专用错误给出 next_action。
			if errors.Is(err, ErrSubsystemAccessOperationConflict) {
				t.Fatal("unknown effect must not collapse into the generic conflict class")
			}
			if repository.requeued != 0 {
				t.Fatalf("unknown effect was requeued %d times", repository.requeued)
			}
			if view.Operation.ID != "" || len(view.Stages) != 0 {
				t.Fatalf("rejected resume returned a view: %+v", view)
			}
			current, getErr := repository.GetOperation(context.Background(), operation.TenantID, operation.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if current.AttemptCount != operation.AttemptCount || current.Version != operation.Version {
				t.Fatalf("rejected resume changed the operation: attempt=%d version=%d, want attempt=%d version=%d",
					current.AttemptCount, current.Version, operation.AttemptCount, operation.Version)
			}
			if current.Status != status || current.ErrorCode != AccessDeploymentEffectUnknown {
				t.Fatalf("rejected resume cleared the unknown effect: %+v", current)
			}
		})
	}
}

// TestSubsystemAccessResumeKeepsOrdinaryWaitingRecoverable 验证普通等待（认证观察期）不被误伤。
// 参数：t为应用服务及仓储替身上下文。
// 返回值：无，条件未清仍等待；条件补齐后必须恢复重排并递增领取轮次/版本。
func TestSubsystemAccessResumeKeepsOrdinaryWaitingRecoverable(t *testing.T) {
	facts := &accessFactsStub{version: "pv-1", facts: SubsystemAccessFacts{InScope: true, Registered: true,
		ControlledDeployment: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true,
		AuthObservationPending: true}}
	repository := newAccessRepositoryStub()
	service := accessServiceFor(t, facts, repository)
	operation := unknownAccessDeploymentOperation()
	operation.ErrorCode = ""
	repository.operations[operation.ID] = operation

	view, err := service.Resume(context.Background(), operation.TenantID, operation.ID)
	if err != nil || repository.requeued != 0 || view.Operation.Status != SubsystemAccessStatusWaiting {
		t.Fatalf("blocked ordinary resume: requeued=%d view=%+v err=%v", repository.requeued, view, err)
	}

	facts.facts.AuthObservationPending = false
	view, err = service.Resume(context.Background(), operation.TenantID, operation.ID)
	if err != nil || repository.requeued != 1 || view.Operation.Status != SubsystemAccessStatusPending {
		t.Fatalf("ordinary waiting was not recoverable: requeued=%d status=%s err=%v",
			repository.requeued, view.Operation.Status, err)
	}
	current := repository.operations[operation.ID]
	if current.AttemptCount != operation.AttemptCount+1 || current.Version != operation.Version+1 {
		t.Fatalf("ordinary requeue did not advance the operation: %+v", current)
	}
}

// TestSubsystemAccessResumeRecoversKnownFailure 验证已知失败仍可显式重试。
// 参数：t为应用服务及仓储替身上下文。
// 返回值：无，只有未知效果被拦截，已知失败不得一起被拒绝。
func TestSubsystemAccessResumeRecoversKnownFailure(t *testing.T) {
	facts := &accessFactsStub{version: "pv-1", facts: SubsystemAccessFacts{InScope: true, Registered: true,
		ControlledDeployment: true, ApprovalReady: true, LicenseReady: true, ImagesReady: true}}
	repository := newAccessRepositoryStub()
	service := accessServiceFor(t, facts, repository)
	operation := unknownAccessDeploymentOperation()
	operation.Status = SubsystemAccessStatusFailed
	operation.ErrorCode = "MANIFEST_MISMATCH"
	operation.Stage = SubsystemAccessStageReadiness
	repository.operations[operation.ID] = operation

	view, err := service.Resume(context.Background(), operation.TenantID, operation.ID)
	if err != nil || repository.requeued != 1 || view.Operation.Status != SubsystemAccessStatusPending {
		t.Fatalf("known failure was not recoverable: requeued=%d view=%+v err=%v", repository.requeued, view, err)
	}
}
