// Package worker consumes durable Keycloak authorization projection events.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type Event struct {
	ID, TenantID, IdentityID, ApplicationID, EnvironmentID, EventType string
	Attempts                                                          uint
}

type Queue interface {
	RecoverStale(context.Context, time.Time, time.Time) error
	Claim(context.Context, string, time.Time) (Event, bool, error)
	Complete(context.Context, Event) error
	Retry(context.Context, Event, string, string, time.Time) error
}

// Synchronizer owns the Keycloak Admin API calls. It must create/update the
// stable identity_id user mapping, organization groups and only the target
// application's Client Roles.
type Synchronizer interface {
	SyncAuthorization(context.Context, Event) error
}

type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type Worker struct {
	queue        Queue
	synchronizer Synchronizer
	logger       *slog.Logger
	workerID     string
	poll         time.Duration
	clock        Clock
	// StaleLockTimeout is the maximum time an event may remain RUNNING before
	// another worker assumes its owner crashed and returns it to PENDING.
	StaleLockTimeout time.Duration
}

func New(queue Queue, synchronizer Synchronizer, logger *slog.Logger, workerID string, poll time.Duration, clocks ...Clock) (*Worker, error) {
	if queue == nil || synchronizer == nil || logger == nil || strings.TrimSpace(workerID) == "" || poll <= 0 || len(clocks) > 1 {
		return nil, errors.New("Keycloak authorization worker configuration is invalid")
	}
	clock := Clock(systemClock{})
	if len(clocks) == 1 {
		if clocks[0] == nil {
			return nil, errors.New("Keycloak authorization worker clock must not be nil")
		}
		clock = clocks[0]
	}
	return &Worker{queue: queue, synchronizer: synchronizer, logger: logger, workerID: strings.TrimSpace(workerID), poll: poll, clock: clock, StaleLockTimeout: 5 * time.Minute}, nil
}

func (worker *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.poll)
	defer ticker.Stop()
	for {
		if err := worker.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			worker.logger.Error("Keycloak authorization projection cycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (worker *Worker) RunOnce(ctx context.Context) error {
	// 每轮先回收崩溃遗留锁，再完成一次“领取—投影—成功或退避重试”的闭环。
	now := worker.clock.Now().UTC()
	if worker.StaleLockTimeout <= 0 {
		return errors.New("Keycloak authorization worker stale lock timeout must be positive")
	}
	if err := worker.queue.RecoverStale(ctx, now.Add(-worker.StaleLockTimeout), now); err != nil {
		return fmt.Errorf("recover stale Keycloak authorization outbox: %w", err)
	}
	event, claimed, err := worker.queue.Claim(ctx, worker.workerID, now)
	if err != nil || !claimed {
		return err
	}
	if err := worker.synchronizer.SyncAuthorization(ctx, event); err != nil {
		retryAt := worker.clock.Now().UTC().Add(retryDelay(event.Attempts))
		if retryErr := worker.queue.Retry(ctx, event, "KEYCLOAK_SYNC_FAILED", trimError(err), retryAt); retryErr != nil {
			return fmt.Errorf("sync Keycloak authorization: %v; schedule retry: %w", err, retryErr)
		}
		return nil
	}
	return worker.queue.Complete(ctx, event)
}

func retryDelay(attempts uint) time.Duration {
	if attempts > 6 {
		attempts = 6
	}
	return time.Second * time.Duration(1<<attempts)
}

func trimError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 1000 {
		return message[:1000]
	}
	return message
}
