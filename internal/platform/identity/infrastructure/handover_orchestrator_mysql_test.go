package infrastructure_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
)

// fakeHandoverSystem 是协议服务端的内存实现，用于在隔离库上验证平台编排的端到端行为。
type fakeHandoverSystem struct {
	mutex            sync.Mutex
	responsibilities []application.HandoverResponsibility
	owners           map[string]string
	transfers        map[string]application.HandoverTransferReceipt
	outcomes         []string
	frozen           bool
	transferCalls    int
	finishFailures   int
	finishCalls      int
}

func newFakeHandoverSystem(resources ...string) *fakeHandoverSystem {
	system := &fakeHandoverSystem{owners: map[string]string{}, transfers: map[string]application.HandoverTransferReceipt{}}
	for _, resource := range resources {
		system.responsibilities = append(system.responsibilities, application.HandoverResponsibility{
			System: "contract", ResourceType: "contract", ResourceID: resource, ResourceName: resource,
			OwnerUserID: "user-leaver", Category: "OWNER", Version: 3, UpdatedAt: time.Now().UTC(),
		})
		system.owners[resource] = "user-leaver"
	}
	return system
}

func (system *fakeHandoverSystem) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/internal/handover/freeze":
		system.mutex.Lock()
		system.frozen = true
		count := len(system.responsibilities)
		system.mutex.Unlock()
		writeJSON(writer, map[string]any{"system": "contract", "frozen": true, "responsibility_count": count})
	case request.Method == http.MethodGet && request.URL.Path == "/internal/handover/responsibilities":
		system.mutex.Lock()
		items := append([]application.HandoverResponsibility(nil), system.responsibilities...)
		system.mutex.Unlock()
		writeJSON(writer, map[string]any{"system": "contract", "items": items})
	case request.Method == http.MethodPost && request.URL.Path == "/internal/handover/transfers":
		var body struct {
			ItemID          string `json:"item_id"`
			ResourceID      string `json:"resource_id"`
			ToUserID        string `json:"to_user_id"`
			ExpectedVersion uint64 `json:"expected_version"`
			IdempotencyKey  string `json:"idempotency_key"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		system.mutex.Lock()
		defer system.mutex.Unlock()
		system.transferCalls++
		if existing, ok := system.transfers[body.IdempotencyKey]; ok {
			writeJSON(writer, existing)
			return
		}
		if system.owners[body.ResourceID] != "user-leaver" {
			writeJSON(writer, application.HandoverTransferReceipt{ItemID: body.ItemID, IdempotencyKey: body.IdempotencyKey, Status: application.HandoverReceiptRejected, ReasonCode: "OWNER_CHANGED"})
			return
		}
		system.owners[body.ResourceID] = body.ToUserID
		receipt := application.HandoverTransferReceipt{
			System: "contract", ItemID: body.ItemID, IdempotencyKey: body.IdempotencyKey,
			Status: application.HandoverReceiptApplied, AppliedVersion: body.ExpectedVersion + 1,
			EvidenceRef: "audit:" + body.ResourceID, ReceivedAt: time.Now().UTC(),
		}
		system.transfers[body.IdempotencyKey] = receipt
		writeJSON(writer, receipt)
	case request.Method == http.MethodGet && len(request.URL.Path) > len("/internal/handover/transfers/"):
		key := request.URL.Query().Get("idempotency_key")
		system.mutex.Lock()
		receipt, ok := system.transfers[key]
		system.mutex.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			writeJSON(writer, map[string]any{"code": "RECEIPT_NOT_FOUND"})
			return
		}
		writeJSON(writer, receipt)
	case request.Method == http.MethodPost && request.URL.Path == "/internal/handover/complete":
		var body struct {
			Outcome string `json:"outcome"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		system.mutex.Lock()
		system.finishCalls++
		if system.finishFailures > 0 {
			system.finishFailures--
			system.mutex.Unlock()
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		system.outcomes = append(system.outcomes, body.Outcome)
		system.frozen = false
		system.mutex.Unlock()
		writeJSON(writer, map[string]any{"system": "contract", "batch_id": "batch", "outcome": body.Outcome})
	default:
		writer.WriteHeader(http.StatusNotFound)
		writeJSON(writer, map[string]any{"code": "NOT_FOUND"})
	}
}

