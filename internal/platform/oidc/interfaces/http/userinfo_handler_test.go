package oidchttp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

func TestUserInfoAcceptsPlatformTokenWhenLegacyAuthorizationContextIsDisabled(t *testing.T) {
	handler := &Handler{
		service: userInfoServiceStub{},
		jwtManager: platformAccessTokenJWTManager{
			claims: security.OIDCTokenClaims{ClientID: "contract-prod-web", Subject: "identity-1", SessionID: "session-1", Scope: []string{"openid"}},
		},
		accessTokenSubjects:            userInfoAccessTokenSubjectResolverStub{},
		authorizationResolver:          userInfoAuthorizationResolverStub{},
		clock:                          authorizationContextClock{},
		logger:                         slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowLegacyPlatformAccessToken: false,
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/oauth2/userinfo", nil)
	request.Header.Set("Authorization", "Bearer "+jwtWithClientID("contract-prod-web"))
	response := httptest.NewRecorder()

	handler.UserInfo(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, stdhttp.StatusOK, response.Body.String())
	}
	var body struct {
		Subject    string   `json:"sub"`
		IdentityID string   `json:"identity_id"`
		TenantID   string   `json:"tenant_id"`
		Roles      []string `json:"roles"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Subject != "identity-1" || body.IdentityID != "identity-1" || body.TenantID != "tenant-1" || len(body.Roles) != 1 || body.Roles[0] != "contract.reader" {
		t.Fatalf("unexpected UserInfo response: %#v", body)
	}
}

type userInfoServiceStub struct{ Service }

func (userInfoServiceStub) IsAccessTokenRevoked(context.Context, string, string) (bool, error) {
	return false, nil
}

func (userInfoServiceStub) ResolveUserInfo(context.Context, application.UserInfoInput) (application.UserInfo, error) {
	return application.UserInfo{Subject: "identity-1"}, nil
}

type userInfoAccessTokenSubjectResolverStub struct{}

func (userInfoAccessTokenSubjectResolverStub) ResolveAccessTokenSubject(context.Context, string, string, string) (AccessTokenSubject, error) {
	return AccessTokenSubject{TenantID: "tenant-1", OAuthClientID: "oauth-client-1"}, nil
}

type userInfoAuthorizationResolverStub struct{}

func (userInfoAuthorizationResolverStub) ResolveOIDCAuthorization(context.Context, string, string, string) (tokenissuer.AuthorizationClaims, error) {
	return tokenissuer.AuthorizationClaims{TenantID: "tenant-1", PersonID: "person-1", Roles: []string{"contract.reader"}}, nil
}
