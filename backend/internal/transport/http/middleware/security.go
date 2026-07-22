package middleware

import "github.com/gin-gonic/gin"

// SecurityHeaders adds browser-facing headers that are safe for the API surface.
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