func writeJSON(writer http.ResponseWriter, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(payload)
}

type handoverInventoryStub struct{ capability string }

func (stub handoverInventoryStub) Systems(context.Context, string) ([]application.HandoverSystemState, error) {
	capability := stub.capability
	if capability == "" {
		capability = application.HandoverSystemApplicable
	}
	return []application.HandoverSystemState{{System: "contract", Capability: capability}}, nil
}

func handoverEndToEndService(t *testing.T, server *httptest.Server, capability string) (*application.HandoverOrchestrator, *infrastructure.PersonnelHandoverGORMRepository, *gorm.DB) {
	t.Helper()
	database := handoverTestDatabase(t)
	repository, err := infrastructure.NewPersonnelHandoverGORMRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	client := handoverEndToEndClient(t, server)
	orchestrator, err := application.NewHandoverOrchestrator(repository, handoverInventoryStub{capability: capability},
		map[string]application.HandoverSystemClient{"contract": client}, ulid.Generator{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator, repository, database
}

func handoverEndToEndClient(t *testing.T, server *httptest.Server) *infrastructure.HandoverHTTPClient {
	t.Helper()
	client, err := infrastructure.NewHandoverHTTPClient(infrastructure.HandoverHTTPClientConfig{
		System: "contract", BaseURL: server.URL, MachineToken: "machine-token-value",
		ApplicationCode: "platform", Environment: "prod", Timeout: 5 * time.Second,
		RequestID: func() (string, error) { return ulid.Generator{}.New(time.Now().UTC()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestHandoverEndToEndStartDispatchCompleteOnMySQL(t *testing.T) {
	system := newFakeHandoverSystem("c-1", "c-2")
	server := httptest.NewServer(system)
	defer server.Close()
	orchestrator, repository, database := handoverEndToEndService(t, server, "")
	seedActiveHandoverUser(t, database, "tenant-e2e", "user-successor")
	ctx := context.Background()

	batch, err := orchestrator.Start(ctx, "tenant-e2e", "request-e2e", "user-leaver")
	if err != nil {
		t.Fatal(err)
	}
	if batch.Status == application.HandoverBatchBlocked {
		t.Fatalf("batch must not be blocked: %+v", batch)
	}
	if !system.frozen {
		t.Fatal("business system must be frozen before discovery")
	}
	items, err := repository.ListItems(ctx, "tenant-e2e", "request-e2e")
	if err != nil || len(items) != 2 {
		t.Fatalf("discovered items = %d (err=%v)", len(items), err)
	}
	// 发现阶段不指定承接人：未指派的项不得派发，转移目标也绝不能默认成离职者本人。
	if _, err := orchestrator.DispatchOnce(ctx, "tenant-e2e", batch.ID, 5); err != nil {
		t.Fatal(err)
	}
	if system.transferCalls != 0 {
		t.Fatal("items without an assigned successor must not be dispatched")
	}
	for _, item := range items {
		if err := orchestrator.AssignTarget(ctx, "tenant-e2e", item.ID, "user-successor"); err != nil {
			t.Fatal(err)
		}
	}
	assigned, err := repository.ListItems(ctx, "tenant-e2e", "request-e2e")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range assigned {
		if item.TargetOwnerID != "user-successor" {
			t.Fatalf("successor assignment did not persist: %+v", item)
		}
	}

	for attempt := 0; attempt < 3; attempt++ {
		dispatched, err := orchestrator.DispatchOnce(ctx, "tenant-e2e", batch.ID, 5)
		if err != nil {
			t.Fatal(err)
		}
		if dispatched == 0 {
			break
		}
	}
	items, err = repository.ListItems(ctx, "tenant-e2e", "request-e2e")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Status != application.HandoverItemTransferred || item.AppliedVersion == 0 || item.EvidenceRef == "" {
			t.Fatalf("item not transferred with evidence: %+v", item)
		}
	}
	if system.owners["c-1"] != "user-successor" && system.owners["c-1"] == "user-leaver" {
		t.Fatalf("business ownership was not transferred: %v", system.owners)
	}
	if err := orchestrator.Complete(ctx, "tenant-e2e", batch.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := repository.GetBatchByID(ctx, "tenant-e2e", batch.ID)
	if err != nil || completed.Status != application.HandoverBatchCompleted {
		t.Fatalf("batch = %+v (err=%v)", completed, err)
	}
	if len(system.outcomes) != 1 || system.outcomes[0] != "COMPLETED" {
		t.Fatalf("system outcomes = %v, want COMPLETED", system.outcomes)
	}
	if system.frozen {
		t.Fatal("completion must release the system freeze")
	}
}

func TestHandoverEndToEndCancelPreservesTransfersOnMySQL(t *testing.T) {
	system := newFakeHandoverSystem("c-1")
	server := httptest.NewServer(system)
	defer server.Close()
	orchestrator, repository, database := handoverEndToEndService(t, server, "")
	seedActiveHandoverUser(t, database, "tenant-cancel", "user-successor")
	ctx := context.Background()

	batch, err := orchestrator.Start(ctx, "tenant-cancel", "request-cancel", "user-leaver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.DispatchOnce(ctx, "tenant-cancel", batch.ID, 5); err != nil {
		t.Fatal(err)
	}
	cancelItems, err := repository.ListItems(ctx, "tenant-cancel", "request-cancel")
	if err != nil || len(cancelItems) != 1 {
		t.Fatalf("discovered items = %d (err=%v)", len(cancelItems), err)
	}
	if err := orchestrator.AssignTarget(ctx, "tenant-cancel", cancelItems[0].ID, "user-successor"); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.DispatchOnce(ctx, "tenant-cancel", batch.ID, 5); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.CancelByRequest(ctx, "tenant-cancel", "request-cancel"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := repository.GetBatchByID(ctx, "tenant-cancel", batch.ID)
	if err != nil || cancelled.Status != application.HandoverBatchCancelled {
		t.Fatalf("batch = %+v (err=%v)", cancelled, err)
	}
	if len(system.outcomes) != 1 || system.outcomes[0] != "CANCELLED" {
		t.Fatalf("system outcomes = %v, want CANCELLED", system.outcomes)
	}
	items, err := repository.ListItems(ctx, "tenant-cancel", "request-cancel")
	if err != nil || len(items) != 1 || items[0].Status != application.HandoverItemTransferred {
		t.Fatalf("completed transfers must be preserved on cancellation: %+v (err=%v)", items, err)
	}
}

func TestHandoverEndToEndBlockedSystemCannotDispatchOrCompleteOnMySQL(t *testing.T) {
	system := newFakeHandoverSystem("c-1")
	server := httptest.NewServer(system)
	defer server.Close()
	orchestrator, repository, _ := handoverEndToEndService(t, server, application.HandoverSystemLicenseDenied)
	ctx := context.Background()

	batch, err := orchestrator.Start(ctx, "tenant-blocked", "request-blocked", "user-leaver")
	if err != nil {
		t.Fatal(err)
	}
	if batch.Status != application.HandoverBatchBlocked {
		t.Fatalf("license-denied system must block the batch: %+v", batch)
	}
	if system.frozen {
		t.Fatal("a blocking system must not be frozen")
	}
	if _, err := orchestrator.DispatchOnce(ctx, "tenant-blocked", batch.ID, 5); err == nil {
		t.Fatal("dispatch must be refused for a blocked batch")
	}
	if err := orchestrator.Complete(ctx, "tenant-blocked", batch.ID); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("completion err = %v, want conflict", err)
	}
	items, err := repository.ListItems(ctx, "tenant-blocked", "request-blocked")
	if err != nil || len(items) != 0 {
		t.Fatalf("blocked batch must not discover responsibilities: %+v (err=%v)", items, err)
	}
}

// Real MySQL persists the personnel transaction and completion queue; only the remote
// subsystem is represented by the local HTTP protocol fixture.
func TestHandoverTerminationFreezeLifecycleAndCompletionRetryOnMySQL(t *testing.T) {
	system := newFakeHandoverSystem("contract-lifecycle")
	server := httptest.NewServer(system)
	defer server.Close()
	orchestrator, repository, database := handoverEndToEndService(t, server, "")
	const tenant = "tenant-freeze-life"
	seedActiveHandoverUser(t, database, tenant, "user-leaver")
	seedActiveHandoverUser(t, database, tenant, "user-successor")
	now := time.Now().UTC()
	requestID, err := (ulid.Generator{}).New(now)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Table("iam_personnel_change_request").Create(map[string]any{
		"id": requestID, "tenant_id": tenant, "user_id": "user-leaver", "change_type": domain.PersonnelChangeTermination,
		"status": domain.PersonnelChangePendingApproval, "reason": "MySQL freeze lifecycle regression", "effective_at": now.Add(time.Hour),
		"submitted_by": "applicant", "version": 1, "created_at": now, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Exec("DELETE FROM iam_personnel_change_transition WHERE tenant_id = ? AND request_id = ?", tenant, requestID).Error; err != nil {
			t.Errorf("clean transitions: %v", err)
		}
		if err := database.Exec("DELETE FROM iam_personnel_change_request WHERE tenant_id = ? AND id = ?", tenant, requestID).Error; err != nil {
			t.Errorf("clean request: %v", err)
		}
	})
	checker, err := infrastructure.NewPersonnelHandoverGORMChecker(database)
	if err != nil {
		t.Fatal(err)
	}
	personnel := infrastructure.NewPersonnelChangeGORMRepository(database)
	service, err := application.NewPersonnelChangeService(personnel, ulid.Generator{}, application.SystemClock{}, checker)
	if err != nil {
		t.Fatal(err)
	}
	service.SetHandoverCoordinator(orchestrator)
	ctx := context.Background()
	transition := application.PersonnelChangeTransitionInput{TenantID: tenant, ID: requestID, OperatorID: "approver", ToStatus: domain.PersonnelChangePendingHandover, ApprovalReference: "APR-LIFECYCLE"}
	if _, err := service.Transition(ctx, transition); err != nil {
		t.Fatal(err)
	}
	items, err := repository.ListItems(ctx, tenant, requestID)
	if err != nil || len(items) != 2 {
		t.Fatalf("manual and remote responsibility items=%+v err=%v", items, err)
	}
	for _, item := range items {
		completed, err := service.CompleteHandoverItem(ctx, tenant, requestID, item.ID, "user-successor", "approver")
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status != "COMPLETED" && completed.Status != application.HandoverItemTransferred {
			t.Fatalf("unresolved item=%+v", completed)
		}
	}
	transition.ToStatus, transition.ApprovalReference = domain.PersonnelChangeScheduled, ""
	if _, err := service.Transition(ctx, transition); err != nil {
		t.Fatal(err)
	}
	assertFrozen := func(wantPersonnel string) application.HandoverBatch {
		t.Helper()
		stored, err := personnel.Get(ctx, tenant, requestID)
		if err != nil || stored.Status != wantPersonnel {
			t.Fatalf("personnel status=%+v err=%v", stored, err)
		}
		batch, err := repository.GetBatch(ctx, tenant, requestID)
		if err != nil || batch.Status != application.HandoverBatchFrozen {
			t.Fatalf("batch must retain pending completion: %+v err=%v", batch, err)
		}
		system.mutex.Lock()
		frozen, outcomes := system.frozen, len(system.outcomes)
		system.mutex.Unlock()
		if !frozen || outcomes != 0 {
			t.Fatal("freeze released before successful completion")
		}
		return batch
	}
	batch := assertFrozen(domain.PersonnelChangeScheduled)
	if pending, err := repository.ListPendingCompletionBatches(ctx, "", 1); err != nil || len(pending) != 0 {
		t.Fatalf("scheduled termination entered completion queue: %+v err=%v", pending, err)
	}
	transition.ToStatus = domain.PersonnelChangeExecuted
	if _, err := service.Transition(ctx, transition); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("future termination executed: %v", err)
	}
	assertFrozen(domain.PersonnelChangeScheduled)
	// Fail an actual GORM write inside Execute, so the MySQL transaction must roll back.
	injected := errors.New("injected termination user update failure")
	const callback = "handover_test:fail_termination_update"
	if err := database.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "iam_user" {
			_ = tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	transition.Immediate = true
	_, executeErr := service.Transition(ctx, transition)
	if err := database.Callback().Update().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(executeErr, injected) {
		t.Fatalf("expected transaction failure: %v", executeErr)
	}
	assertFrozen(domain.PersonnelChangeScheduled)
	var user struct{ Status, EmploymentStatus string }
	if err := database.Table("iam_user").Where("tenant_id = ? AND id = ?", tenant, "user-leaver").Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Status != "ACTIVE" {
		t.Fatalf("failed execution changed user: %+v", user)
	}
	system.mutex.Lock()
	system.finishFailures = 1
	system.mutex.Unlock()
	result, err := service.Transition(ctx, transition)
	if err != nil || result.Status != domain.PersonnelChangeExecuted {
		t.Fatalf("committed termination returned a false failure: %+v err=%v", result, err)
	}
	assertFrozen(domain.PersonnelChangeExecuted)
	if err := database.Table("iam_user").Where("tenant_id = ? AND id = ?", tenant, "user-leaver").Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Status != "DISABLED" || user.EmploymentStatus != "TERMINATED" {
		t.Fatalf("termination did not persist: %+v", user)
	}
	pending, err := repository.ListPendingCompletionBatches(ctx, "", 1)
	if err != nil || len(pending) != 1 || pending[0].ID != batch.ID {
		t.Fatalf("durable completion queue=%+v err=%v", pending, err)
	}
	// Rebuild the orchestrator from the same database, without Start or rediscovery.
	restarted, err := application.NewHandoverOrchestrator(handoverRepository(t, database), handoverInventoryStub{}, map[string]application.HandoverSystemClient{"contract": handoverEndToEndClient(t, server)}, ulid.Generator{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := restarted.CompleteExecuted(ctx, 1); err != nil || completed != 1 {
		t.Fatalf("restart completion=%d err=%v", completed, err)
	}
	finished, err := repository.GetBatch(ctx, tenant, requestID)
	if err != nil || finished.Status != application.HandoverBatchCompleted {
		t.Fatalf("completion checkpoint=%+v err=%v", finished, err)
	}
	if pending, err := repository.ListPendingCompletionBatches(ctx, "", 1); err != nil || len(pending) != 0 {
		t.Fatalf("completed batch remains queued: %+v err=%v", pending, err)
	}
	if completed, err := restarted.CompleteExecuted(ctx, 1); err != nil || completed != 0 {
		t.Fatalf("completion replay=%d err=%v", completed, err)
	}
	system.mutex.Lock()
	defer system.mutex.Unlock()
	if system.frozen || len(system.outcomes) != 1 || system.outcomes[0] != "COMPLETED" || system.finishCalls != 2 || system.transferCalls != 1 || system.owners["contract-lifecycle"] != "user-successor" {
		t.Fatalf("unexpected remote lifecycle: frozen=%v outcomes=%v finish=%d transfers=%d owners=%v", system.frozen, system.outcomes, system.finishCalls, system.transferCalls, system.owners)
	}
}
