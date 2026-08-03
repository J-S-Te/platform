package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// ClientIP 只通过 Gin 已配置的可信代理链解析客户端地址，再写入标准 request context。
// 下游不能自行信任 X-Forwarded-For，否则攻击者可影响限流键和审计来源地址。
func ClientIP() gin.HandlerFunc {
	return func(context *gin.Context) {
		clientIP := normalizeClientIP(context.ClientIP())
		context.Request = context.Request.WithContext(requestctx.WithClientIP(context.Request.Context(), clientIP))
		context.Next()
	}
}

// RequestClientIP 优先返回中间件建立的可信地址；RemoteAddr 回退仅服务于未经过完整路由链的
// 单元测试或内部适配器，不解析任何转发请求头。
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
