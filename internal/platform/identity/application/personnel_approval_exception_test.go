package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// approvalOutboxStoreStub 模拟本地 outbox 表：已投递项从待投递集合移除，
// 重试只记录投递进度，不改变审批状态。
type approvalOutboxStoreStub struct {
	entries          []ApprovalExceptionOutboxEntry
	delivered        []string
	retries          []approvalOutboxRetry
	listErr          error
	markDeliveredErr error
	markRetryErr     error
}

type approvalOutboxRetry struct {
	eventID       string
	attempts      int
	nextAttemptAt time.Time
	lastError     string
}

func (stub *approvalOutboxStoreStub) UpdateStatusWithApprovalException(context.Context, PersonnelChangeRequest, string, string, string, time.Time, PersonnelChangeAuditEvent) (PersonnelChangeRequest, error) {
	return PersonnelChangeRequest{}, errors.New("unexpected transition write")
}

func (stub *approvalOutboxStoreStub) PendingApprovalExceptions(_ context.Context, _ time.Time, limit int) ([]ApprovalExceptionOutboxEntry, error) {
	if stub.listErr != nil {
		return nil, stub.listErr
	}
	if limit <= 0 || limit > len(stub.entries) {
		limit = len(stub.entries)
	}
	return append([]ApprovalExceptionOutboxEntry(nil), stub.entries[:limit]...), nil
}

func (stub *approvalOutboxStoreStub) MarkApprovalExceptionDelivered(_ context.Context, tenantID, eventID string, _ time.Time) error {
	if stub.markDeliveredErr != nil {
		return stub.markDeliveredErr
	}
	for index, entry := range stub.entries {
		if entry.Event.TenantID == tenantID && entry.Event.EventID == eventID {
			stub.entries = append(stub.entries[:index], stub.entries[index+1:]...)
			stub.delivered = append(stub.delivered, eventID)
			return nil
		}
	}
	return ErrNotFound
}

func (stub *approvalOutboxStoreStub) MarkApprovalExceptionRetry(_ context.Context, _ string, eventID string, attempts int, nextAttemptAt time.Time, lastError string, _ time.Time) error {
	if stub.markRetryErr != nil {
		return stub.markRetryErr
	}
	stub.retries = append(stub.retries, approvalOutboxRetry{eventID: eventID, attempts: attempts, nextAttemptAt: nextAttemptAt, lastError: lastError})
	return nil
}

// scriptedAuditSink 允许前若干次投递失败，用于验证重试而不是同步回滚审批。
type scriptedAuditSink struct {
	failures int
	events   []PersonnelChangeAuditEvent
}

func (sink *scriptedAuditSink) RecordApprovalException(_ context.Context, event PersonnelChangeAuditEvent) error {
	if sink.failures > 0 {
		sink.failures--
		return errors.New("audit endpoint unavailable")
	}
	sink.events = append(sink.events, event)
	return nil
}

func approvalExceptionEvent(eventID string) PersonnelChangeAuditEvent {
	return PersonnelChangeAuditEvent{
		EventID: eventID, TenantID: "tenant-1", OperatorID: "applicant-1", RequestID: "change-1",
		ToStatus: domain.PersonnelChangeScheduled, SubmittedBy: "applicant-1", SubjectUserID: "user-1",
		Reason: "唯一在岗审批人，已按例外流程复核", RuleHit: "SELF_APPROVAL_EXCEPTION",
		ApprovalReference: "APR-1001", OccurredAt: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
	}
}

