// Package authctx stores authenticated principal information in request contexts.
package authctx

import "context"

type principalKey struct{}

// ReferenceName is the compact identity representation returned by the authentication API.
type ReferenceName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

// Principal is the server-verified identity available to protected request handlers.
// It is populated only by authentication middleware; callers must not construct it from headers.
type Principal struct {
	SessionID       string          `json:"-"`
	Tenant          ReferenceName   `json:"tenant"`
	User            ReferenceName   `json:"user"`
	Account         ReferenceName   `json:"account"`
	Roles           []ReferenceName `json:"roles"`
	PermissionCodes []string        `json:"permission_codes"`
}

// WithPrincipal attaches an authenticated principal to a request context.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

// PrincipalFromContext returns the authenticated principal and whether middleware set one.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey{}).(Principal)
	return principal, ok
}
