package httptransport

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	identityhttp "github.com/J-S-Te/Basic-Platform/internal/platform/identity/interfaces/http"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

func TestRouterLivenessUsesStandardEnvelope(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := config.Config{
		AppName:     "basic-platform",
		CORSOrigins: []string{"http://localhost:5173"},
	}
	router := NewRouter(cfg, logger, &sql.DB{}, nil)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want JSON response", got)
	}
	if got := response.Header().Get("X-Request-ID"); got == "" {
		t.Fatal("X-Request-ID is empty")
	}
}

func TestRouterRejectsUnknownRouteWithStandardError(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := config.Config{AppName: "basic-platform", CORSOrigins: []string{"http://localhost:5173"}}
	router := NewRouter(cfg, logger, &sql.DB{}, nil)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestRouterProtectsAuthMeRoute(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := config.Config{
		AppName: "basic-platform",
		Auth: config.AuthConfig{
			SessionCookieName: "bp_session", SessionCookieSameSite: "Lax",
		},
		CORSOrigins: []string{"http://localhost:5173"},
	}
	authHandler, err := identityhttp.NewHandler(routerTestAuthenticationService{}, logger, cfg.Auth)
	if err != nil {
		t.Fatalf("new authentication handler: %v", err)
	}
	router := NewRouter(cfg, logger, &sql.DB{}, authHandler)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

type routerTestAuthenticationService struct{}

func (routerTestAuthenticationService) Login(context.Context, application.LoginInput) (application.SessionResult, error) {
	return application.SessionResult{}, application.ErrUnauthenticated
}

func (routerTestAuthenticationService) Authenticate(context.Context, string) (authctx.Principal, error) {
	return authctx.Principal{}, application.ErrUnauthenticated
}

func (routerTestAuthenticationService) Refresh(context.Context, authctx.Principal) (application.SessionResult, error) {
	return application.SessionResult{}, application.ErrUnauthenticated
}

func (routerTestAuthenticationService) Logout(context.Context, authctx.Principal) error {
	return application.ErrUnauthenticated
}
