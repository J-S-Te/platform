package middleware

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// RequireMetricsAccess 保护 /metrics（安全审查 SEC-B4）：
//   - 部署配置 METRICS_ACCESS_TOKEN 时要求 Authorization: Bearer <token>（常量时间比较），
//     缺失或不匹配返回401——远程 Prometheus 抓取凭此访问；
//   - 未配置 token 时仅放行 TCP 对端为回环地址的本机抓取，其余403（fail-closed），
//     本机开发/体检无需额外配置。
//
// 取舍说明：只看连接对端 RemoteAddr、完全不看 X-Forwarded-For，防止伪造代理头冒充回环；
// 认证失败会计入 authz_failures 指标，配合登录/抓取限流可被告警。
func RequireMetricsAccess(accessToken string) gin.HandlerFunc {
	expected := strings.TrimSpace(accessToken)
	return func(context *gin.Context) {
		if expected != "" {
			token, ok := bearerToken(context.GetHeader("Authorization"))
			if ok && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
				context.Next()
				return
			}
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		peer := context.Request.RemoteAddr
		if host, _, err := net.SplitHostPort(peer); err == nil {
			peer = host
		}
		if address := net.ParseIP(peer); address != nil && address.IsLoopback() {
			context.Next()
			return
		}
		context.Abort()
		httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
	}
}
