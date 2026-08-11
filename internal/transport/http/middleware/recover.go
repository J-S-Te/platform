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

// Recover converts unexpected panics into the standard API error response and logs the stack
// trace. It must remain outside business handlers so a single request cannot stop the process.
func Recover(logger *slog.Logger) gin.HandlerFunc {
	return func(context *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("panic recovered from http request",
					"request_id", requestctx.RequestID(context.Request.Context()),
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
