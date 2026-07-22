// Package worker contains independently composable observability background jobs.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/observability/domain"
)

// AlertExecutor is the narrow application dependency required by the periodic alert job.
// It deliberately does not depend on the notification implementation or a process bootstrap.
type AlertExecutor interface {
	ExecuteEnabled(context.Context) ([]domain.AlertExecution, error)
}

// AlertRunner periodically evaluates enabled metric alert rules. The caller owns its lifecycle;
// this package does not start goroutines during construction.
type AlertRunner struct {
	executor AlertExecutor
	logger   *slog.Logger
	interval time.Duration
}

// NewAlertRunner creates a runner with an explicit execution interval. One second is accepted to
// support deterministic local tests, while production wiring should use at least one minute.
func NewAlertRunner(executor AlertExecutor, logger *slog.Logger, interval time.Duration) (*AlertRunner, error) {
	if executor == nil || logger == nil {
		return nil, errors.New("alert runner dependencies must not be nil")
	}
	if interval < time.Second {
		return nil, errors.New("alert runner interval must be at least one second")
	}
	return &AlertRunner{executor: executor, logger: logger, interval: interval}, nil
}

// Run evaluates once at startup and then at the configured interval until context cancellation.
// Failed passes are logged and retried on the next tick; an alert evaluation failure must not stop
// unrelated platform workers.
func (runner *AlertRunner) Run(ctx context.Context) {
	runner.ExecuteOnce(ctx)

	ticker := time.NewTicker(runner.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runner.ExecuteOnce(ctx)
		}
	}
}

// ExecuteOnce runs a bounded enabled-rule evaluation pass and writes only operational summaries to
// the process log. It never logs metric values, tokens, request payloads or notification content.
func (runner *AlertRunner) ExecuteOnce(ctx context.Context) {
	executions, err := runner.executor.ExecuteEnabled(ctx)
	if err != nil {
		runner.logger.Error("observability alert evaluation pass failed", "error", err)
		return
	}

	var triggered, recovered, suppressed int
	for _, execution := range executions {
		if execution.Triggered {
			triggered++
		}
		if execution.Recovered {
			recovered++
		}
		if execution.Suppressed {
			suppressed++
		}
	}
	runner.logger.Info("observability alert evaluation pass completed",
		"rule_count", len(executions),
		"triggered_count", triggered,
		"recovered_count", recovered,
		"suppressed_count", suppressed,
	)
}
