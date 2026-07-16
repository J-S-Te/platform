// Package bootstrap wires infrastructure dependencies into runnable processes.
package bootstrap

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	identityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/identity/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/database"
	"github.com/J-S-Te/Basic-Platform/internal/shared/observability"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
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

	tokenManager, err := security.LoadJWTManager(
		cfg.Auth.JWTIssuer, cfg.Auth.JWTAudience, cfg.Auth.JWTPrivateKeyPath, cfg.Auth.JWTPublicKeyPath,
	)
	if err != nil {
		_ = db.Close()
		_ = logFile.Close()
		return nil, fmt.Errorf("load JWT signing keys: %w", err)
	}
	repository, err := infrastructure.NewMySQLRepository(db)
	if err != nil {
		_ = db.Close()
		_ = logFile.Close()
		return nil, err
	}
	authService, err := application.NewService(
		repository, security.Argon2idPasswordVerifier{}, tokenManager, ulid.Generator{}, application.SystemClock{}, cfg.Auth.SessionTTL,
	)
	if err != nil {
		_ = db.Close()
		_ = logFile.Close()
		return nil, err
	}
	mobileProtector, err := security.NewMobileProtector(cfg.Identity.MobileEncryptionKey)
	if err != nil {
		_ = db.Close()
		_ = logFile.Close()
		return nil, err
	}
	managementService, err := application.NewManagementService(
		repository, mobileProtector, ulid.Generator{}, application.SystemClock{},
	)
	if err != nil {
		_ = db.Close()
		_ = logFile.Close()
		return nil, err
	}
	managementHandler, err := identityhttp.NewManagementHandler(managementService, logger)
	if err != nil {
		_ = db.Close()
		_ = logFile.Close()
		return nil, err
	}

	authHandler, err := identityhttp.NewHandler(authService, logger, cfg.Auth)
	if err != nil {
		_ = db.Close()
		_ = logFile.Close()
		return nil, err
	}

	return &API{
		Handler:  httptransport.NewRouter(cfg, logger, db, authHandler, managementHandler),
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
