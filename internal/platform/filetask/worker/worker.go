// Package worker 定时恢复 File Gateway 卡在中间状态的上传记录。
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
)

// TenantSource 提供当前租户快照；实现方必须只返回仍然有效的租户 ID。
type TenantSource interface {
	ListTenantIDs(context.Context) ([]string, error)
}

// Runner 是独立于 HTTP 进程的有界对账 Worker。
type Runner struct {
	files                *application.FileService
	tenants              TenantSource
	interval, staleAfter time.Duration
	limit                int
	logger               *slog.Logger
}

// NewRunner 创建对账 Worker，所有时间和批量参数均显式限制，避免异常配置形成无限扫描。
func NewRunner(files *application.FileService, tenants TenantSource, interval, staleAfter time.Duration, limit int, logger *slog.Logger) (*Runner, error) {
	if files == nil || tenants == nil || interval <= 0 || staleAfter <= 0 || limit < 1 || limit > 500 || logger == nil {
		return nil, application.ErrValidation
	}
	return &Runner{files: files, tenants: tenants, interval: interval, staleAfter: staleAfter, limit: limit, logger: logger}, nil
}

// Run 按固定周期执行有界对账；每轮都传递 context，收到停机信号后不再启动新租户扫描。
func (runner *Runner) Run(ctx context.Context) {
	runner.runOnce(ctx)
	ticker := time.NewTicker(runner.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runner.runOnce(ctx)
		}
	}
}

func (runner *Runner) runOnce(ctx context.Context) {
	tenantIDs, err := runner.tenants.ListTenantIDs(ctx)
	if err != nil {
		runner.logger.Error("file gateway reconciliation tenant listing failed", "error", err)
		return
	}
	cutoff := time.Now().UTC().Add(-runner.staleAfter)
	for _, tenantID := range tenantIDs {
		result, err := runner.files.ReconcileStaleUploads(ctx, tenantID, cutoff, runner.limit)
		if err != nil {
			runner.logger.Error("file gateway reconciliation failed", "tenant_id", tenantID, "error", err)
			continue
		}
		runner.logger.Info("file gateway reconciliation completed", "tenant_id", tenantID, "inspected", result.Inspected, "recovered", result.Recovered, "rejected", result.Rejected, "failed", result.Failed)
	}
}
