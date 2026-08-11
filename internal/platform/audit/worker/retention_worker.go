package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
)

// RetentionWorker runs controlled audit archive and retention-cleanup tasks claimed from MySQL.
// It does not depend on Redis: ClaimRetentionTask uses a database row lock and stale-lock recovery.
type RetentionWorker struct {
	service          *application.RetentionService
	logger           *slog.Logger
	workerID         string
	pollInterval     time.Duration
	staleLockTimeout time.Duration
}

// NewRetentionWorker validates worker dependencies and timing configuration before polling starts.
func NewRetentionWorker(service *application.RetentionService, logger *slog.Logger, workerID string, pollInterval, staleLockTimeout time.Duration) (*RetentionWorker, error) {
	if service == nil || logger == nil {
		return nil, errors.New("audit retention worker dependencies must not be nil")
	}
	if strings.TrimSpace(workerID) == "" || pollInterval <= 0 || staleLockTimeout <= 0 {
		return nil, errors.New("audit retention worker configuration is invalid")
	}
	return &RetentionWorker{service: service, logger: logger, workerID: strings.TrimSpace(workerID), pollInterval: pollInterval, staleLockTimeout: staleLockTimeout}, nil
}

// Run polls MySQL until cancellation. Per-task failures are persisted by RetentionService and do
// not stop later audit-retention jobs from being claimed.
func (worker *RetentionWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.pollInterval)
	defer ticker.Stop()
	for {
		worker.ProcessOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOne claims and processes at most one retention task. The bool reports whether this worker
// claimed a task, including a task that subsequently failed and was persisted as failed.
func (worker *RetentionWorker) ProcessOne(ctx context.Context) bool {
	claimed, err := worker.service.RunOnce(ctx, worker.workerID, time.Now().UTC().Add(-worker.staleLockTimeout))
	if err != nil {
		worker.logger.Error("process audit retention task", "error", err)
	}
	return claimed
}
