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

// Authentication verifies the configured HttpOnly session cookie before a protected route runs.
// It never trusts identity headers supplied by a browser or external caller.
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
		// Every Cookie-authenticated response is tenant/user specific. Disable browser and shared
		// proxy caching before the handler writes headers, otherwise an account switch can reuse the
		// previous bp_session user's data or navigation permissions for the same URL.
		context.Header("Cache-Control", "no-store, private")
		context.Header("Pragma", "no-cache")
		context.Writer.Header().Add("Vary", "Cookie")
		context.Request = context.Request.WithContext(authctx.WithPrincipal(context.Request.Context(), principal))
		context.Next()
	}
}
