package httptransport

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// HealthHandler owns process liveness and dependency readiness endpoints.
type HealthHandler struct {
	db      *sql.DB
	appName string
}

// NewHealthHandler creates a handler using the application's shared database pool.
func NewHealthHandler(db *sql.DB, appName string) HealthHandler {
	return HealthHandler{db: db, appName: appName}
}

// Liveness reports whether the HTTP process is running. It intentionally does not query MySQL.
func (handler HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	httpresponse.WriteSuccess(w, r, http.StatusOK, "服务运行正常", map[string]string{
		"status":  "ok",
		"service": handler.appName,
	})
}

// Readiness reports whether the API's required database dependency can be reached.
func (handler HealthHandler) Readiness(w http.ResponseWriter, r *http.Request) {
	contextWithTimeout, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := handler.db.PingContext(contextWithTimeout); err != nil {
		httpresponse.WriteError(w, r, http.StatusServiceUnavailable, httperror.DependencyUnavailable)
		return
	}

	httpresponse.WriteSuccess(w, r, http.StatusOK, "依赖服务可用", map[string]string{
		"status": "ok",
		"mysql":  "available",
	})
}
