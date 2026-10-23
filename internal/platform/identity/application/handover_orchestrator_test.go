package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

type handoverFakeRepository struct {
	batch        HandoverBatch
	items        []HandoverItem
	systems      []HandoverSystemState
	receipts     []HandoverTransferReceipt
	keys         map[string]bool
	insertedKeys map[string]bool
}

func newHandoverFakeRepository() *handoverFakeRepository {
	return &handoverFakeRepository{keys: map[string]bool{}, insertedKeys: map[string]bool{}}
}

func (r *handoverFakeRepository) EnsureBatch(_ context.Context, tenantID, requestID, userID string, now time.Time) (HandoverBatch, error) {
	if r.batch.ID == "" {
		r.batch = HandoverBatch{ID: "batch-1", TenantID: tenantID, RequestID: requestID, UserID: userID, Status: HandoverBatchDiscovering, Version: 1, CreatedAt: now, UpdatedAt: now}
	}
	return r.batch, nil
}
func (r *handoverFakeRepository) GetBatch(context.Context, string, string) (HandoverBatch, error) {
	return r.batch, nil
}
func (r *handoverFakeRepository) GetBatchByID(context.Context, string, string) (HandoverBatch, error) {
	return r.batch, nil
}
func (r *handoverFakeRepository) UpsertSystemState(_ context.Context, _ string, _ string, system, capability, reason string, now time.Time) error {
	for i := range r.systems {
		if r.systems[i].System == system {
			r.systems[i].Capability = capability
			return nil
		}
	}
	r.systems = append(r.systems, HandoverSystemState{System: system, Capability: capability})
	return nil
}
func (r *handoverFakeRepository) MarkFrozen(_ context.Context, _ string, _ string, system string, frozenAt time.Time, _ int) error {
	for i := range r.systems {
		if r.systems[i].System == system {
			r.systems[i].Frozen, r.systems[i].FrozenAt = true, &frozenAt
			return nil
		}
	}
	return ErrNotFound
}
func (r *handoverFakeRepository) AssignTarget(_ context.Context, _ string, itemID, targetUserID string, _ time.Time) error {
	for i := range r.items {
		if r.items[i].ID == itemID {
			r.items[i].TargetOwnerID = targetUserID
			return nil
		}
	}
	return ErrNotFound
}

