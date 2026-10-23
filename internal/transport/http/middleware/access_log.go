package middleware

import (
	"log/slog"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// AccessLog 为每个完成的 HTTP 请求记录一条结构化日志；凭据与请求体绝不入日志，
// 防止敏感信息经访问日志泄露。
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
