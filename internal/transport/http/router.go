// Package httptransport assembles the HTTP transport without embedding business logic.
package httptransport

import (
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
	"github.com/go-chi/chi/v5"
)

// NewRouter creates the shared middleware chain and registers infrastructure endpoints. Domain
// modules register their own routes here only through their public HTTP adapters.
func NewRouter(cfg config.Config, logger *slog.Logger, db *sql.DB) http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.Recover(logger))
	router.Use(middleware.AccessLog(logger))
	router.Use(middleware.SecurityHeaders)
	router.Use(middleware.CORS(cfg.CORSOrigins))

	healthHandler := NewHealthHandler(db, cfg.AppName)
	router.Get("/healthz", healthHandler.Liveness)
	router.Get("/readyz", healthHandler.Readiness)

	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpresponse.WriteError(w, r, http.StatusMethodNotAllowed, httperror.MethodNotAllowed)
	})

	return router
}
