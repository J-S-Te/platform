package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// Recover 把意外 panic 转换为标准 API 错误响应并记录调用栈。必须保持在业务处理器
// 之外装配，保证单个请求的异常不会拖垮整个进程。
func Recover(logger *slog.Logger) gin.HandlerFunc {
	return func(context *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("panic recovered from http request",
					"request_id", requestctx.RequestID(context.Request.Context()),
					"trace_id", requestctx.TraceID(context.Request.Context()),
					"correlation_id", requestctx.CorrelationID(context.Request.Context()),
					"panic", recovered,
					"stack", string(debug.Stack()),
				)
				context.Abort()
				httpresponse.WriteError(context.Writer, context.Request, http.StatusInternalServerError, httperror.Internal)
			}
		}()
		context.Next()
	}
}