func (r *handoverFakeRepository) InsertResponsibilityItems(_ context.Context, _ string, batchID, requestID, userID, targetUserID string, items []HandoverResponsibility, now time.Time) (int, error) {
	inserted := 0
	for _, item := range items {
		key := item.System + "|" + item.ResourceType + "|" + item.ResourceID
		if r.insertedKeys[key] {
			continue
		}
		r.insertedKeys[key] = true
		id := "item-" + item.ResourceID
		itemID := id
		r.items = append(r.items, HandoverItem{
			ID: itemID, BatchID: batchID, System: item.System, ResourceType: item.ResourceType, ResourceID: item.ResourceID,
			CurrentOwnerID: userID, TargetOwnerID: targetUserID, Status: HandoverItemPending,
			ExpectedVersion: item.Version, IdempotencyKey: HandoverIdempotencyKey(batchID, itemID), Version: 1,
		})
		inserted++
	}
	return inserted, nil
}
func (r *handoverFakeRepository) ClaimNextItem(_ context.Context, _ string, _ string, leaseID string, leaseDeadline, now time.Time) (HandoverItem, bool, error) {
	for i := range r.items {
		item := &r.items[i]
		if item.Status != HandoverItemPending || item.TargetOwnerID == "" {
			continue
		}
		if item.NextRetryAt != nil && item.NextRetryAt.After(now) {
			continue
		}
		if item.LeaseDeadline != nil && !HandoverLeaseExpired(*item.LeaseDeadline, now) {
			continue
		}
		item.LeaseID, item.LeaseDeadline, item.Attempts, item.Version = leaseID, &leaseDeadline, item.Attempts+1, item.Version+1
		return *item, true, nil
	}
	return HandoverItem{}, false, nil
}
func (r *handoverFakeRepository) RecordReceipt(_ context.Context, _ string, _ string, expectedVersion uint64, receipt HandoverTransferReceipt, now time.Time) (bool, error) {
	if r.keys[receipt.IdempotencyKey] {
		for _, previous := range r.receipts {
			if previous.IdempotencyKey == receipt.IdempotencyKey {
				if terminal, _ := HandoverReceiptTerminal(previous.Status); terminal {
					return false, nil
				}
			}
		}
	}
	r.keys[receipt.IdempotencyKey] = true
	r.receipts = append(r.receipts, receipt)
	for i := range r.items {
		item := &r.items[i]
		if item.ID != receipt.ItemID {
			continue
		}
		if item.Version != expectedVersion {
			return false, ErrConflict
		}
		item.LeaseID, item.LeaseDeadline, item.Version = "", nil, item.Version+1
		switch receipt.Status {
		case HandoverReceiptApplied, HandoverReceiptAlreadyApplied:
			item.Status, item.AppliedVersion = HandoverItemTransferred, receipt.AppliedVersion
		case HandoverReceiptRejected:
			item.Status, item.LastErrorCode = HandoverItemFailed, receipt.ReasonCode
		default:
			next := NextHandoverRetryAt(int(item.Attempts), now)
			item.NextRetryAt, item.LastErrorCode = &next, receipt.ReasonCode
		}
		return true, nil
	}
	return false, ErrNotFound
}
func (r *handoverFakeRepository) CompleteBatch(_ context.Context, _ string, _ string, status string, now time.Time) error {
	r.batch.Status, r.batch.CompletedAt = status, &now
	return nil
}
func (r *handoverFakeRepository) RequestCancellation(_ context.Context, _, _ string, now time.Time) error {
	r.batch.CancelRequestedAt = &now
	return nil
}
func (r *handoverFakeRepository) ListSystemStates(context.Context, string, string) ([]HandoverSystemState, error) {
	return r.systems, nil
}
func (r *handoverFakeRepository) ListStaleBatches(_ context.Context, tenantID string, olderThan time.Time, limit int) ([]HandoverBatch, error) {
	if r.batch.ID == "" || r.batch.TenantID != tenantID {
		return nil, nil
	}
	switch r.batch.Status {
	case HandoverBatchDiscovering, HandoverBatchFrozen, HandoverBatchTransferring, HandoverBatchBlocked:
	default:
		return nil, nil
	}
	if !r.batch.UpdatedAt.Before(olderThan) {
		return nil, nil
	}
	return []HandoverBatch{r.batch}, nil
}

func (r *handoverFakeRepository) ListItems(context.Context, string, string) ([]HandoverItem, error) {
	return r.items, nil
}

type handoverFakeClient struct {
	calls            []string
	responsibilities []HandoverResponsibility
	transfer         HandoverTransferReceipt
	transferErr      error
	receipt          HandoverTransferReceipt
	receiptErr       error
	finishOutcomes   []string
	freezeFails      bool
}

func (c *handoverFakeClient) Freeze(context.Context, HandoverFreezeRequest) (HandoverFreezeResponse, error) {
	c.calls = append(c.calls, "freeze")
	if c.freezeFails {
		return HandoverFreezeResponse{}, errors.New("freeze rejected")
	}
	return HandoverFreezeResponse{Frozen: true}, nil
}
func (c *handoverFakeClient) Responsibilities(context.Context, string, string, int) (HandoverResponsibilityPage, error) {
	c.calls = append(c.calls, "responsibilities")
	return HandoverResponsibilityPage{Items: c.responsibilities}, nil
}
func (c *handoverFakeClient) Transfer(context.Context, HandoverTransferRequest) (HandoverTransferReceipt, error) {
	c.calls = append(c.calls, "transfer")
	return c.transfer, c.transferErr
}
func (c *handoverFakeClient) Receipt(context.Context, string, string) (HandoverTransferReceipt, error) {
	c.calls = append(c.calls, "receipt")
	return c.receipt, c.receiptErr
}
func (c *handoverFakeClient) Finish(_ context.Context, _, _, outcome string) error {
	c.calls = append(c.calls, "finish:"+outcome)
	c.finishOutcomes = append(c.finishOutcomes, outcome)
	return nil
}

type handoverFakeInventory struct{ states []HandoverSystemState }

func (i handoverFakeInventory) Systems(context.Context, string) ([]HandoverSystemState, error) {
	return i.states, nil
}

