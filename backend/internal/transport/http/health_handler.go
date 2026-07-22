package httptransport

import (
	"context"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// HealthHandler owns process liveness and dependency readiness endpoints.
type HealthHandler struct {
	database *gorm.DB
	appName  string
}

// NewHealthHandler creates a handler using the application's shared GORM connection.
func NewHealthHandler(database *gorm.DB, appName string) HealthHandler {
	return HealthHandler{database: database, appName: appName}
}

// Liveness reports whether the HTTP process is running. It intentionally does not query MySQL.
func (handler HealthHandler) Liveness(ginContext *gin.Context) {
	httpresponse.WriteSuccess(ginContext.Writer, ginContext.Request, http.StatusOK, "服务运行正常", map[string]string{
		"status":  "ok",
		"service": handler.appName,
	})
}

// Readiness reports whether the API's required database dependency can be reached.
func (handler HealthHandler) Readiness(ginContext *gin.Context) {
	contextWithTimeout, cancel := context.WithTimeout(ginContext.Request.Context(), 2*time.Second)
	defer cancel()

	if err := handler.database.WithContext(contextWithTimeout).Exec("SELECT 1").Error; err != nil {
		httpresponse.WriteError(ginContext.Writer, ginContext.Request, http.StatusServiceUnavailable, httperror.DependencyUnavailable)
		return
	}

	httpresponse.WriteSuccess(ginContext.Writer, ginContext.Request, http.StatusOK, "依赖服务可用", map[string]string{
		"status": "ok",
		"mysql":  "available",
	})
}
