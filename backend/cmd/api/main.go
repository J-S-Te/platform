// Command api 启动基础平台 HTTP 进程，并在 SIGINT/SIGTERM 到达时完成有界优雅停机。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/bootstrap"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api process stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	app, err := bootstrap.NewAPI(cfg)
	if err != nil {
		return err
	}
	defer app.Close()

	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           app.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	// 监听必须放入独立协程，主协程才能同时等待进程信号和立即发生的监听失败。
	// 缓冲槽避免 Shutdown 路径退出时 ListenAndServe 的返回值阻塞协程。
	go func() {
		app.Logger.Info("http server started", "address", cfg.HTTP.Addr, "environment", cfg.Environment)
		serverErrors <- server.ListenAndServe()
	}()

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case signalValue := <-shutdownSignal:
		app.Logger.Info("shutdown signal received", "signal", signalValue.String())
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Shutdown 停止接收新连接并等待在途请求；超时后把错误交给进程管理器处理，
	// 防止发布过程无限等待一个失联客户端。
	if err := server.Shutdown(shutdownContext); err != nil {
		return err
	}

	return nil
}
