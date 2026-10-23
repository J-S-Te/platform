package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type handoverCoordinatorStub struct {
	startCalls  []string
	cancelCalls []string
	startErr    error
	cancelErr   error

	verifyCalls  []string
	verification HandoverCompletionVerification
	verifyErr    error
}

func (stub *handoverCoordinatorStub) Start(_ context.Context, tenantID, requestID, userID string) (HandoverBatch, error) {
	stub.startCalls = append(stub.startCalls, tenantID+"|"+requestID+"|"+userID)
	if stub.startErr != nil {
		return HandoverBatch{}, stub.startErr
	}
	return HandoverBatch{ID: "batch-1", TenantID: tenantID, RequestID: requestID, UserID: userID, Status: HandoverBatchDiscovering}, nil
}

func (stub *handoverCoordinatorStub) CancelByRequest(_ context.Context, tenantID, requestID string) error {
	stub.cancelCalls = append(stub.cancelCalls, tenantID+"|"+requestID)
	return stub.cancelErr
}

// VerifyCrossSystemCompletion 让测试桩具备跨系统完成核验能力，默认返回"未完成"。
func (stub *handoverCoordinatorStub) VerifyCrossSystemCompletion(_ context.Context, tenantID, requestID string) (HandoverCompletionVerification, error) {
	stub.verifyCalls = append(stub.verifyCalls, tenantID+"|"+requestID)
	if stub.verifyErr != nil {
		return HandoverCompletionVerification{}, stub.verifyErr
	}
	return stub.verification, nil
}

// handoverCoordinatorWithoutVerification 模拟只实现基础编排、没有完成核验能力的协调器；
// 跨系统启用时这类协调器不得让排期绕过远端完成证明。
type handoverCoordinatorWithoutVerification struct{}

func (handoverCoordinatorWithoutVerification) Start(context.Context, string, string, string) (HandoverBatch, error) {
	return HandoverBatch{ID: "batch-1"}, nil
}

func (handoverCoordinatorWithoutVerification) CancelByRequest(context.Context, string, string) error {
	return nil
}

func pendingHandoverTermination() PersonnelChangeRequest {
	request := pendingApprovalRequest(domain.PersonnelChangeTermination)
	request.Status = domain.PersonnelChangePendingHandover
	return request
}

func terminationSchedulingService(t *testing.T, request PersonnelChangeRequest, coordinator HandoverCoordinator) (*PersonnelChangeService, *personnelChangeTransitionRepository) {
	t.Helper()
	repository := &personnelChangeTransitionRepository{request: request}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)}, stubHandoverChecker{ready: true})
	if err != nil {
		t.Fatal(err)
	}
	service.SetHandoverCoordinator(coordinator)
	return service, repository
}

// 本地交接项全部人工确认完成，但跨系统核验未完成时不得排期：
// 本地 COMPLETED 只是人工签认，不是远端责任转移回执。
func TestTerminationSchedulingRejectsLocallyCompleteButCrossSystemIncomplete(t *testing.T) {
	coordinator := &handoverCoordinatorStub{verification: HandoverCompletionVerification{BatchFound: true, BatchID: "batch-1", BatchStatus: HandoverBatchTransferring, UnresolvedItems: []string{"item-1"}}}
	service, repository := terminationSchedulingService(t, pendingHandoverTermination(), coordinator)

	_, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-1", ToStatus: domain.PersonnelChangeScheduled,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict when cross-system handover is incomplete", err)
	}
	if repository.request.Status != domain.PersonnelChangePendingHandover {
		t.Fatalf("status = %s, want the request to stay in PENDING_HANDOVER", repository.request.Status)
	}
	if len(coordinator.verifyCalls) != 1 || coordinator.verifyCalls[0] != "tenant-1|change-1" {
		t.Fatalf("verify calls = %v", coordinator.verifyCalls)
	}
}

// 跨系统核验本身出错时按失败关闭处理，不能把核验不可用当成完成。
func TestTerminationSchedulingReportsCrossSystemVerificationError(t *testing.T) {
	coordinator := &handoverCoordinatorStub{verifyErr: errors.New("orchestration store unavailable")}
	service, repository := terminationSchedulingService(t, pendingHandoverTermination(), coordinator)

	_, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-1", ToStatus: domain.PersonnelChangeScheduled,
	})
	if err == nil || !strings.Contains(err.Error(), "cross-system") {
		t.Fatalf("err = %v, want an explicit cross-system verification failure", err)
	}
	if repository.request.Status != domain.PersonnelChangePendingHandover {
		t.Fatalf("status = %s, want the request to stay in PENDING_HANDOVER", repository.request.Status)
	}
}

// 跨系统核验确认全部结清时才允许排期。
func TestTerminationSchedulingAllowsVerifiedCrossSystemCompletion(t *testing.T) {
	coordinator := &handoverCoordinatorStub{verification: HandoverCompletionVerification{BatchFound: true, BatchID: "batch-1", BatchStatus: HandoverBatchFrozen, CrossSystemComplete: true}}
	service, repository := terminationSchedulingService(t, pendingHandoverTermination(), coordinator)

	result, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-1", ToStatus: domain.PersonnelChangeScheduled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.PersonnelChangeScheduled {
		t.Fatalf("status = %s, want SCHEDULED", result.Status)
	}
	if repository.request.Status != domain.PersonnelChangeScheduled {
		t.Fatalf("stored status = %s, want SCHEDULED", repository.request.Status)
	}
}

