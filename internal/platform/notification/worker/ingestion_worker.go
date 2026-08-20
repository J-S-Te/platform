// Package worker runs durable notification-ingestion projections.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
)

// IngestionWorker turns accepted integration events into ordinary inbox deliveries. It uses
// database leases, so more than one worker instance can poll safely.
type IngestionWorker struct {
	service          *application.Service
	logger           *slog.Logger
	pollInterval     time.Duration
	staleLockTimeout time.Duration
}

func NewIngestionWorker(service *application.Service, logger *slog.Logger, pollInterval, staleLockTimeout time.Duration) (*IngestionWorker, error) {
	if service == nil || logger == nil || pollInterval <= 0 || staleLockTimeout <= 0 {
		return nil, errors.New("notification ingestion worker configuration is invalid")
	}
	return &IngestionWorker{service: service, logger: logger, pollInterval: pollInterval, staleLockTimeout: staleLockTimeout}, nil
}

func (worker *IngestionWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.pollInterval)
	defer ticker.Stop()
	for {
		worker.Process(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (worker *IngestionWorker) Process(ctx context.Context) {
	if _, err := worker.service.ProcessIngestionBatch(ctx, 100, time.Now().UTC().Add(worker.staleLockTimeout)); err != nil {
		worker.logger.Error("process notification ingestion events", "error", err)
	}
}
