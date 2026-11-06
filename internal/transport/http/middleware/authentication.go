package middleware

import (
	"context"
	"errors"
	identityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// Authenticator 校验浏览器会话令牌并返回服务端解析出的主体。
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (authctx.Principal, error)
}

// Authentication 将已核验的平台身份和会话期限写入请求上下文。
//
//	Authentication 在受保护路由前校验 HttpOnly 会话 Cookie。用户、租户和权限只能来自服务端
//	会话解析结果，浏览器可伪造的身份请求头永远不进入可信上下文。
//
// 参数：authenticator为本应用服务端会话校验器。
//
//	cookieName为当前应用独立Cookie名称。
//
// 返回值：执行服务端会话校验并写入可信主体的HTTP中间件。
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
			if errors.Is(err, identityapplication.ErrUnauthenticated) {
				httpresponse.WriteError(context.Writer, context.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			} else {
				httpresponse.WriteError(context.Writer, context.Request, http.StatusServiceUnavailable, httperror.New("AUTH_DEPENDENCY_UNAVAILABLE", "会话服务暂不可用，请稍后重试", nil))
			}
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