// 跨系统模式启用却没有完成核验能力时必须失败关闭，不能退回只看本地勾选。
func TestTerminationSchedulingFailsClosedWithoutCrossSystemVerifier(t *testing.T) {
	service, repository := terminationSchedulingService(t, pendingHandoverTermination(), handoverCoordinatorWithoutVerification{})

	_, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", OperatorID: "approver-1", ID: "change-1", ToStatus: domain.PersonnelChangeScheduled,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict when the coordinator cannot verify cross-system completion", err)
	}
	if repository.request.Status != domain.PersonnelChangePendingHandover {
		t.Fatalf("status = %s, want the request to stay in PENDING_HANDOVER", repository.request.Status)
	}
}

func personnelServiceWithCoordinator(t *testing.T, request PersonnelChangeRequest, coordinator HandoverCoordinator) (*PersonnelChangeService, *approvalRepository) {
	t.Helper()
	repository := &approvalRepository{request: request}
	service := approvalTestService(t, repository)
	service.SetHandoverCoordinator(coordinator)
	return service, repository
}

// 离职审批通过进入交接阶段时必须真实启动跨系统交接。
func TestTransitionToPendingHandoverStartsOrchestration(t *testing.T) {
	coordinator := &handoverCoordinatorStub{}
	service, repository := personnelServiceWithCoordinator(t, pendingApprovalRequest(domain.PersonnelChangeTermination), coordinator)

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", ID: "change-1", ToStatus: domain.PersonnelChangePendingHandover,
		OperatorID: "approver-2", ApprovalReference: "APR-1001",
	}); err != nil {
		t.Fatal(err)
	}
	if repository.request.Status != domain.PersonnelChangePendingHandover {
		t.Fatalf("status = %s", repository.request.Status)
	}
	if len(coordinator.startCalls) != 1 || coordinator.startCalls[0] != "tenant-1|change-1|user-1" {
		t.Fatalf("start calls = %v", coordinator.startCalls)
	}
}

// 编排启动失败必须显式上报，不能静默进入交接阶段。
func TestTransitionReportsOrchestrationStartFailure(t *testing.T) {
	coordinator := &handoverCoordinatorStub{startErr: errors.New("freeze unavailable")}
	service, _ := personnelServiceWithCoordinator(t, pendingApprovalRequest(domain.PersonnelChangeTermination), coordinator)

	_, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", ID: "change-1", ToStatus: domain.PersonnelChangePendingHandover,
		OperatorID: "approver-2", ApprovalReference: "APR-1001",
	})
	if err == nil || !strings.Contains(err.Error(), "handover") {
		t.Fatalf("err = %v, want a handover start failure", err)
	}
}

// 取消失败（远端仍有在途操作）时不得推进状态。
func TestTransitionCancelRefusesWhenOrchestratorBlocks(t *testing.T) {
	request := pendingApprovalRequest(domain.PersonnelChangeTermination)
	request.Status = domain.PersonnelChangePendingHandover
	coordinator := &handoverCoordinatorStub{cancelErr: errors.Join(ErrConflict, errors.New("item in flight"))}
	service, repository := personnelServiceWithCoordinator(t, request, coordinator)

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", ID: "change-1", ToStatus: domain.PersonnelChangeCancelled, OperatorID: "operator-9",
	}); err == nil {
		t.Fatal("cancellation must fail while the remote system still has in-flight work")
	}
	if repository.request.Status != domain.PersonnelChangePendingHandover {
		t.Fatalf("status = %s, want the request to stay in PENDING_HANDOVER", repository.request.Status)
	}
	if len(coordinator.cancelCalls) != 1 {
		t.Fatalf("cancel calls = %v", coordinator.cancelCalls)
	}
}

// 编排确认无可取消在途操作后，取消正常推进。
func TestTransitionCancelSucceedsAfterOrchestratorReleases(t *testing.T) {
	request := pendingApprovalRequest(domain.PersonnelChangeTermination)
	request.Status = domain.PersonnelChangePendingHandover
	coordinator := &handoverCoordinatorStub{}
	service, repository := personnelServiceWithCoordinator(t, request, coordinator)

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", ID: "change-1", ToStatus: domain.PersonnelChangeCancelled, OperatorID: "operator-9",
	}); err != nil {
		t.Fatal(err)
	}
	if repository.request.Status != domain.PersonnelChangeCancelled {
		t.Fatalf("status = %s, want CANCELLED", repository.request.Status)
	}
	if len(coordinator.cancelCalls) != 1 || coordinator.cancelCalls[0] != "tenant-1|change-1" {
		t.Fatalf("cancel calls = %v", coordinator.cancelCalls)
	}
}

// 非离职异动不触发跨系统交接取消。
func TestTransitionCancelSkipsOrchestrationForNonTermination(t *testing.T) {
	request := pendingApprovalRequest(domain.PersonnelChangeTransfer)
	request.Status = domain.PersonnelChangePendingHandover
	coordinator := &handoverCoordinatorStub{}
	service, _ := personnelServiceWithCoordinator(t, request, coordinator)

	if _, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{
		TenantID: "tenant-1", ID: "change-1", ToStatus: domain.PersonnelChangeCancelled, OperatorID: "operator-9",
	}); err != nil {
		t.Fatal(err)
	}
	if len(coordinator.cancelCalls) != 0 {
		t.Fatalf("non-termination cancellation must not touch the handover orchestration: %v", coordinator.cancelCalls)
	}
}
