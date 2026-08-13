package oidchttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	oidcapplication "github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

func TestAuthorizationContextStrictlyBindsExternalAccessToken(t *testing.T) {
	tests := []struct {
		name   string
		claims ExternalAuthorizationTokenClaims
		want   int
	}{
		{
			name: "bound access token",
			claims: ExternalAuthorizationTokenClaims{
				Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", SessionID: "session-1",
				AuthorizedParty: "contract-prod-web", Audience: []string{"account", "contract-prod-web"}, TokenUse: "access_token",
			},
			want: http.StatusOK,
		},
		{
			name: "ID token rejected",
			claims: ExternalAuthorizationTokenClaims{
				Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", SessionID: "session-1",
				AuthorizedParty: "contract-prod-web", Audience: []string{"contract-prod-web"}, TokenUse: "id_token",
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "identity mismatch rejected",
			claims: ExternalAuthorizationTokenClaims{
				Subject: "identity-1", IdentityID: "identity-2", TenantID: "tenant-1", SessionID: "session-1",
				AuthorizedParty: "contract-prod-web", Audience: []string{"contract-prod-web"}, TokenUse: "access_token",
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "missing authorized party rejected",
			claims: ExternalAuthorizationTokenClaims{
				Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", SessionID: "session-1",
				Audience: []string{"contract-prod-web"}, TokenUse: "access_token",
			},
			want: http.StatusUnauthorized,
		},
		{
			name: "audience mismatch rejected",
			claims: ExternalAuthorizationTokenClaims{
				Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", SessionID: "session-1",
				AuthorizedParty: "contract-prod-web", Audience: []string{"account"}, TokenUse: "access_token",
			},
			want: http.StatusUnauthorized,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &authorizationContextResolverStub{}
			handler := &Handler{
				jwtManager:                    rejectingExternalJWTManager{},
				accessTokenSubjects:           accessTokenSubjectResolverStub{},
				externalAuthorizationVerifier: externalAuthorizationVerifierStub{claims: test.claims},
				authorizationContextResolver:  resolver,
				clock:                         authorizationContextClock{},
				logger:                        slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			request := httptest.NewRequest(http.MethodGet, "/oauth2/authorization-context", nil)
			request.Header.Set("Authorization", "Bearer external-token")
			response := httptest.NewRecorder()

			handler.AuthorizationContext(response, request)

			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
			if test.want == http.StatusOK {
				if resolver.calls != 1 || resolver.tenantID != "tenant-1" || resolver.clientID != "contract-prod-web" || resolver.subjectID != "identity-1" {
					t.Fatalf("resolver = calls:%d tenant:%q client:%q subject:%q", resolver.calls, resolver.tenantID, resolver.clientID, resolver.subjectID)
				}
				var body struct {
					ClientID        string `json:"client_id"`
					ApplicationCode string `json:"application_code"`
					EnvironmentCode string `json:"environment_code"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if body.ClientID != "contract-prod-web" || body.ApplicationCode != "contract_management" || body.EnvironmentCode != "prod" {
					t.Fatalf("application binding = %#v", body)
				}
			} else if resolver.calls != 0 {
				t.Fatalf("resolver calls = %d, want 0", resolver.calls)
			}
		})
	}
}

func TestAuthorizationContextRespectsLegacyAccessTokenCompatFlag(t *testing.T) {
	handler := &Handler{
		jwtManager: platformAccessTokenJWTManager{
			claims: security.OIDCTokenClaims{ClientID: "contract-prod-web", Subject: "identity-1", SessionID: "session-1", Scope: []string{"openid"}},
		},
		accessTokenSubjects:            accessTokenSubjectResolverStub{},
		authorizationContextResolver:   &authorizationContextResolverStub{},
		clock:                          authorizationContextClock{},
		allowLegacyPlatformAccessToken: false,
		logger:                         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodGet, "/oauth2/authorization-context", nil)
	request.Header.Set("Authorization", "Bearer "+jwtWithClientID("contract-prod-web"))
	response := httptest.NewRecorder()

	handler.AuthorizationContext(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
}

type externalAuthorizationVerifierStub struct {
	claims ExternalAuthorizationTokenClaims
}

func (v externalAuthorizationVerifierStub) Verify(context.Context, string) (ExternalAuthorizationTokenClaims, error) {
	return v.claims, nil
}

type authorizationContextResolverStub struct {
	calls                         int
	tenantID, clientID, subjectID string
	err                           error
}

func (r *authorizationContextResolverStub) ResolveOIDCAuthorizationContext(_ context.Context, tenantID, clientID, subjectID string) (tokenissuer.AuthorizationContext, error) {
	r.calls++
	r.tenantID, r.clientID, r.subjectID = tenantID, clientID, subjectID
	if r.err != nil {
		return tokenissuer.AuthorizationContext{}, r.err
	}
	return tokenissuer.AuthorizationContext{ClientID: clientID, ApplicationCode: "contract_management", EnvironmentCode: "prod", TenantID: tenantID, PersonID: "person-1", Roles: []string{"reader"}, Permissions: []string{"contract.read"}, AuthorizationRevision: 1}, nil
}

func TestAuthorizationContextSeparatesAccessDenialFromInfrastructureFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "application access denied", err: oidcapplication.ErrAccessDenied, want: http.StatusForbidden},
		{name: "resolver unavailable", err: errors.New("database unavailable"), want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := &Handler{
				jwtManager: rejectingExternalJWTManager{}, accessTokenSubjects: accessTokenSubjectResolverStub{},
				externalAuthorizationVerifier: externalAuthorizationVerifierStub{claims: ExternalAuthorizationTokenClaims{
					Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", SessionID: "session-1",
					AuthorizedParty: "contract-prod-web", Audience: []string{"contract-prod-web"}, TokenUse: "access_token",
				}},
				authorizationContextResolver: &authorizationContextResolverStub{err: test.err},
				clock:                        authorizationContextClock{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			request := httptest.NewRequest(http.MethodGet, "/oauth2/authorization-context", nil)
			request.Header.Set("Authorization", "Bearer external-token")
			response := httptest.NewRecorder()

			handler.AuthorizationContext(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

type accessTokenSubjectResolverStub struct{}

func (accessTokenSubjectResolverStub) ResolveAccessTokenSubject(context.Context, string, string, string) (AccessTokenSubject, error) {
	return AccessTokenSubject{}, errors.New("platform access token not expected")
}

type rejectingExternalJWTManager struct{}

func (rejectingExternalJWTManager) Issuer() string { return "https://platform.example.com" }
func (rejectingExternalJWTManager) JWKS() security.OIDCJWKS {
	return security.OIDCJWKS{}
}
func (rejectingExternalJWTManager) VerifyAccessToken(string, string, time.Time) (security.OIDCTokenClaims, error) {
	return security.OIDCTokenClaims{}, errors.New("not a platform access token")
}
func (rejectingExternalJWTManager) VerifyIDToken(string, string, time.Time) (security.OIDCTokenClaims, error) {
	return security.OIDCTokenClaims{}, errors.New("not an ID token")
}

type authorizationContextClock struct{}

func (authorizationContextClock) Now() time.Time { return time.Unix(1, 0).UTC() }

func jwtWithClientID(clientID string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"client_id":"` + clientID + `"}`))
	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".signature"
}

type platformAccessTokenJWTManager struct {
	claims security.OIDCTokenClaims
}

func (m platformAccessTokenJWTManager) Issuer() string { return "https://platform.example.com" }
func (m platformAccessTokenJWTManager) JWKS() security.OIDCJWKS {
	return security.OIDCJWKS{}
}
func (m platformAccessTokenJWTManager) VerifyAccessToken(string, string, time.Time) (security.OIDCTokenClaims, error) {
	return m.claims, nil
}
func (m platformAccessTokenJWTManager) VerifyIDToken(string, string, time.Time) (security.OIDCTokenClaims, error) {
	return security.OIDCTokenClaims{}, errors.New("not an ID token")
}
