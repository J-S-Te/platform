package oidchttp

import (
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
)

func TestUserInfoRespectsLegacyAccessTokenCompatFlag(t *testing.T) {
	handler := &Handler{
		jwtManager: platformAccessTokenJWTManager{
			claims: security.OIDCTokenClaims{ClientID: "contract-prod-web", Subject: "identity-1", SessionID: "session-1", Scope: []string{"openid"}},
		},
		clock:                          authorizationContextClock{},
		logger:                         slog.New(slog.NewTextHandler(io.Discard, nil)),
		allowLegacyPlatformAccessToken: false,
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/oauth2/userinfo", nil)
	request.Header.Set("Authorization", "Bearer "+jwtWithClientID("contract-prod-web"))
	response := httptest.NewRecorder()

	handler.UserInfo(response, request)

	if response.Code != stdhttp.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, stdhttp.StatusUnauthorized, response.Body.String())
	}
}
