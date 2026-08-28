package middleware

import (
	"log/slog"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// AccessLog records one structured event per completed HTTP request without logging credentials
// or request bodies.
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(context *gin.Context) {
		startedAt := time.Now()
		context.Next()

		principal, _ := authctx.PrincipalFromContext(context.Request.Context())
		logger.Info("http request completed",
			"request_id", requestctx.RequestID(context.Request.Context()),
			"trace_id", requestctx.TraceID(context.Request.Context()),
			"correlation_id", requestctx.CorrelationID(context.Request.Context()),
			"tenant_id", principal.Tenant.ID,
			"subject_id", principal.User.ID,
			"method", context.Request.Method,
			"path", context.Request.URL.Path,
			"status", context.Writer.Status(),
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
	}
}
