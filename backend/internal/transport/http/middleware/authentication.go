package middleware

import (
	"context"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// Authenticator verifies a browser session token and returns the server-side principal.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (authctx.Principal, error)
}

// Authentication 在受保护路由前校验 HttpOnly 会话 Cookie。用户、租户和权限只能来自服务端
// 会话解析结果，浏览器可伪造的身份请求头永远不进入可信上下文。
func Authentication(authenticator Authenticator, cookieName string) gin.HandlerFunc {
	return func(context *gin.Context) {
		cookie, err := context.Request.Cookie(cookieName)
		if err != nil || cookie.Value == "" {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		principal, err := authenticator.Authenticate(context.Request.Context(), cookie.Value)
		if err != nil {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		// Cookie 响应与租户、用户绑定，必须在业务处理器写响应前禁用浏览器和共享代理缓存；
		// 否则同一 URL 在切换账号后可能复用上一用户的数据或导航权限。
		context.Header("Cache-Control", "no-store, private")
		context.Header("Pragma", "no-cache")
		context.Writer.Header().Add("Vary", "Cookie")
		context.Request = context.Request.WithContext(authctx.WithPrincipal(context.Request.Context(), principal))
		context.Next()
	}
}
