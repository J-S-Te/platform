package http

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"strings"
)

// EnsureSubsystemAccessBrowserCredential reuses the normal controlled RP
// reconciliation. It reads the existing Keycloak secret; it does not rotate it.
func (c *keycloakControlPlane) EnsureSubsystemAccessBrowserCredential(ctx context.Context, id, redirect string) (application.SubsystemUpdateBrowserCredential, error) {
	client, err := c.EnsureClient(ctx, id, "Basic Platform "+id, redirect)
	if err != nil || client.ClientID != id || strings.TrimSpace(client.ClientSecret) == "" {
		return application.SubsystemUpdateBrowserCredential{}, application.ErrSubsystemProvisioningUnavailable
	}
	return application.SubsystemUpdateBrowserCredential{ClientID: client.ClientID, ClientSecret: client.ClientSecret}, nil
}
