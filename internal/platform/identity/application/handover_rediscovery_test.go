package application

import (
	"context"
	"testing"
	"time"
)

func staleOrchestrator(t *testing.T, repository *handoverFakeRepository, capability string, client *handoverFakeClient) *HandoverOrchestrator {
	t.Helper()
	inventory := handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: capability}}}
	return handoverOrchestratorFor(t, repository, inventory, map[string]HandoverSystemClient{"contract": client})
}

// 历史滞留批次必须被重新冻结并补发现责任（冻结与落项都幂等）。
func TestRediscoverStaleBatchesRefreezesAndDiscovers(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := staleOrchestrator(t, repository, HandoverSystemApplicable, client)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	repository.batch.Status = HandoverBatchDiscovering
	repository.batch.UpdatedAt = now.Add(-48 * time.Hour)

	report, err := orchestrator.RediscoverStaleBatches(context.Background(), "tenant-1", now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Inspected != 1 || report.Refreshed != 1 || report.Blocked != 0 {
		t.Fatalf("report = %+v", report)
	}
	if !containsCall(client.calls, "freeze") || !containsCall(client.calls, "responsibilities") {
		t.Fatalf("rediscovery must re-freeze and re-discover: %v", client.calls)
	}
}

// 重新发现遇到阻断系统时批次转为 BLOCKED，且不静默视为完成。
func TestRediscoverStaleBatchesBlocksOnUnreachableSystem(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{}
	orchestrator := staleOrchestrator(t, repository, HandoverSystemUnreachable, client)
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	repository.batch = HandoverBatch{ID: "batch-1", TenantID: "tenant-1", RequestID: "request-1", UserID: "user-leaver", Status: HandoverBatchDiscovering, UpdatedAt: now.Add(-72 * time.Hour)}
	// 该批次此前已记录过系统适用性事实（Start 阶段写入），重新发现据此判定阻断。
	repository.systems = []HandoverSystemState{{System: "contract", Capability: HandoverSystemUnreachable}}

	report, err := orchestrator.RediscoverStaleBatches(context.Background(), "tenant-1", now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Blocked != 1 || report.Refreshed != 0 {
		t.Fatalf("report = %+v", report)
	}
	if repository.batch.Status != HandoverBatchBlocked {
		t.Fatalf("batch status = %s, want BLOCKED", repository.batch.Status)
	}
}

// 未到截止时间的批次不参与重新发现。
func TestRediscoverStaleBatchesSkipsFreshBatches(t *testing.T) {
	repository := newHandoverFakeRepository()
	orchestrator := staleOrchestrator(t, repository, HandoverSystemApplicable, &handoverFakeClient{})
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	repository.batch = HandoverBatch{ID: "batch-1", TenantID: "tenant-1", Status: HandoverBatchDiscovering, UpdatedAt: now}

	report, err := orchestrator.RediscoverStaleBatches(context.Background(), "tenant-1", now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Inspected != 0 {
		t.Fatalf("fresh batch must be skipped: %+v", report)
	}
}

// 本地"已执行"标记不能作为跨系统完成证明：未结项或阻断系统都必须让核对失败。
func TestVerifyCrossSystemCompletionRequiresResolvedItems(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := staleOrchestrator(t, repository, HandoverSystemApplicable, client)

	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	repository.batch.Status = HandoverBatchFrozen

	verification, err := orchestrator.VerifyCrossSystemCompletion(context.Background(), "tenant-1", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if !verification.BatchFound || verification.CrossSystemComplete || len(verification.UnresolvedItems) != 1 {
		t.Fatalf("pending item must block cross-system completion: %+v", verification)
	}

	for index := range repository.items {
		repository.items[index].Status = HandoverItemTransferred
	}
	verification, err = orchestrator.VerifyCrossSystemCompletion(context.Background(), "tenant-1", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if !verification.CrossSystemComplete {
		t.Fatalf("resolved items with applicable systems must verify: %+v", verification)
	}
}

// 平台本地人工责任项不属于跨系统编排（没有批次），不能在跨系统核验里要求远端回执；
// 它是否完成由本地 HandoverManager 闸门负责。编排管理的远端责任项仍必须是 TRANSFERRED/SKIPPED。
func TestVerifyCrossSystemCompletionIgnoresLocalManualItems(t *testing.T) {
	repository := newHandoverFakeRepository()
	repository.batch = HandoverBatch{ID: "batch-1", TenantID: "tenant-1", RequestID: "request-1", UserID: "user-leaver", Status: HandoverBatchFrozen}
	repository.items = []HandoverItem{
		{ID: "local-1", BatchID: "", System: "platform", ResourceType: "PERSONNEL_RESPONSIBILITY", Status: "COMPLETED"},
		{ID: "remote-1", BatchID: "batch-1", System: "contract", ResourceType: "contract", Status: HandoverItemTransferred},
	}
	repository.systems = []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable, Frozen: true}}
	orchestrator := handoverOrchestratorFor(t, repository, handoverFakeInventory{}, map[string]HandoverSystemClient{})

	verification, err := orchestrator.VerifyCrossSystemCompletion(context.Background(), "tenant-1", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if !verification.CrossSystemComplete || len(verification.UnresolvedItems) != 0 {
		t.Fatalf("local manual item must not be treated as an unresolved remote responsibility: %+v", verification)
	}
}

// 阻断系统或已取消批次一律不算跨系统完成。
func TestVerifyCrossSystemCompletionFalseOnBlockingOrCancelled(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for name, setup := range map[string]struct {
		capability string
		status     string
	}{
		"blocking-system": {capability: HandoverSystemLicenseDenied, status: HandoverBatchFrozen},
		"cancelled":       {capability: HandoverSystemApplicable, status: HandoverBatchCancelled},
	} {
		t.Run(name, func(t *testing.T) {
			repository := newHandoverFakeRepository()
			client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
			orchestrator := staleOrchestrator(t, repository, setup.capability, client)
			if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
				t.Fatal(err)
			}
			for index := range repository.items {
				repository.items[index].Status = HandoverItemTransferred
			}
			repository.batch.Status = setup.status

			verification, err := orchestrator.VerifyCrossSystemCompletion(context.Background(), "tenant-1", "request-1")
			if err != nil {
				t.Fatal(err)
			}
			if verification.CrossSystemComplete {
				t.Fatalf("must not verify as complete: %+v", verification)
			}
			_ = now
		})
	}
}
