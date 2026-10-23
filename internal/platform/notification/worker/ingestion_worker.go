// Package worker 运行持久化的通知摄取展开与到期投递重试任务。
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
)

// IngestionWorker 把已接收的集成事件展开为普通站内信投递，并重试已持久化的到期失败投递。
// 两个队列独立领取，采用数据库租约供多个 worker 实例并发轮询。
type IngestionWorker struct {
	service          *application.Service
	logger           *slog.Logger
	pollInterval     time.Duration
	staleLockTimeout time.Duration
}

// NewIngestionWorker 构造摄取 worker；配置非法时失败关闭。
func NewIngestionWorker(service *application.Service, logger *slog.Logger, pollInterval, staleLockTimeout time.Duration) (*IngestionWorker, error) {
	if service == nil || logger == nil || pollInterval <= 0 || staleLockTimeout <= 0 {
		return nil, errors.New("notification ingestion worker configuration is invalid")
	}
	return &IngestionWorker{service: service, logger: logger, pollInterval: pollInterval, staleLockTimeout: staleLockTimeout}, nil
}

// Run 按固定周期轮询处理摄取事件，直到 ctx 取消；单轮失败只记录日志不中断循环。
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

// Process 分别处理有界的摄取事件与到期投递（各上限 100）；一阶段失败不阻断另一阶段。
func (worker *IngestionWorker) Process(ctx context.Context) {
	if _, err := worker.service.ProcessIngestionBatch(ctx, 100, time.Now().UTC().Add(worker.staleLockTimeout)); err != nil {
		worker.logger.Error("process notification ingestion events", "error", err)
	}
	if _, err := worker.service.RetryDueDeliveries(ctx, 100); err != nil {
		worker.logger.Error("retry due notification deliveries", "error", err)
	}
}
