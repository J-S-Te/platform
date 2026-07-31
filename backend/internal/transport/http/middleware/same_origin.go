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

// RequireAllowedOriginForUnsafeMethods protects cookie-backed APIs from cross-site writes.
// Every unsafe browser-session request must supply an explicitly allowed Origin. Failing closed
// when Origin is absent avoids treating a missing or stripped browser header as proof that a
// request is same-origin. Non-browser automation must use its bearer-token/service-account
// boundary instead of a browser session cookie.
func RequireAllowedOriginForUnsafeMethods(origins ...string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if normalized := issuerOrigin(origin); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}

	return func(context *gin.Context) {
		if !isUnsafeMethod(context.Request.Method) {
			context.Next()
			return
		}

		originHeader := strings.TrimSpace(context.GetHeader("Origin"))
		if originHeader != "" {
			origin := normalizedOrigin(originHeader)
			if _, ok := allowed[origin]; !ok || origin == "" {
				context.Abort()
				httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
				return
			}
			context.Next()
			return
		}

		// Sec-Fetch-Site is useful telemetry/defense in depth, but it is neither universal
		// nor an authentication signal. Do not allow a missing Origin based on this header.
		context.Abort()
		httpresponse.WriteError(context.Writer, context.Request, http.StatusForbidden, httperror.Forbidden)
	}
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
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

// RequireAllowedOriginForUnsafeMethodsOrBearer keeps the CSRF protection used by browser
// sessions while allowing an OAuth bearer request to come from a backend service with no browser
// Origin header. Only the Authorization header syntax is checked here; token authentication still
// happens in the following middleware.
func RequireAllowedOriginForUnsafeMethodsOrBearer(origins ...string) gin.HandlerFunc {
	browserGuard := RequireAllowedOriginForUnsafeMethods(origins...)
	return func(context *gin.Context) {
		if !isUnsafeMethod(context.Request.Method) {
			context.Next()
			return
		}

		// Cookie-backed requests must always pass the browser Origin check, even when an
		// Authorization header is also present. Treat any Cookie header conservatively so a
		// malformed or unrelated cookie cannot turn a browser request into a bearer request.
		if strings.TrimSpace(context.GetHeader("Cookie")) != "" {
			browserGuard(context)
			return
		}

		// A supplied Origin remains authoritative. In particular, a syntactically valid bearer
		// token must not override an explicitly cross-origin browser request.
		if strings.TrimSpace(context.GetHeader("Origin")) != "" {
			browserGuard(context)
			return
		}

		// Sec-Fetch-Site is not an authentication signal, but an explicit cross-site value is
		// strong evidence that this is a browser request rather than backend automation.
		if strings.EqualFold(strings.TrimSpace(context.GetHeader("Sec-Fetch-Site")), "cross-site") {
			browserGuard(context)
			return
		}

		if hasStrictBearerAuthorization(context.GetHeader("Authorization")) {
			context.Next()
			return
		}
		browserGuard(context)
	}
}

// hasStrictBearerAuthorization accepts the token68 form used by OAuth bearer credentials:
// one or more alphanumeric or -._~+/ characters followed only by optional trailing '=' padding.
// bearerToken also keeps this check aligned with the following authentication middleware's scheme,
// field-count and maximum-length validation.
func hasStrictBearerAuthorization(header string) bool {
	token, ok := bearerToken(header)
	if !ok {
		return false
	}

	seenTokenCharacter := false
	seenPadding := false
	for _, character := range token {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			strings.ContainsRune("-._~+/", character):
			if seenPadding {
				return false
			}
			seenTokenCharacter = true
		case character == '=':
			if !seenTokenCharacter {
				return false
			}
			seenPadding = true
		default:
			return false
		}
	}
	return seenTokenCharacter
}
