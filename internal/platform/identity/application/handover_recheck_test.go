package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type reviewUnavailableInventory struct{}

func (reviewUnavailableInventory) Systems(context.Context, string) ([]HandoverSystemState, error) {
	return nil, errors.New("review fixture: inventory unavailable")
}

type reviewManager struct {
	repository *handoverFakeRepository
	manual     HandoverItem
}

func (m *reviewManager) List(ctx context.Context, tenant, request string) ([]HandoverItem, error) {
	return append([]HandoverItem{m.manual}, m.repository.items...), nil
}
func (m *reviewManager) Check(ctx context.Context, request PersonnelChangeRequest) (HandoverReport, error) {
	items, _ := m.List(ctx, request.TenantID, request.ID)
	report := HandoverReport{Ready: true}
	for _, item := range items {
		if item.Status != "COMPLETED" && item.Status != HandoverItemTransferred {
			report.Ready = false
			report.Outstanding = append(report.Outstanding, item)
		}
	}
	return report, nil
}
func (m *reviewManager) Complete(_ context.Context, _, _, id, target, _ string) (HandoverItem, error) {
	if id != m.manual.ID {
		return HandoverItem{}, ErrConflict
	}
	m.manual.Status = "COMPLETED"
	m.manual.TargetOwnerID = target
	return m.manual, nil
}

func TestReviewServiceTransfersRemoteAndKeepsFreezeWhileScheduled(t *testing.T) {
	ctx := context.Background()
	repo := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-1", Version: 2}}, transferErr: errors.New("timeout"), receiptErr: errors.New("unavailable")}
	orchestrator := staleOrchestrator(t, repo, HandoverSystemApplicable, client)
	manager := &reviewManager{repository: repo, manual: HandoverItem{ID: "local-1", Status: "PENDING"}}
	personnelRepo := &personnelChangeTransitionRepository{request: pendingHandoverTermination()}
	service, err := NewPersonnelChangeService(personnelRepo, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{}, manager)
	if err != nil {
		t.Fatal(err)
	}
	service.SetHandoverCoordinator(orchestrator)
	if _, err := service.RetryHandover(ctx, "tenant-1", "change-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteHandoverItem(ctx, "tenant-1", "change-1", "local-1", "successor", "operator"); err != nil {
		t.Fatal(err)
	}
	item, err := service.CompleteHandoverItem(ctx, "tenant-1", "change-1", repo.items[0].ID, "successor", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != HandoverItemPending || item.NextRetryAt == nil {
		t.Fatalf("unknown transfer must remain pending: %+v", item)
	}
	key := item.IdempotencyKey
	client.transferErr = nil
	client.receiptErr = nil
	client.receipt = HandoverTransferReceipt{System: "contract", ItemID: item.ID, IdempotencyKey: key, Status: HandoverReceiptAlreadyApplied, AppliedVersion: 3}
	orchestrator.clock = fixedClock{now: item.NextRetryAt.Add(time.Second)}
	client.calls = nil
	item, err = service.CompleteHandoverItem(ctx, "tenant-1", "change-1", item.ID, "successor", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != HandoverItemTransferred || item.IdempotencyKey != key {
		t.Fatalf("receipt did not resolve stable key: %+v", item)
	}
	if len(client.calls) != 1 || client.calls[0] != "receipt" {
		t.Fatalf("retry must query receipt without resending applied transfer: %v", client.calls)
	}
	if _, err := service.Transition(ctx, PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "operator", ID: "change-1", ToStatus: domain.PersonnelChangeScheduled}); err != nil {
		t.Fatal(err)
	}
	if repo.batch.Status != HandoverBatchFrozen || containsCall(client.calls, "finish:COMPLETED") {
		t.Fatal("scheduling released the freeze before local termination")
	}
}

// 职责发现失败不能证明不存在跨系统职责。
func TestReviewInventoryFailureMustNotProveHandoverComplete(t *testing.T) {
	repo := newHandoverFakeRepository()
	orchestrator := handoverOrchestratorFor(t, repo, reviewUnavailableInventory{}, map[string]HandoverSystemClient{})
	if _, err := orchestrator.Start(context.Background(), "review-tenant", "review-request", "review-leaver"); err == nil {
		t.Fatal("fixture did not enter the inventory failure path")
	}
	result, err := orchestrator.VerifyCrossSystemCompletion(context.Background(), "review-tenant", "review-request")
	if err == nil && result.CrossSystemComplete {
		t.Fatalf("failed inventory was accepted as complete: batch=%s", result.BatchStatus)
	}
	if err := orchestrator.Complete(context.Background(), "review-tenant", repo.batch.ID); err == nil {
		t.Fatal("failed inventory must also block batch completion")
	}
}

type reviewDiscoveryFailureClient struct{ handoverFakeClient }

func (*reviewDiscoveryFailureClient) Responsibilities(context.Context, string, string, int) (HandoverResponsibilityPage, error) {
	return HandoverResponsibilityPage{}, errors.New("page unavailable")
}

func TestReviewDiscoveryFailureAndUnfinishedBatchBlockCompletion(t *testing.T) {
	repo := newHandoverFakeRepository()
	orchestrator := handoverOrchestratorFor(t, repo, handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}}, map[string]HandoverSystemClient{"contract": &reviewDiscoveryFailureClient{}})
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "leaver"); err == nil {
		t.Fatal("expected discovery error")
	}
	result, err := orchestrator.VerifyCrossSystemCompletion(context.Background(), "tenant-1", "request-1")
	if err == nil && result.CrossSystemComplete {
		t.Fatal("partial discovery accepted")
	}
	if err := orchestrator.Complete(context.Background(), "tenant-1", repo.batch.ID); err == nil {
		t.Fatal("partial discovery completed")
	}
}

