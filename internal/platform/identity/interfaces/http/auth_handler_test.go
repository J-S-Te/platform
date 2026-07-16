package identityhttp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/config"
)

func TestLoginRejectsUnknownFieldsUsingValidationEnvelope(t *testing.T) {
	service := &fakeApplicationService{}
	handler := newTestHandler(t, service)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"account":"admin","password":"pw","login_type":"password","tenant_id":"not-allowed"}`))
	response := httptest.NewRecorder()

	handler.Login(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Code != "PLATFORM_VALIDATION_ERROR" {
		t.Fatalf("error code = %q", body.Code)
	}
	if service.loginCalls != 0 {
		t.Fatal("invalid request called the application service")
	}
}

func TestLoginSetsHttpOnlySessionCookie(t *testing.T) {
	expiresAt := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)
	service := &fakeApplicationService{loginResult: application.SessionResult{ExpiresAt: expiresAt, RedirectURL: "/", Token: "signed-token"}}
	handler := newTestHandler(t, service)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"account":"admin","password":"pw","login_type":"password"}`))
	response := httptest.NewRecorder()

	handler.Login(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookie count = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "bp_session" || cookie.Value != "signed-token" || !cookie.HttpOnly || cookie.Path != "/" {
		t.Fatalf("unexpected session cookie: %#v", cookie)
	}
	if cookie.Secure {
		t.Fatal("development test cookie unexpectedly has Secure")
	}
}

func newTestHandler(t *testing.T, service *fakeApplicationService) *Handler {
	t.Helper()
	handler, err := NewHandler(service, slog.New(slog.NewJSONHandler(io.Discard, nil)), config.AuthConfig{
		SessionCookieName: "bp_session", SessionCookieSameSite: "Lax",
	})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

type fakeApplicationService struct {
	loginResult application.SessionResult
	loginErr    error
	loginCalls  int
}

func (service *fakeApplicationService) Login(context.Context, application.LoginInput) (application.SessionResult, error) {
	service.loginCalls++
	return service.loginResult, service.loginErr
}

func (service *fakeApplicationService) Authenticate(context.Context, string) (authctx.Principal, error) {
	return authctx.Principal{}, application.ErrUnauthenticated
}

func (service *fakeApplicationService) Refresh(context.Context, authctx.Principal) (application.SessionResult, error) {
	return application.SessionResult{}, application.ErrUnauthenticated
}

func (service *fakeApplicationService) Logout(context.Context, authctx.Principal) error {
	return application.ErrUnauthenticated
}
