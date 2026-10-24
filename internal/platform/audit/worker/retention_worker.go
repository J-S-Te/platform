package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
)

// RetentionWorker 运行从 MySQL 领取的受控审计归档与保留期清理任务。不依赖 Redis：
// ClaimRetentionTask 使用数据库行锁与失联锁恢复。
type RetentionWorker struct {
	service          *application.RetentionService
	logger           *slog.Logger
	workerID         string
	pollInterval     time.Duration
	staleLockTimeout time.Duration
}

// NewRetentionWorker 在开始轮询前校验 worker 依赖与时间参数。
func NewRetentionWorker(service *application.RetentionService, logger *slog.Logger, workerID string, pollInterval, staleLockTimeout time.Duration) (*RetentionWorker, error) {
	if service == nil || logger == nil {
		return nil, errors.New("audit retention worker dependencies must not be nil")
	}
	if strings.TrimSpace(workerID) == "" || pollInterval <= 0 || staleLockTimeout <= 0 {
		return nil, errors.New("audit retention worker configuration is invalid")
	}
	return &RetentionWorker{service: service, logger: logger, workerID: strings.TrimSpace(workerID), pollInterval: pollInterval, staleLockTimeout: staleLockTimeout}, nil
}

// Run 轮询 MySQL 直到被取消。单个任务的失败由 RetentionService 持久化，
// 不会阻止后续审计保留任务被领取。
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

// ProcessOne 领取并处理至多一个保留任务。bool 报告本 worker 是否领取了任务，
// 包括随后失败并被持久化为失败状态的任务。
func (worker *RetentionWorker) ProcessOne(ctx context.Context) bool {
	claimed, err := worker.service.RunOnce(ctx, worker.workerID, time.Now().UTC().Add(-worker.staleLockTimeout))
	if err != nil {
		worker.logger.Error("process audit retention task", "error", err)
	}
	return claimed
}
