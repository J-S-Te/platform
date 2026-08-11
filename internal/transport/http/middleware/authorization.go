package middleware

import (
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// RequirePermission 校验服务端主体当前租户内的后台权限。权限在会话校验时从可信状态加载，
// 请求头或前端可见性只能改善体验，不能增加实际权限。
func RequirePermission(permissionCode string) gin.HandlerFunc {
	return func(context *gin.Context) {
		principal, ok := authctx.PrincipalFromContext(context.Request.Context())
		if !ok {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusUnauthorized, httperror.Unauthenticated)
			return
		}
		for _, code := range principal.PermissionCodes {
			if code == permissionCode {
				context.Next()
				return
			}
		}
		context.Abort()
		httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
	}
}
