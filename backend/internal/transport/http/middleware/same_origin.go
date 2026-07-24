package middleware

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/httpresponse"
	"github.com/gin-gonic/gin"
)

// RequireSameOrigin rejects state-changing browser requests that do not originate from the
// configured OIDC issuer. It is intended for cookie-authenticated OIDC consent operations, where
// an external site must not be able to grant or revoke an end user's consent with their session.
//
// The caller provides a validated absolute issuer URL from configuration. The middleware compares
// only the normalized scheme and host origin; issuer paths are intentionally not part of an Origin
// header and must therefore not participate in the comparison.
func RequireSameOrigin(issuer string) gin.HandlerFunc {
	allowedOrigin := issuerOrigin(issuer)

	return func(context *gin.Context) {
		origin := normalizedOrigin(context.GetHeader("Origin"))
		if allowedOrigin == "" || origin == "" || origin != allowedOrigin {
			context.Abort()
			httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
			return
		}
		context.Next()
	}
}

func issuerOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

func normalizedOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}