func TestReviewRemoteResponsibilityCannotBeManuallyCompleted(t *testing.T) {
	repo := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{ID: "change-1", TenantID: "tenant-1", UserID: "user-1", ChangeType: domain.PersonnelChangeTermination, Status: domain.PersonnelChangePendingHandover}}
	manager := &personnelHandoverManagerStub{items: []HandoverItem{{ID: "remote-1", BatchID: "batch-1", Status: "PENDING"}}}
	service, err := NewPersonnelChangeService(repo, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{}, manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteHandoverItem(context.Background(), "tenant-1", "change-1", "remote-1", "user-2", "operator"); err == nil {
		t.Fatal("remote item accepted by manual completion")
	}
	if manager.completed {
		t.Fatal("manual writer called for remote item")
	}
}

func TestReviewExecutionRechecksHandover(t *testing.T) {
	request := pendingHandoverTermination()
	request.Status = domain.PersonnelChangeScheduled
	coordinator := &handoverCoordinatorStub{verification: HandoverCompletionVerification{BatchFound: true, BatchStatus: HandoverBatchBlocked}}
	service, _ := terminationSchedulingService(t, request, coordinator)
	_, err := service.Transition(context.Background(), PersonnelChangeTransitionInput{TenantID: "tenant-1", OperatorID: "worker", ID: "change-1", ToStatus: domain.PersonnelChangeExecuted, Immediate: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("execution must recheck handover: %v", err)
	}
	if len(coordinator.verifyCalls) != 1 {
		t.Fatal("execution did not verify cross-system completion")
	}
}

func TestReviewZeroResponsibilitiesAndRetryDiscovery(t *testing.T) {
	repo := newHandoverFakeRepository()
	client := &handoverFakeClient{freezeFails: true}
	orchestrator := staleOrchestrator(t, repo, HandoverSystemApplicable, client)
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "leaver"); err != nil {
		t.Fatal(err)
	}
	if repo.batch.Status != HandoverBatchBlocked {
		t.Fatal("expected blocked freeze")
	}
	client.freezeFails = false
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "leaver"); err != nil {
		t.Fatal(err)
	}
	if repo.batch.Status != HandoverBatchFrozen {
		t.Fatalf("discovery did not recover: %s", repo.batch.Status)
	}
	repo.items = append(repo.items, HandoverItem{ID: "manual", Status: "COMPLETED"})
	if err := orchestrator.Complete(context.Background(), "tenant-1", repo.batch.ID); err != nil {
		t.Fatal(err)
	}
	if repo.batch.Status != HandoverBatchCompleted {
		t.Fatal("legitimate zero remote responsibilities not completed")
	}
}

func TestReviewCancellationUnknownResultMustNotReleaseFreeze(t *testing.T) {
	repo := newHandoverFakeRepository()
	client := &handoverFakeClient{receiptErr: errors.New("unavailable")}
	orchestrator := staleOrchestrator(t, repo, HandoverSystemApplicable, client)
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "leaver"); err != nil {
		t.Fatal(err)
	}
	repo.items = []HandoverItem{{ID: "unknown", BatchID: repo.batch.ID, Status: HandoverItemPending, Attempts: 1, LastErrorCode: "TRANSFER_RESULT_UNKNOWN"}}
	if err := orchestrator.Cancel(context.Background(), "tenant-1", repo.batch.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown remote result must block cancellation: %v", err)
	}
	if containsCall(client.calls, "finish:CANCELLED") {
		t.Fatal("released freeze before reconciling unknown result")
	}
	if repo.batch.CancelRequestedAt == nil {
		t.Fatal("cancellation intent was not persisted")
	}
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "leaver"); !errors.Is(err, ErrConflict) {
		t.Fatal("discovery resumed while cancelling")
	}
	if _, err := orchestrator.DispatchOnce(context.Background(), "tenant-1", repo.batch.ID, 1); err == nil {
		t.Fatal("dispatch resumed while cancelling")
	}
}

func TestReviewUnfrozenSystemCannotProveCompletion(t *testing.T) {
	repo := newHandoverFakeRepository()
	repo.batch = HandoverBatch{ID: "batch-1", RequestID: "request-1", Status: HandoverBatchFrozen}
	repo.systems = []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable, Frozen: false}}
	orchestrator := staleOrchestrator(t, repo, HandoverSystemApplicable, &handoverFakeClient{})
	result, err := orchestrator.VerifyCrossSystemCompletion(context.Background(), "tenant-1", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.CrossSystemComplete || len(result.BlockingSystems) != 1 {
		t.Fatal("unconfirmed freeze accepted")
	}
}
