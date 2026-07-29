package applicationaccess

import (
	"context"
	"errors"

	oidcapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/tokenissuer"
)

// ApplicationAuthorizationResolver adapts application-scoped authorization to the OIDC issuer.
// The OAuth client is the source of truth for application and environment selection; no
// subsystem-specific application code is inspected here.
type ApplicationAuthorizationResolver struct{ service *Service }

func NewApplicationAuthorizationResolver(service *Service) (*ApplicationAuthorizationResolver, error) {
	if service == nil {
		return nil, errors.New("application authorization resolver service must not be nil")
	}
	return &ApplicationAuthorizationResolver{service: service}, nil
}

var _ tokenissuer.AuthorizationResolver = (*ApplicationAuthorizationResolver)(nil)

func (resolver *ApplicationAuthorizationResolver) ResolveOIDCAuthorization(ctx context.Context, tenantID, clientID, userID string) (tokenissuer.AuthorizationClaims, error) {
	resolved, err := resolver.service.ResolveOIDCAuthorization(ctx, tenantID, clientID, userID)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrAccessDenied) {
			return tokenissuer.AuthorizationClaims{}, oidcapplication.ErrAccessDenied
		}
		return tokenissuer.AuthorizationClaims{}, err
	}
	return tokenissuer.AuthorizationClaims{
		TenantID: resolved.TenantID, Roles: append([]string(nil), resolved.Roles...),
		Permissions: append([]string(nil), resolved.Permissions...), RoleConfigHash: resolved.RoleConfigHash,
		AuthzRevision: resolved.AuthzRevision,
	}, nil
}
