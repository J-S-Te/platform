package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// 地址来源标记（与统一审计协议一致）。
const (
	ipSourceDirect       = "DIRECT"
	ipSourceTrustedProxy = "TRUSTED_PROXY"
	ipSourceUnknown      = "UNKNOWN"
)

// ClientIP 解析客户端地址并记录**解析来源**，再写入标准 request context。
//
// 关键点：来源必须由解析过程产生。此前的实现让下游按“上下文里有没有地址”推断，
// 结果直连请求也会被标成 TRUSTED_PROXY——代理头是否被采信，只有解析处知道。
func ClientIP(trustedProxies []string) gin.HandlerFunc {
	trusted := parseTrustedProxies(trustedProxies)
	return func(context *gin.Context) {
		clientIP, source := resolveClientIP(context, trusted)
		context.Request = context.Request.WithContext(
			requestctx.WithClientIPSource(context.Request.Context(), clientIP, source))
		context.Next()
	}
}

// resolveClientIP 只在直接对端属于显式可信代理时才采信代理头，否则使用连接对端地址。
func resolveClientIP(context *gin.Context, trusted []netip.Prefix) (string, string) {
	peer := normalizeClientIP(context.RemoteIP())
	if peer == "" {
		return "", ipSourceUnknown
	}
	if !trustedContains(trusted, peer) {
		// 非可信对端：代理头一律忽略，地址来自连接本身。
		return peer, ipSourceDirect
	}
	// 可信代理：Gin 已按同一份可信列表解析代理头。
	resolved := normalizeClientIP(context.ClientIP())
	if resolved == "" || resolved == peer {
		// 可信代理但没有可采信的转发头。
		return peer, ipSourceDirect
	}
	return resolved, ipSourceTrustedProxy
}

func parseTrustedProxies(values []string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(value); err == nil {
			result = append(result, prefix)
			continue
		}
		if address, err := netip.ParseAddr(value); err == nil {
			result = append(result, netip.PrefixFrom(address, address.BitLen()))
		}
	}
	return result
}

func trustedContains(prefixes []netip.Prefix, value string) bool {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return false
	}
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
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
