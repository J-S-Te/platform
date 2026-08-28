package bootstrap

import (
	"log/slog"
	"io"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
