package oidchttp

import (
	"context"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/tokenissuer"
)

type staticAuthorizationResolver struct {
	claims tokenissuer.AuthorizationClaims
	err    error
}

func (resolver staticAuthorizationResolver) ResolveOIDCAuthorization(context.Context, string, string, string) (tokenissuer.AuthorizationClaims, error) {
	return resolver.claims, resolver.err
}

func TestAuthorizationResolverCarriesCurrentApplicationSnapshot(t *testing.T) {
	resolver := staticAuthorizationResolver{claims: tokenissuer.AuthorizationClaims{
		TenantID: "tenant-1", Roles: []string{"sales"}, Permissions: []string{"dashboard", "contract.read"},
		RoleConfigHash: "hash-1", AuthzRevision: 8,
	}}
	claims, err := resolver.ResolveOIDCAuthorization(context.Background(), "tenant-1", "client-1", "user-1")
	if err != nil {
		t.Fatalf("ResolveOIDCAuthorization() error = %v", err)
	}
	if claims.AuthzRevision != 8 || claims.RoleConfigHash != "hash-1" || len(claims.Roles) != 1 || len(claims.Permissions) != 2 {
		t.Fatalf("authorization claims = %#v", claims)
	}
}
