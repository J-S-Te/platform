package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORS 仅对精确配置的 Origin 返回凭据许可，并设置 Vary: Origin，防止共享缓存把一个来源的
// Access-Control-Allow-Origin 响应复用于另一个来源。未命中来源时不回显请求值。
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if normalized := strings.TrimSpace(origin); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}

	return func(context *gin.Context) {
		origin := context.GetHeader("Origin")
		if origin != "" {
			if _, ok := allowed[origin]; ok {
				context.Header("Access-Control-Allow-Origin", origin)
				context.Header("Access-Control-Allow-Credentials", "true")
				context.Writer.Header().Add("Vary", "Origin")
			}
		}

		if context.Request.Method == http.MethodOptions && context.GetHeader("Access-Control-Request-Method") != "" {
			if _, ok := allowed[origin]; !ok {
				context.AbortWithStatus(http.StatusForbidden)
				return
			}
			context.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			context.Header("Access-Control-Allow-Headers", "Accept, Content-Type, X-Request-ID")
			context.Header("Access-Control-Max-Age", "600")
			context.AbortWithStatus(http.StatusNoContent)
			return
		}

		context.Next()
	}
}

// ContainsExactOrigin 保持字符串精确匹配；协议、端口或主机任一不同都属于另一个安全来源。
func ContainsExactOrigin(allowedOrigins []string, origin string) bool {
	for _, allowed := range allowedOrigins {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}
	return false
}
