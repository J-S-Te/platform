// Command worker 运行以 MySQL 租约协调的异步任务；进程信号通过同一 context 传给全部后台循环。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/J-S-Te/Basic-Platform/internal/bootstrap"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker process stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	app, err := bootstrap.NewWorker(cfg)
	if err != nil {
		return err
	}
	defer app.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	app.Logger.Info("asynchronous worker started", "worker_id", cfg.Worker.ID, "environment", cfg.Environment)
	app.Runner.Run(ctx)
	// 有界 join：预算内未退出的后台循环必须以非零退出码暴露，不能只留一行告警后静默退出 0。
	if err := app.JoinError(); err != nil {
		app.Logger.Error("asynchronous worker stopped before all runners drained", "worker_id", cfg.Worker.ID, "error", err)
		return err
	}
	app.Logger.Info("asynchronous worker stopped", "worker_id", cfg.Worker.ID)
	return nil
}
