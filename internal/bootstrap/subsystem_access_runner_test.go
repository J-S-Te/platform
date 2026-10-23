package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type subsystemAccessWorkerStub struct {
	mu      sync.Mutex
	calls   int
	handled int
	err     error
	limit   int
	once    chan struct{}
}

func (stub *subsystemAccessWorkerStub) DriveClaimable(_ context.Context, limit int) (int, error) {
	stub.mu.Lock()
	stub.calls++
	stub.limit = limit
	stub.mu.Unlock()
	if stub.once != nil {
		select {
		case stub.once <- struct{}{}:
		default:
		}
	}
	return stub.handled, stub.err
}

func (stub *subsystemAccessWorkerStub) callCount() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.calls
}

func subsystemAccessRunnerFor(t *testing.T, worker *subsystemAccessWorkerStub, interval time.Duration, batch int) *subsystemAccessRunner {
	t.Helper()
	runner, err := newSubsystemAccessRunner(worker, slog.New(slog.NewTextHandler(io.Discard, nil)), interval, batch)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// 循环按间隔调度，并在 context 取消后退出（不泄漏 goroutine）。
func TestSubsystemAccessRunnerDrivesUntilCancelled(t *testing.T) {
	worker := &subsystemAccessWorkerStub{handled: 2, once: make(chan struct{}, 1)}
	runner := subsystemAccessRunnerFor(t, worker, time.Millisecond, 7)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runner.Run(ctx); close(done) }()
	select {
	case <-worker.once:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not drive within the expected window")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop after cancellation")
	}
	if worker.callCount() == 0 || worker.limit != 7 {
		t.Fatalf("calls=%d limit=%d", worker.callCount(), worker.limit)
	}
}

// 单次调度失败不终止循环：后续轮次继续尝试（失败操作由租约机制重领）。
func TestSubsystemAccessRunnerKeepsRunningAfterDriveError(t *testing.T) {
	worker := &subsystemAccessWorkerStub{err: errors.New("database unavailable"), once: make(chan struct{}, 1)}
	runner := subsystemAccessRunnerFor(t, worker, time.Millisecond, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { runner.Run(ctx); close(done) }()
	<-worker.once
	deadline := time.Now().Add(time.Second)
	for worker.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	if worker.callCount() < 2 {
		t.Fatalf("runner stopped after an error: calls=%d", worker.callCount())
	}
	if worker.limit != 20 {
		t.Fatalf("default batch = %d, want 20", worker.limit)
	}
}

func TestSubsystemAccessRunnerRequiresDependencies(t *testing.T) {
	if _, err := newSubsystemAccessRunner(nil, slog.Default(), time.Second, 1); err == nil {
		t.Fatal("nil worker must fail construction")
	}
	if _, err := newSubsystemAccessRunner(&subsystemAccessWorkerStub{}, nil, time.Second, 1); err == nil {
		t.Fatal("nil logger must fail construction")
	}
}
