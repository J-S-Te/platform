package application

import (
	"context"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

type personnelHandoverManagerStub struct {
	items      []HandoverItem
	completed  bool
	targetUser string
}

func (stub *personnelHandoverManagerStub) Check(context.Context, PersonnelChangeRequest) (HandoverReport, error) {
	return HandoverReport{Ready: false, Outstanding: stub.items}, nil
}

func (stub *personnelHandoverManagerStub) List(context.Context, string, string) ([]HandoverItem, error) {
	return stub.items, nil
}

func (stub *personnelHandoverManagerStub) Complete(_ context.Context, _, _, itemID, targetUserID, _ string) (HandoverItem, error) {
	stub.completed = true
	stub.targetUser = targetUserID
	return HandoverItem{ID: itemID, TargetOwnerID: targetUserID, Status: "COMPLETED"}, nil
}

func TestCompleteHandoverRequiresTerminationAtHandoverStage(t *testing.T) {
	repository := &personnelChangeExecutionRepository{request: PersonnelChangeRequest{
		ID: "change-1", TenantID: "tenant-1", UserID: "user-1",
		ChangeType: domain.PersonnelChangeTermination, Status: domain.PersonnelChangePendingHandover,
	}}
	manager := &personnelHandoverManagerStub{items: []HandoverItem{{ID: "item-1", Status: "PENDING"}}}
	service, err := NewPersonnelChangeService(repository, personnelChangeLifecycleIDGenerator{}, personnelChangeLifecycleClock{}, manager)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	item, err := service.CompleteHandoverItem(context.Background(), "tenant-1", "change-1", "item-1", "user-2", "approver-1")
	if err != nil {
		t.Fatalf("complete handover: %v", err)
	}
	if !manager.completed || manager.targetUser != "user-2" || item.Status != "COMPLETED" {
		t.Fatalf("handover completion was not delegated correctly: manager=%+v item=%+v", manager, item)
	}

	repository.request.Status = domain.PersonnelChangeScheduled
	manager.completed = false
	if _, err := service.CompleteHandoverItem(context.Background(), "tenant-1", "change-1", "item-1", "user-2", "approver-1"); err != ErrConflict {
		t.Fatalf("scheduled termination must reject late handover edits, got %v", err)
	}
	if manager.completed {
		t.Fatal("handover manager must not run outside PENDING_HANDOVER")
	}
}