// 已持久化的例外审计事件必须先投递成功再标记完成；已完成的事件不会再次投递。
func TestApprovalExceptionDispatcherDeliversPersistedOutbox(t *testing.T) {
	store := &approvalOutboxStoreStub{entries: []ApprovalExceptionOutboxEntry{{Event: approvalExceptionEvent("01JAUDIT000000000000000001")}}}
	sink := &scriptedAuditSink{}
	dispatcher, err := NewApprovalExceptionDispatcher(store, sink, personnelChangeLifecycleClock{now: time.Date(2026, 10, 22, 10, 5, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}

	report, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Inspected != 1 || report.Delivered != 1 || report.Failed != 0 {
		t.Fatalf("report = %+v, want 1 inspected / 1 delivered", report)
	}
	if len(sink.events) != 1 || sink.events[0].EventID != "01JAUDIT000000000000000001" || sink.events[0].Reason == "" {
		t.Fatalf("delivered events = %+v", sink.events)
	}
	if len(store.delivered) != 1 || store.delivered[0] != "01JAUDIT000000000000000001" {
		t.Fatalf("delivered markers = %v", store.delivered)
	}

	// 幂等：已标记 DELIVERED 的事件再次轮询不应重复投递。
	again, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if again.Inspected != 0 || again.Delivered != 0 || len(sink.events) != 1 {
		t.Fatalf("second dispatch = %+v events=%d, want no redelivery", again, len(sink.events))
	}
}

// 审计端失败时审批结果已经持久化，只能登记重试，不能把事件丢掉或回滚已提交的审批。
func TestApprovalExceptionDispatcherSchedulesRetryOnAuditFailure(t *testing.T) {
	store := &approvalOutboxStoreStub{entries: []ApprovalExceptionOutboxEntry{{Event: approvalExceptionEvent("01JAUDIT000000000000000002"), Attempts: 2}}}
	sink := &scriptedAuditSink{failures: 1}
	now := time.Date(2026, 10, 22, 10, 5, 0, 0, time.UTC)
	dispatcher, err := NewApprovalExceptionDispatcher(store, sink, personnelChangeLifecycleClock{now: now})
	if err != nil {
		t.Fatal(err)
	}

	report, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Inspected != 1 || report.Delivered != 0 || report.Failed != 1 {
		t.Fatalf("report = %+v, want 1 inspected / 1 failed", report)
	}
	if len(store.delivered) != 0 {
		t.Fatalf("failed deliveries must not be marked DELIVERED: %v", store.delivered)
	}
	if len(store.retries) != 1 {
		t.Fatalf("retry records = %d, want one", len(store.retries))
	}
	retry := store.retries[0]
	if retry.eventID != "01JAUDIT000000000000000002" || retry.attempts != 3 {
		t.Fatalf("retry = %+v, want event id and attempts incremented to 3", retry)
	}
	if !retry.nextAttemptAt.After(now) {
		t.Fatalf("next attempt %v must be after %v", retry.nextAttemptAt, now)
	}
	if !strings.Contains(retry.lastError, "audit endpoint unavailable") {
		t.Fatalf("last error = %q", retry.lastError)
	}

	// 重试成功后使用同一个事件 ID，接收端按幂等键去重。
	if _, err := dispatcher.DispatchOnce(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 || sink.events[0].EventID != "01JAUDIT000000000000000002" {
		t.Fatalf("events after retry = %+v, want the same stable event id", sink.events)
	}
	if len(store.delivered) != 1 {
		t.Fatalf("delivered markers = %v, want one after retry", store.delivered)
	}
}

// 投递标记失败时事件保持待投递；下一次投递仍使用同一事件 ID，审计端可幂等去重。
func TestApprovalExceptionDispatcherKeepsPendingWhenDeliveredMarkFails(t *testing.T) {
	store := &approvalOutboxStoreStub{
		entries:          []ApprovalExceptionOutboxEntry{{Event: approvalExceptionEvent("01JAUDIT000000000000000003")}},
		markDeliveredErr: errors.New("database unavailable"),
	}
	sink := &scriptedAuditSink{}
	dispatcher, err := NewApprovalExceptionDispatcher(store, sink, personnelChangeLifecycleClock{now: time.Date(2026, 10, 22, 10, 5, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	report, err := dispatcher.DispatchOnce(context.Background(), 10)
	if err == nil {
		t.Fatal("mark failure must be reported so the worker logs it")
	}
	if report.Delivered != 0 || report.Failed != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(store.entries) != 1 {
		t.Fatalf("pending entries = %d, want the event to remain deliverable", len(store.entries))
	}
}

func TestApprovalExceptionDispatcherRequiresDependencies(t *testing.T) {
	if _, err := NewApprovalExceptionDispatcher(nil, &scriptedAuditSink{}, personnelChangeLifecycleClock{}); err == nil {
		t.Fatal("dispatcher must reject a nil store")
	}
	if _, err := NewApprovalExceptionDispatcher(&approvalOutboxStoreStub{}, nil, personnelChangeLifecycleClock{}); err == nil {
		t.Fatal("dispatcher must reject a nil audit sink")
	}
}
