package middleware

import "github.com/gin-gonic/gin"

// SecurityHeaders 为 API 响应附加浏览器面向的安全响应头（CSP 全禁、禁帧、禁嗅探等）。
func SecurityHeaders() gin.HandlerFunc {
	return func(context *gin.Context) {
		context.Header("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		context.Header("X-Content-Type-Options", "nosniff")
		context.Header("X-Frame-Options", "DENY")
		context.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		context.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		context.Next()
	}
}