func handoverOrchestratorFor(t *testing.T, repository *handoverFakeRepository, inventory HandoverSystemInventory, clients map[string]HandoverSystemClient) *HandoverOrchestrator {
	t.Helper()
	orchestrator, err := NewHandoverOrchestrator(repository, inventory, clients, &sequentialIDGenerator{}, fixedClock{now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator
}

type sequentialIDGenerator struct{ n int }

func (g *sequentialIDGenerator) New(time.Time) (string, error) {
	g.n++
	return "01HANDOVERID" + string(rune('0'+g.n%10)) + "0000000000000", nil
}

func TestStartFreezesBeforeDiscoveryAndSkipsNotInstalled(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := handoverOrchestratorFor(t, repository,
		handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}, {System: "portal", Capability: HandoverSystemNotInstalled}}},
		map[string]HandoverSystemClient{"contract": client})

	batch, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.calls) < 2 || client.calls[0] != "freeze" || client.calls[1] != "responsibilities" {
		t.Fatalf("freeze must precede discovery, calls = %v", client.calls)
	}
	if len(repository.items) != 1 || len(repository.systems) != 2 {
		t.Fatalf("items=%d systems=%d", len(repository.items), len(repository.systems))
	}
	for _, state := range repository.systems {
		if state.System == "portal" && state.Frozen {
			t.Fatal("not-installed systems must not be frozen")
		}
		if state.System == "contract" && !state.Frozen {
			t.Fatal("applicable systems must be frozen")
		}
	}
	if batch.Status == HandoverBatchBlocked {
		t.Fatal("not-installed systems must not block the batch")
	}
}

func TestStartBlocksOnUnavailableOrMissingClient(t *testing.T) {
	for name, states := range map[string][]HandoverSystemState{
		"unreachable": {{System: "contract", Capability: HandoverSystemUnreachable}},
		"no-client":   {{System: "contract", Capability: HandoverSystemApplicable}},
		"license":     {{System: "contract", Capability: HandoverSystemLicenseDenied}},
	} {
		t.Run(name, func(t *testing.T) {
			repository := newHandoverFakeRepository()
			clients := map[string]HandoverSystemClient{}
			if name != "no-client" {
				clients["contract"] = &handoverFakeClient{}
			}
			orchestrator := handoverOrchestratorFor(t, repository, handoverFakeInventory{states: states}, clients)
			batch, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver")
			if err != nil {
				t.Fatal(err)
			}
			if batch.Status != HandoverBatchBlocked {
				t.Fatalf("batch status = %s, want BLOCKED", batch.Status)
			}
		})
	}
}

func TestDispatchRecordsTerminalReceipt(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := handoverOrchestratorFor(t, repository, handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}}, map[string]HandoverSystemClient{"contract": client})
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	repository.batch.Status = HandoverBatchFrozen
	assignAllTargets(t, orchestrator, repository)

	itemID := repository.items[0].ID
	client.transfer = HandoverTransferReceipt{ItemID: itemID, IdempotencyKey: repository.items[0].IdempotencyKey, Status: HandoverReceiptApplied, AppliedVersion: 4}
	dispatched, err := orchestrator.DispatchOnce(context.Background(), "tenant-1", "batch-1", 5)
	if err != nil || dispatched != 1 {
		t.Fatalf("dispatch = %d/%v", dispatched, err)
	}
	if repository.items[0].Status != HandoverItemTransferred || repository.items[0].AppliedVersion != 4 {
		t.Fatalf("item not transferred: %+v", repository.items[0])
	}
}

func TestDispatchQueriesReceiptBeforeRetryWhenTransferFails(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := handoverOrchestratorFor(t, repository, handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}}, map[string]HandoverSystemClient{"contract": client})
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	repository.batch.Status = HandoverBatchFrozen
	assignAllTargets(t, orchestrator, repository)

	item := repository.items[0]
	client.transferErr = errors.New("timeout")
	client.receipt = HandoverTransferReceipt{ItemID: item.ID, IdempotencyKey: item.IdempotencyKey, Status: HandoverReceiptApplied, AppliedVersion: 5}
	if _, err := orchestrator.DispatchOnce(context.Background(), "tenant-1", "batch-1", 5); err != nil {
		t.Fatal(err)
	}
	if !containsCall(client.calls, "receipt") {
		t.Fatalf("unknown outcome must query the receipt first, calls = %v", client.calls)
	}
	if repository.items[0].Status != HandoverItemTransferred {
		t.Fatalf("receipt reported success but item = %+v", repository.items[0])
	}
}

