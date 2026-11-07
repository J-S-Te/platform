// Command api 启动基础平台 HTTP 进程，并在 SIGINT/SIGTERM 到达时完成有界优雅停机。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/bootstrap"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/shutdown"
)

// apiDrainBudget 是“停止接收 + 排空在途请求”的总上限，沿用整改前的 15s。
const apiDrainBudget = 15 * time.Second

func main() {
	// 只读配置校验：复用 API 的同一套解析/校验逻辑，不启动 HTTP、数据库或后台任务。
	if len(os.Args) == 2 && os.Args[1] == "config-check" {
		if _, err := config.Load(); err != nil {
			slog.Error("configuration invalid", "error", err)
			os.Exit(1)
		}
		fmt.Println("configuration valid")
		return
	}
	if err := run(); err != nil {
		slog.Error("api process stopped with an error", "error", err)
		os.Exit(1)
	}
}

// run 启动 HTTP 服务，并用 shutdown.Manager 编排“停止接收 → 排空在途 → 释放依赖”的顺序。
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	app, err := bootstrap.NewAPI(cfg)
	if err != nil {
		return err
	}
	// 关闭顺序契约：HTTP 服务先停止接收并在预算内排空在途请求，
	// Manager 返回后才由这里释放数据库连接与日志文件。
	defer func() {
		app.Logger.Info("closing api dependencies after drain")
		app.Close()
	}()

	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           app.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 信号只负责触发排空；具体顺序与预算由 shutdown.Manager 统一编排。
	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(shutdownSignal)

	drainContext, cancelDrain := context.WithCancel(context.Background())
	defer cancelDrain()

	manager := shutdown.New(apiDrainBudget).WithLogger(app.Logger)
	manager.Register(shutdown.Hook{
		Name:   "http-server",
		Budget: apiDrainBudget,
		Run: func(hookContext context.Context) error {
			// draining 摘要：进程正在停止接收新请求，并在预算内等待在途请求完成。
			app.Logger.Info("api draining", "address", cfg.HTTP.Addr, "in_flight_budget", apiDrainBudget.String())
			return server.Shutdown(hookContext)
		},
	})

	serverErrors := make(chan error, 1)
	// 监听协程由 Manager 跟踪：HTTP Shutdown 结束后，Manager 会等它真正返回再让进程退出。
	// 缓冲槽避免 Shutdown 路径不读取 serverErrors 时阻塞该协程。
	manager.Go("http-listener", func(context.Context) {
		app.Logger.Info("http server started", "address", cfg.HTTP.Addr, "environment", cfg.Environment)
		serverErrors <- server.ListenAndServe()
	})

	drained := make(chan error, 1)
	go func() {
		drained <- manager.Run(drainContext)
	}()

	var serveErr error
	select {
	case serveErr = <-serverErrors:
		// 监听提前失败（例如端口被占用）：同样走排空路径，不跳过依赖释放。
		cancelDrain()
	case signalValue := <-shutdownSignal:
		app.Logger.Info("shutdown signal received", "signal", signalValue.String())
		cancelDrain()
	}

	drainErr := <-drained
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	// 排空失败（在途请求未能在预算内完成、钩子报错）必须交给进程退出码，不能静默吞掉。
	return drainErr
}
