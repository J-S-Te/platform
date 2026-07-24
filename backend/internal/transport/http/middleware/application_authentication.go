// Package middleware contains application bearer authentication for integration endpoints.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

const maxApplicationBearerTokenLength = 16 * 1024

// ApplicationAuthenticator verifies an external system bearer access token.
type ApplicationAuthenticator interface {
	Authenticate(context.Context, string) (appctx.Principal, error)
}

// ApplicationAuthentication requires an OAuth application bearer token and exposes its trusted
// client/app/environment binding in the request context. All token failures intentionally map to
// one public response so callers cannot distinguish forged, expired, revoked or mismatched tokens.
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

// RequireApplicationScope checks a scope granted to the integration client rather than a console
// user's role permission.
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
