package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

type operationsStoreStub struct {
	query  FailurePageRequest
	replay ReplayInput
	when   time.Time
}

func (store *operationsStoreStub) ListFailedProjections(_ context.Context, _ string, query FailurePageRequest) (FailurePageResult, error) {
	store.query = query
	return FailurePageResult{Page: query.Page, PageSize: query.PageSize}, nil
}
func (store *operationsStoreStub) GetProjectionAlertStatus(context.Context, string) (AlertStatus, error) {
	return AlertStatus{}, nil
}
func (store *operationsStoreStub) ReplayFailedProjection(_ context.Context, input ReplayInput, when time.Time) (ReplayResult, error) {
	store.replay, store.when = input, when
	return ReplayResult{EventID: input.EventID, Replayed: true, AvailableAt: when}, nil
}

type fixedOperationsClock struct{ now time.Time }

func (clock fixedOperationsClock) Now() time.Time { return clock.now }

func TestOperationsNormalizesFailedProjectionPageAndReplayConfirmation(t *testing.T) {
	store := &operationsStoreStub{}
	service, err := NewOperations(store, fixedOperationsClock{now: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ListFailed(context.Background(), "tenant-1", FailurePageRequest{PageSize: 999, Environment: " PROD "})
	if err != nil || result.Page != 1 || result.PageSize != maxFailurePageSize || store.query.Environment != "prod" {
		t.Fatalf("normalized result=%#v query=%#v err=%v", result, store.query, err)
	}
	replay, err := service.Replay(context.Background(), ReplayInput{TenantID: "tenant-1", EventID: "event-1", OperatorID: "user-1", Confirmation: "event-1", Reason: "已确认目标 Client 恢复可用"})
	if err != nil || !replay.Replayed || store.replay.EventID != "event-1" || !store.when.Equal(time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("replay=%#v store=%#v err=%v", replay, store, err)
	}
}

func TestOperationsRejectsUnsafeReplay(t *testing.T) {
	service, err := NewOperations(&operationsStoreStub{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Replay(context.Background(), ReplayInput{TenantID: "tenant-1", EventID: "event-1", OperatorID: "user-1", Confirmation: "other", Reason: "short"})
	if !errors.Is(err, ErrOperationsValidation) {
		t.Fatalf("err=%v", err)
	}
}
