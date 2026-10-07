package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// subsystemDeploymentSweepStore 组合看护任务需要的两个能力：枚举在途部署 + 写状态机。
// 生产实现是 gorm 仓储；测试可以用任何满足组合接口的假件。
type subsystemDeploymentSweepStore interface {
	application.SubsystemDeploymentInFlightLister
	application.SubsystemDeploymentStateStore
}

// subsystemDeploymentSweeper 周期枚举在途部署状态，把超过 stale 阈值、已无人认领的编排
// （API 进程重启/崩溃、编排协程异常死亡遗留）收口为 DEPLOYMENT_INTERRUPTED。此前该恢复
// 只由管理端状态查询触发：没有访问就没有恢复，状态会永久停留在“更新中”并拒绝一切
// 受控操作。收口后操作者可以直接重试，不需要任何数据库手工干预。
type subsystemDeploymentSweeper struct {
	store      subsystemDeploymentSweepStore
	logger     *slog.Logger
	poll       time.Duration
	staleAfter time.Duration
}

func newSubsystemDeploymentSweeper(store subsystemDeploymentSweepStore, logger *slog.Logger, poll, staleAfter time.Duration) (*subsystemDeploymentSweeper, error) {
	if store == nil || logger == nil || poll <= 0 || staleAfter <= 0 {
		return nil, errors.New("subsystem deployment sweeper configuration is invalid")
	}
	return &subsystemDeploymentSweeper{store: store, logger: logger, poll: poll, staleAfter: staleAfter}, nil
}

// Run 以固定周期清扫；退出依赖 ctx 取消，与 worker 进程的其他后台循环一致。
func (runner *subsystemDeploymentSweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(runner.poll)
	defer ticker.Stop()
	for {
		runner.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (runner *subsystemDeploymentSweeper) sweep(ctx context.Context) {
	states, err := runner.store.ListInFlightSubsystemDeployments(ctx)
	if err != nil {
		runner.logger.Error("list in-flight subsystem deployments", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, state := range states {
		if state.StartedAt == nil || now.Sub(state.StartedAt.UTC()) <= runner.staleAfter {
			continue
		}
		// 单行写限制时间；Worker 停止时仍遵守进程取消信号。
		writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		recovered, err := runner.store.RecoverStaleSubsystemDeployment(writeCtx, state, now.Add(-runner.staleAfter), now)
		cancel()
		if err != nil {
			runner.logger.Warn("stale subsystem deployment could not be recovered",
				"application_code", state.ApplicationCode, "environment", state.Environment, "error", err)
			continue
		}
		if !recovered {
			continue
		}
		runner.logger.Warn("stale subsystem deployment recovered by sweeper",
			"application_code", state.ApplicationCode, "environment", state.Environment,
			"generation", state.Generation)
	}
}
