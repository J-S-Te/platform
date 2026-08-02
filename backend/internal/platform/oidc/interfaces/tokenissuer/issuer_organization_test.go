package tokenissuer

import (
	"context"
	"testing"
	"time"

	oidcapplication "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/security"
)

type capturingSigner struct {
	access security.OIDCTokenClaims
	id     security.OIDCTokenClaims
}

func (signer *capturingSigner) Issuer() string { return "https://identity.example.test" }

func (signer *capturingSigner) IssueAccessToken(claims security.OIDCTokenClaims) (string, error) {
	signer.access = claims
	return "access-token", nil
}

func (signer *capturingSigner) IssueIDToken(claims security.OIDCTokenClaims) (string, error) {
	signer.id = claims
	return "id-token", nil
}

type fixedIDGenerator struct{ value string }

func (generator fixedIDGenerator) New(time.Time) (string, error) { return generator.value, nil }

type fixedAuthorizationResolver struct{ claims AuthorizationClaims }

func (resolver fixedAuthorizationResolver) ResolveOIDCAuthorization(context.Context, string, string, string) (AuthorizationClaims, error) {
	return resolver.claims, nil
}

func TestIssueOIDCTokensCarriesOrganizationClaimsIntoBothTokens(t *testing.T) {
	signer := &capturingSigner{}
	issuer, err := newIssuer(signer, fixedIDGenerator{value: "id-token-id"}, fixedAuthorizationResolver{claims: AuthorizationClaims{
		TenantID:        "tenant-1",
		PersonID:        "PMS-U10086",
		PrimaryOrgID:    "org-a",
		OrganizationIDs: []string{"org-a", "org-b"},
		Roles:           []string{"sales"},
		Permissions:     []string{"customer.read"},
		RoleConfigHash:  "role-hash",
		AuthzRevision:   9,
	}})
	if err != nil {
		t.Fatalf("newIssuer() error = %v", err)
	}
	now := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	result, err := issuer.IssueOIDCTokens(context.Background(), oidcapplication.TokenIssue{
		AccessTokenID:        "access-token-id",
		TenantID:             "tenant-1",
		ClientID:             "crm-client",
		SessionID:            "session-1",
		UserID:               "user-1",
		Scopes:               []string{"openid"},
		AuthorizedAt:         now,
		IssuedAt:             now,
		AccessTokenExpiresAt: now.Add(5 * time.Minute),
		IssueIDToken:         true,
	})
	if err != nil {
		t.Fatalf("IssueOIDCTokens() error = %v", err)
	}
	if result.AccessToken != "access-token" || result.IDToken != "id-token" {
		t.Fatalf("issued tokens = %#v", result)
	}
	for name, claims := range map[string]security.OIDCTokenClaims{"access": signer.access, "id": signer.id} {
		if claims.PersonID != "PMS-U10086" {
			t.Fatalf("%s token person_id = %q", name, claims.PersonID)
		}
		if claims.PrimaryOrgID != "org-a" || len(claims.OrganizationIDs) != 2 || claims.OrganizationIDs[0] != "org-a" || claims.OrganizationIDs[1] != "org-b" {
			t.Fatalf("%s token organization claims = %#v", name, claims)
		}
	}
	if signer.access.JWTID != "access-token-id" || signer.id.JWTID != "id-token-id" {
		t.Fatalf("token identifiers: access=%q id=%q", signer.access.JWTID, signer.id.JWTID)
	}
}

func TestIssueOIDCTokensRejectsAuthorizationFromAnotherTenant(t *testing.T) {
	signer := &capturingSigner{}
	issuer, err := newIssuer(signer, fixedIDGenerator{value: "id-token-id"}, fixedAuthorizationResolver{claims: AuthorizationClaims{
		TenantID:        "tenant-2",
		OrganizationIDs: []string{"org-foreign"},
	}})
	if err != nil {
		t.Fatalf("newIssuer() error = %v", err)
	}
	now := time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)
	_, err = issuer.IssueOIDCTokens(context.Background(), oidcapplication.TokenIssue{
		AccessTokenID:        "access-token-id",
		TenantID:             "tenant-1",
		ClientID:             "crm-client",
		SessionID:            "session-1",
		UserID:               "user-1",
		Scopes:               []string{"openid"},
		AuthorizedAt:         now,
		IssuedAt:             now,
		AccessTokenExpiresAt: now.Add(5 * time.Minute),
	})
	if err == nil {
		t.Fatal("cross-tenant authorization was accepted")
	}
	if signer.access.Subject != "" || signer.id.Subject != "" {
		t.Fatalf("tokens were signed before tenant validation: access=%#v id=%#v", signer.access, signer.id)
	}
}
