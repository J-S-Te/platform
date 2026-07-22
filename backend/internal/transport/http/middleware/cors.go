package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORS allows credentialed browser requests only from exact configured origins.
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

// ContainsExactOrigin reports whether an origin is configured. It is kept separate to make the
// exact-origin rule testable without exercising a full HTTP request.
func ContainsExactOrigin(allowedOrigins []string, origin string) bool {
	for _, allowed := range allowedOrigins {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}
	return false
}
