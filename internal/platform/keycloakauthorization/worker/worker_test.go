package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type queueSpy struct {
	event       Event
	claimed     bool
	completed   bool
	retried     bool
	recovered   bool
	staleBefore time.Time
	availableAt time.Time
	recoverErr  error
}

func (queue *queueSpy) RecoverStale(_ context.Context, staleBefore, availableAt time.Time) error {
	queue.recovered = true
	queue.staleBefore = staleBefore
	queue.availableAt = availableAt
	return queue.recoverErr
}
func (queue *queueSpy) Claim(context.Context, string, time.Time) (Event, bool, error) {
	return queue.event, queue.claimed, nil
}
func (queue *queueSpy) Complete(context.Context, Event) error { queue.completed = true; return nil }
func (queue *queueSpy) Retry(context.Context, Event, string, string, time.Time) error {
	queue.retried = true
	return nil
}

type synchronizerStub struct{ err error }

func (sync synchronizerStub) SyncAuthorization(context.Context, Event) error { return sync.err }

func TestRunOnceCompletesSuccessfulProjection(t *testing.T) {
	queue := &queueSpy{event: Event{ID: "event-1"}, claimed: true}
	worker, err := New(queue, synchronizerStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Second, fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.completed || queue.retried {
		t.Fatalf("queue state completed=%v retried=%v", queue.completed, queue.retried)
	}
}

func TestRunOnceSchedulesRetryForFailedProjection(t *testing.T) {
	queue := &queueSpy{event: Event{ID: "event-1"}, claimed: true}
	worker, err := New(queue, synchronizerStub{err: errors.New("Keycloak unavailable")}, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Second, fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if queue.completed || !queue.retried {
		t.Fatalf("queue state completed=%v retried=%v", queue.completed, queue.retried)
	}
}

func TestRunOnceRecoversOnlyExpiredLocksBeforeClaiming(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	queue := &queueSpy{}
	worker, err := New(queue, synchronizerStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Second, fixedClock{now: now})
	if err != nil {
		t.Fatal(err)
	}
	worker.StaleLockTimeout = 2 * time.Minute
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.recovered {
		t.Fatal("stale lock recovery was not requested")
	}
	if want := now.Add(-2 * time.Minute); !queue.staleBefore.Equal(want) {
		t.Fatalf("stale cutoff = %s, want %s", queue.staleBefore, want)
	}
	if !queue.availableAt.Equal(now) {
		t.Fatalf("recovered event availability = %s, want %s", queue.availableAt, now)
	}
}

func TestRunOnceDoesNotClaimWhenStaleRecoveryFails(t *testing.T) {
	queue := &queueSpy{claimed: true, recoverErr: errors.New("database unavailable")}
	worker, err := New(queue, synchronizerStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)), "worker-1", time.Second, fixedClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil")
	}
	if queue.completed || queue.retried {
		t.Fatalf("worker processed event after failed recovery: completed=%v retried=%v", queue.completed, queue.retried)
	}
}
