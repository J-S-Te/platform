package middleware

import (
	"context"
	"net/http"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// Authenticator verifies a browser session token and returns the server-side principal.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (authctx.Principal, error)
}

// Authentication verifies the configured HttpOnly session cookie before a protected route runs.
// It never trusts identity headers supplied by a browser or external caller.
func Authentication(authenticator Authenticator, cookieName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			cookie, err := request.Cookie(cookieName)
			if err != nil || cookie.Value == "" {
				httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
				return
			}
			principal, err := authenticator.Authenticate(request.Context(), cookie.Value)
			if err != nil {
				httpresponse.WriteError(writer, request, http.StatusUnauthorized, httperror.Unauthenticated)
				return
			}
			next.ServeHTTP(writer, request.WithContext(authctx.WithPrincipal(request.Context(), principal)))
		})
	}
}
