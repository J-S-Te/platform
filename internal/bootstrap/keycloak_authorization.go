package bootstrap

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	oidchttp "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
)

// keycloakAuthorizationVerifier adapts the narrowly scoped broker verifier to
// the authorization-context endpoint. It does not make Keycloak tokens valid
// for platform management APIs or the platform OAuth token endpoint.
type keycloakAuthorizationVerifier struct {
	verifier *middleware.KeycloakBrokerJWTVerifier
}

func (v keycloakAuthorizationVerifier) Verify(ctx context.Context, raw string) (oidchttp.ExternalAuthorizationTokenClaims, error) {
	claims, err := v.verifier.VerifyAuthorizationAccessToken(ctx, raw)
	if err != nil {
		return oidchttp.ExternalAuthorizationTokenClaims{}, err
	}
	return oidchttp.ExternalAuthorizationTokenClaims{
		Subject: claims.Subject, IdentityID: claims.IdentityID, TenantID: claims.TenantID, SessionID: claims.SessionID,
		AuthorizedParty: claims.AuthorizedParty, Audience: append([]string(nil), claims.Audience...), TokenUse: claims.TokenUse,
	}, nil
}

// keycloakJWKSBackchannel keeps the public issuer in token validation while
// allowing the API container to fetch signing keys over the Compose network.
type keycloakJWKSBackchannel struct {
	base             http.RoundTripper
	public, internal *url.URL
}

func (t keycloakJWKSBackchannel) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.public.Scheme || request.URL.Host != t.public.Host {
		return t.base.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = t.internal.Scheme, t.internal.Host
	return t.base.RoundTrip(clone)
}

func newKeycloakAuthorizationVerifier(cfgIssuer, cfgAdminURL, realm string) (*keycloakAuthorizationVerifier, error) {
	publicIssuer := strings.TrimRight(cfgIssuer, "/") + "/realms/" + strings.TrimSpace(realm)
	internalIssuer := strings.TrimRight(cfgAdminURL, "/") + "/realms/" + strings.TrimSpace(realm)
	public, err := url.Parse(publicIssuer)
	if err != nil {
		return nil, err
	}
	internal, err := url.Parse(internalIssuer)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: keycloakJWKSBackchannel{base: http.DefaultTransport, public: public, internal: internal}}
	verifier, err := middleware.NewKeycloakBrokerJWTVerifier(publicIssuer, client)
	if err != nil {
		return nil, err
	}
	return &keycloakAuthorizationVerifier{verifier: verifier}, nil
}

var _ oidchttp.ExternalAuthorizationTokenVerifier = (*keycloakAuthorizationVerifier)(nil)
