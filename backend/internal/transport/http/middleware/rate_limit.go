package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// FixedWindowRateLimit 用进程内固定窗口保护敏感端点。互斥锁同时保护计数和清理；它适用于
// 单体或单副本防护，不提供跨实例总额度，扩容后必须改用共享限流存储或网关策略。
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
		// 清理与请求计数共用临界区，避免删除刚被另一请求刷新过的桶；清理频率最多每窗口一次。
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
