// Package appctx stores the authenticated application principal in request contexts.
package appctx

import (
	"context"
	"strings"
)

type contextKey struct{}

// Principal identifies an OAuth client that is allowed to call a platform integration API.
// It intentionally contains no client secret, bearer token or other credential material.
type Principal struct {
	OAuthClientID   string
	ClientID        string
	TenantID        string
	ApplicationID   string
	ApplicationCode string
	EnvironmentID   string
	EnvironmentCode string
	Scopes          map[string]struct{}
}

// Valid reports whether the principal has the complete, non-empty application binding required
// for integration requests. It is intentionally checked at the transport boundary so a faulty
// authenticator cannot turn an incomplete token into a trusted principal.
func (p Principal) Valid() bool {
	for _, value := range []string{
		p.OAuthClientID,
		p.ClientID,
		p.TenantID,
		p.ApplicationID,
		p.ApplicationCode,
		p.EnvironmentID,
		p.EnvironmentCode,
	} {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	if len(p.Scopes) == 0 {
		return false
	}
	for scope := range p.Scopes {
		if scope == "" || strings.TrimSpace(scope) != scope {
			return false
		}
	}
	return true
}

// HasScope reports whether the client token contains the requested scope.
func (p Principal) HasScope(scope string) bool {
	if scope == "" || strings.TrimSpace(scope) != scope {
		return false
	}
	_, ok := p.Scopes[scope]
	return ok
}

// WithPrincipal attaches an immutable snapshot of an application principal to a context.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, clonePrincipal(principal))
}

// PrincipalFromContext retrieves an authenticated application principal. The returned scope map
// is a copy so downstream handlers cannot mutate the value retained by the request context.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(contextKey{}).(Principal)
	if !ok || !principal.Valid() {
		return Principal{}, false
	}
	return clonePrincipal(principal), true
}

func clonePrincipal(principal Principal) Principal {
	if principal.Scopes == nil {
		return principal
	}
	scopes := make(map[string]struct{}, len(principal.Scopes))
	for scope := range principal.Scopes {
		scopes[scope] = struct{}{}
	}
	principal.Scopes = scopes
	return principal
}
