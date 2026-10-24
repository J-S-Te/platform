// Package observability 提供进程级结构化日志辅助能力。
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

// NewLogger 创建同时写标准输出与本地追加式日志文件的 JSON 日志器。
// 轮转与集中采集有意延后到可观测性模块处理，本包不重复造轮子。
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
