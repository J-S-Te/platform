package middleware

import (
	"net/http"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// RequirePermission rejects a protected route unless the authenticated server-side principal has
// the named tenant-scoped console permission. Permission state is loaded during session validation,
// so request headers can never add permissions.
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
