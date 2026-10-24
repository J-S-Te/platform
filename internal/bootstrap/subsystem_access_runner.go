package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// subsystemAccessDriveWorker 是 Worker 循环需要的最小能力（便于用桩件验证生命周期）。
type subsystemAccessDriveWorker interface {
	DriveClaimable(ctx context.Context, limit int) (int, error)
}

// subsystemAccessRunner 把统一接入的持久执行器接入平台 Worker 生命周期：
// 周期领取可执行操作并推进阶段，随进程取消信号优雅退出。
// 说明：单个操作内部的领取/续租/阶段推进由应用层 Worker 负责；这里只做调度。
type subsystemAccessRunner struct {
	worker   subsystemAccessDriveWorker
	logger   *slog.Logger
	interval time.Duration
	batch    int
}

// newSubsystemAccessRunner 构造运行循环；依赖或参数非法即失败。
func newSubsystemAccessRunner(worker subsystemAccessDriveWorker, logger *slog.Logger, interval time.Duration, batch int) (*subsystemAccessRunner, error) {
	if worker == nil || logger == nil {
		return nil, errors.New("subsystem access runner requires worker and logger")
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if batch <= 0 {
		batch = 20
	}
	return &subsystemAccessRunner{worker: worker, logger: logger, interval: interval, batch: batch}, nil
}

// Run 周期调度，直到 context 取消（进程优雅退出）。
// 单次调度失败只记录日志，不终止循环：失败的操作由租约机制在后续轮次重新领取。
func (runner *subsystemAccessRunner) Run(ctx context.Context) {
	// 运维可见性：调度循环启动时留一行日志，便于确认 Worker 是否在运行。
	runner.logger.Info("subsystem access runner started", "interval", runner.interval.String(), "batch", runner.batch)
	ticker := time.NewTicker(runner.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return
			}
			handled, err := runner.worker.DriveClaimable(ctx, runner.batch)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				runner.logger.Warn("subsystem access drive failed", "error", err)
				continue
			}
			if handled > 0 {
				runner.logger.Info("subsystem access operations advanced", "handled", handled)
			}
		}
	}
}
