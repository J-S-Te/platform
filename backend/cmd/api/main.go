// Command api starts the Basic Platform HTTP API process.
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

	"github.com/J-S-Te/Basic-Platform/internal/bootstrap"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
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

	if err := server.Shutdown(shutdownContext); err != nil {
		return err
	}

	return nil
}