func TestDispatchSchedulesRetryWhenOutcomeUnknown(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := handoverOrchestratorFor(t, repository, handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}}, map[string]HandoverSystemClient{"contract": client})
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	repository.batch.Status = HandoverBatchFrozen
	assignAllTargets(t, orchestrator, repository)

	client.transferErr = errors.New("timeout")
	client.receiptErr = errors.New("receipt unavailable")
	if _, err := orchestrator.DispatchOnce(context.Background(), "tenant-1", "batch-1", 5); err != nil {
		t.Fatal(err)
	}
	item := repository.items[0]
	if item.Status != HandoverItemPending || item.NextRetryAt == nil || item.LastErrorCode != "TRANSFER_RESULT_UNKNOWN" {
		t.Fatalf("unknown outcome must schedule a retry: %+v", item)
	}
}

func TestCompleteRefusesUnresolvedItemsAndReleasesSystems(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := handoverOrchestratorFor(t, repository, handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}}, map[string]HandoverSystemClient{"contract": client})
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	repository.batch.Status = HandoverBatchFrozen
	assignAllTargets(t, orchestrator, repository)

	if err := orchestrator.Complete(context.Background(), "tenant-1", "batch-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending items must block completion: %v", err)
	}
	repository.items[0].Status = HandoverItemTransferred
	if err := orchestrator.Complete(context.Background(), "tenant-1", "batch-1"); err != nil {
		t.Fatal(err)
	}
	if !containsCall(client.calls, "finish:COMPLETED") {
		t.Fatalf("finish must be sent on completion, calls = %v", client.calls)
	}
	if repository.batch.Status != HandoverBatchCompleted {
		t.Fatalf("batch status = %s", repository.batch.Status)
	}
}

func TestCancelRefusesInFlightItemThenReleasesFrozenSystems(t *testing.T) {
	repository := newHandoverFakeRepository()
	client := &handoverFakeClient{responsibilities: []HandoverResponsibility{{System: "contract", ResourceType: "contract", ResourceID: "c-1", OwnerUserID: "user-leaver", Version: 3}}}
	orchestrator := handoverOrchestratorFor(t, repository, handoverFakeInventory{states: []HandoverSystemState{{System: "contract", Capability: HandoverSystemApplicable}}}, map[string]HandoverSystemClient{"contract": client})
	if _, err := orchestrator.Start(context.Background(), "tenant-1", "request-1", "user-leaver"); err != nil {
		t.Fatal(err)
	}
	repository.batch.Status = HandoverBatchFrozen
	assignAllTargets(t, orchestrator, repository)

	future := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	repository.items[0].LeaseID, repository.items[0].LeaseDeadline = "lease-1", &future
	if err := orchestrator.Cancel(context.Background(), "tenant-1", "batch-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("in-flight item must block cancellation: %v", err)
	}
	repository.items[0].LeaseID, repository.items[0].LeaseDeadline = "", nil
	repository.items[0].Status = HandoverItemTransferred
	if err := orchestrator.Cancel(context.Background(), "tenant-1", "batch-1"); err != nil {
		t.Fatal(err)
	}
	if !containsCall(client.calls, "finish:CANCELLED") {
		t.Fatalf("cancellation must release frozen systems, calls = %v", client.calls)
	}
	if repository.batch.Status != HandoverBatchCancelled {
		t.Fatalf("batch status = %s", repository.batch.Status)
	}
	if repository.items[0].Status != HandoverItemTransferred {
		t.Fatal("completed transfers must be preserved on cancellation")
	}
}

func containsCall(calls []string, expected string) bool {
	for _, call := range calls {
		if call == expected {
			return true
		}
	}
	return false
}

// assignAllTargets 在单测中模拟管理员逐一指定承接人。
func assignAllTargets(t *testing.T, orchestrator *HandoverOrchestrator, repository *handoverFakeRepository) {
	t.Helper()
	for i := range repository.items {
		if repository.items[i].TargetOwnerID == "" {
			if err := orchestrator.AssignTarget(context.Background(), repository.batch.TenantID, repository.items[i].ID, "user-successor"); err != nil {
				t.Fatal(err)
			}
		}
	}
}
