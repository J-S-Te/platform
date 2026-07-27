package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// ClientIP resolves the request address through Gin's explicitly configured trusted-proxy list
// and stores the result in the standard request context for downstream net/http handlers.
func ClientIP() gin.HandlerFunc {
	return func(context *gin.Context) {
		clientIP := normalizeClientIP(context.ClientIP())
		context.Request = context.Request.WithContext(requestctx.WithClientIP(context.Request.Context(), clientIP))
		context.Next()
	}
}

// RequestClientIP returns only the address established by ClientIP. The RemoteAddr fallback is
// retained for unit tests or adapters invoked without the normal router middleware chain.
func RequestClientIP(request *http.Request) string {
	if request == nil {
		return ""
	}
	if clientIP := normalizeClientIP(requestctx.ClientIP(request.Context())); clientIP != "" {
		return clientIP
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(request.RemoteAddr))
	if err == nil {
		return normalizeClientIP(host)
	}
	return normalizeClientIP(request.RemoteAddr)
}

func normalizeClientIP(value string) string {
	parsed := net.ParseIP(strings.TrimSpace(value))
	if parsed == nil {
		return ""
	}
	return parsed.String()
}
