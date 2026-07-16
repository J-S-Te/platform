// Package observability provides process-level structured logging helpers.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

// NewLogger creates a JSON logger that writes to standard output and a local append-only log
// file. Rotation and centralized export are intentionally deferred to the observability module.
func NewLogger(cfg config.LoggingConfig, appName, environment string) (*slog.Logger, io.Closer, error) {
	if !strings.EqualFold(cfg.Format, "json") {
		return nil, nil, fmt.Errorf("LOG_FORMAT %q is not supported; only json is supported", cfg.Format)
	}

	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, nil, err
	}

	if err := os.MkdirAll(cfg.Directory, 0o750); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}

	file, err := os.OpenFile(filepath.Join(cfg.Directory, "application.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("open application log file: %w", err)
	}

	handler := slog.NewJSONHandler(io.MultiWriter(os.Stdout, file), &slog.HandlerOptions{Level: level})
	logger := slog.New(handler).With("service", appName, "environment", environment)
	return logger, file, nil
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL %q is invalid", value)
	}
}
