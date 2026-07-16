package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

// Recover converts unexpected panics into the standard API error response and logs the stack
// trace. It must remain outside business handlers so a single request cannot stop the process.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("panic recovered from http request",
						"request_id", requestctx.RequestID(r.Context()),
						"panic", recovered,
						"stack", string(debug.Stack()),
					)
					httpresponse.WriteError(w, r, http.StatusInternalServerError, httperror.Internal)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
