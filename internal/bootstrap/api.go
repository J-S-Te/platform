// Package bootstrap wires infrastructure dependencies into runnable processes.
package bootstrap

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/internal/shared/observability"
	httptransport "github.com/J-S-Te/Basic-Platform/internal/transport/http"
)

// API is the dependency container for the HTTP process.
type API struct {
	Handler http.Handler
	Logger  *slog.Logger

	database *sql.DB
	logFile  io.Closer
}

// NewAPI creates the local storage directories, structured logger, database pool and HTTP router.
// It deliberately does not ping MySQL during startup; /readyz reports dependency state while
// /healthz remains available for process liveness.
func NewAPI(cfg config.Config) (*API, error) {
	if err := os.MkdirAll(cfg.FileStorageRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create file storage root: %w", err)
	}

	logger, logFile, err := observability.NewLogger(cfg.Logging, cfg.AppName, cfg.Environment)
	if err != nil {
		return nil, err
	}

	db, err := database.OpenMySQL(cfg.MySQL)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}

	return &API{
		Handler:  httptransport.NewRouter(cfg, logger, db),
		Logger:   logger,
		database: db,
		logFile:  logFile,
	}, nil
}

// Close releases process-owned resources. It is safe to defer after a successful NewAPI call.
func (api *API) Close() {
	if api.database != nil {
		if err := api.database.Close(); err != nil {
			api.Logger.Error("close mysql database handle", "error", err)
		}
	}
	if api.logFile != nil {
		if err := api.logFile.Close(); err != nil {
			api.Logger.Error("close application log file", "error", err)
		}
	}
}
