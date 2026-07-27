package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// FixedWindowRateLimit protects sensitive read endpoints without an external cache. The map is
// bounded by periodic cleanup, so it is suitable for this monolith but intentionally not a
// distributed rate-limit implementation.
func FixedWindowRateLimit(limit int, window time.Duration) gin.HandlerFunc {
	type bucket struct {
		startedAt time.Time
		count     int
	}
	var mutex sync.Mutex
	buckets := make(map[string]bucket)
	lastCleanup := time.Now()
	return func(context *gin.Context) {
		now := time.Now()
		key := RequestClientIP(context.Request)
		if key == "" {
			key = "unknown"
		}
		mutex.Lock()
		if now.Sub(lastCleanup) >= window {
			for candidate, current := range buckets {
				if now.Sub(current.startedAt) >= window {
					delete(buckets, candidate)
				}
			}
			lastCleanup = now
		}
		current := buckets[key]
		if current.startedAt.IsZero() || now.Sub(current.startedAt) >= window {
			current = bucket{startedAt: now}
		}
		current.count++
		buckets[key] = current
		allowed := current.count <= limit
		mutex.Unlock()
		if !allowed {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusTooManyRequests, httperror.New("PLATFORM_RATE_LIMITED", "请求过于频繁，请稍后再试", nil))
			return
		}
		context.Next()
	}
}
