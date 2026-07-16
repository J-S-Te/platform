// Package httptransport assembles the HTTP transport without embedding business logic.
package httptransport

import (
	"database/sql"
	"log/slog"
	"net/http"

	identityhttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/identity/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/config"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/backend/internal/transport/http/middleware"
	"github.com/go-chi/chi/v5"
)

// NewRouter creates the shared middleware chain and registers infrastructure endpoints. Domain
// modules register their own routes here only through their public HTTP adapters.
func NewRouter(cfg config.Config, logger *slog.Logger, db *sql.DB, authHandler *identityhttp.Handler, managementHandler *identityhttp.ManagementHandler) http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(middleware.Recover(logger))
	router.Use(middleware.AccessLog(logger))
	router.Use(middleware.SecurityHeaders)
	router.Use(middleware.CORS(cfg.CORSOrigins))

	healthHandler := NewHealthHandler(db, cfg.AppName)
	router.Get("/healthz", healthHandler.Liveness)
	router.Get("/readyz", healthHandler.Readiness)

	if authHandler != nil {
		router.Route("/api/v1/auth", func(authRouter chi.Router) {
			authRouter.Post("/login", authHandler.Login)
			authRouter.Group(func(protected chi.Router) {
				protected.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
				protected.Post("/token/refresh", authHandler.Refresh)
				protected.Post("/logout", authHandler.Logout)
				protected.Get("/me", authHandler.Me)
			})
		})
	}

	if authHandler != nil && managementHandler != nil {
		router.Route("/api/v1", func(apiRouter chi.Router) {
			apiRouter.Use(middleware.Authentication(authHandler, authHandler.CookieName()))
			apiRouter.Get("/users", managementHandler.ListUsers)
			apiRouter.Post("/users", managementHandler.CreateUser)
			apiRouter.Get("/users/{user_id}", managementHandler.GetUser)
			apiRouter.Patch("/users/{user_id}", managementHandler.UpdateUser)
			apiRouter.Get("/accounts", managementHandler.ListAccounts)
			apiRouter.Patch("/accounts/{account_id}", managementHandler.UpdateAccount)
			apiRouter.Get("/org-units", managementHandler.ListOrgUnits)
			apiRouter.Post("/org-units", managementHandler.CreateOrgUnit)
			apiRouter.Get("/positions", managementHandler.ListPositions)
			apiRouter.Post("/positions", managementHandler.CreatePosition)
			apiRouter.Get("/memberships", managementHandler.ListMemberships)
			apiRouter.Post("/memberships", managementHandler.CreateMembership)
			apiRouter.Patch("/memberships/{membership_id}", managementHandler.UpdateMembership)
		})
	}

	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpresponse.WriteError(w, r, http.StatusNotFound, httperror.NotFound)
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpresponse.WriteError(w, r, http.StatusMethodNotAllowed, httperror.MethodNotAllowed)
	})

	return router
}
