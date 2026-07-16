package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
)

// AccessLog records one structured event per completed HTTP request without logging credentials
// or request bodies.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startedAt := time.Now()
			recorder := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(recorder, r)

			logger.Info("http request completed",
				"request_id", requestctx.RequestID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.Status(),
				"duration_ms", time.Since(startedAt).Milliseconds(),
			)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *statusRecorder) WriteHeader(status int) {
	recorder.status = status
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *statusRecorder) Write(body []byte) (int, error) {
	if recorder.status == 0 {
		recorder.WriteHeader(http.StatusOK)
	}
	return recorder.ResponseWriter.Write(body)
}

func (recorder *statusRecorder) Status() int {
	if recorder.status == 0 {
		return http.StatusOK
	}
	return recorder.status
}
