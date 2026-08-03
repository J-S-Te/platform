// Package middleware contains application bearer authentication for integration endpoints.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

const maxApplicationBearerTokenLength = 16 * 1024

// ApplicationAuthenticator verifies an external system bearer access token.
type ApplicationAuthenticator interface {
	Authenticate(context.Context, string) (appctx.Principal, error)
}

// ApplicationAuthentication 校验应用 OAuth Bearer，并把可信的客户端/应用/环境绑定写入上下文。
// 伪造、过期、撤销和绑定不匹配统一返回未认证，避免向攻击者泄露凭据状态差异。
func ApplicationAuthentication(authenticator ApplicationAuthenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		if authenticator == nil {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusInternalServerError, httperror.Internal)
			c.Abort()
			return
		}
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			c.Abort()
			return
		}
		principal, err := authenticator.Authenticate(c.Request.Context(), token)
		if err != nil || !principal.Valid() {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(appctx.WithPrincipal(c.Request.Context(), principal))
		c.Next()
	}
}

// RequireApplicationScope 校验集成客户端 scope，而不是后台用户角色权限；两种授权模型不能互换。
func RequireApplicationScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if scope == "" || strings.TrimSpace(scope) != scope {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusInternalServerError, httperror.Internal)
			c.Abort()
			return
		}
		principal, ok := appctx.PrincipalFromContext(c.Request.Context())
		if !ok || !principal.HasScope(scope) {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusForbidden, httperror.Forbidden)
			c.Abort()
			return
		}
		c.Next()
	}
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" || len(parts[1]) > maxApplicationBearerTokenLength {
		return "", false
	}
	return parts[1], true
}

// ConsoleOrApplicationAuthentication 允许后台 Cookie 或应用 Bearer 二选一，仅适用于处理器能分别
// 对两类主体完成授权判断的接口。Cookie 优先，防止浏览器附带 Bearer 后绕过用户权限边界。
func ConsoleOrApplicationAuthentication(console Authenticator, cookieName string, application ApplicationAuthenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		if console == nil || strings.TrimSpace(cookieName) == "" || application == nil {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusInternalServerError, httperror.Internal)
			c.Abort()
			return
		}
		if cookie, err := c.Request.Cookie(cookieName); err == nil && strings.TrimSpace(cookie.Value) != "" {
			principal, authErr := console.Authenticate(c.Request.Context(), cookie.Value)
			if authErr != nil {
				httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
				c.Abort()
				return
			}
			c.Request = c.Request.WithContext(authctx.WithPrincipal(c.Request.Context(), principal))
			c.Next()
			return
		}
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			c.Abort()
			return
		}
		principal, authErr := application.Authenticate(c.Request.Context(), token)
		if authErr != nil || !principal.Valid() {
			httpresponse.WriteError(c.Writer, c.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(appctx.WithPrincipal(c.Request.Context(), principal))
		c.Next()
	}
}
