package oidchttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type logoutSessionAdapterStub struct {
	principal authctx.Principal
	logoutErr error
	calls     int
}

func (stub *logoutSessionAdapterStub) Authenticate(context.Context, string) (authctx.Principal, error) {
	return stub.principal, nil
}

func (stub *logoutSessionAdapterStub) Logout(context.Context, authctx.Principal) error {
	stub.calls++
	return stub.logoutErr
}

func TestLogoutFailsClosedWhenGlobalSessionRevocationFails(t *testing.T) {
	adapter := &logoutSessionAdapterStub{principal: authctx.Principal{SessionID: "session-1"}, logoutErr: errors.New("keycloak unavailable")}
	handler := &Handler{
		jwtManager: authorizationContextJWTManager{}, sessionAuth: adapter, sessionLogout: adapter,
		cookie: cookieConfig{name: "bp_session"}, clock: authorizationContextClock{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil)
	request.AddCookie(&http.Cookie{Name: "bp_session", Value: "signed-session"})
	response := httptest.NewRecorder()

	handler.Logout(response, request)

	if response.Code != http.StatusServiceUnavailable || adapter.calls != 1 {
		t.Fatalf("logout status=%d calls=%d body=%s", response.Code, adapter.calls, response.Body.String())
	}
	if response.Header().Get("Set-Cookie") != "" || response.Header().Get("Clear-Site-Data") != "" {
		t.Fatalf("failed logout must preserve browser state: headers=%v", response.Header())
	}
}

func TestLogoutClearsSameOriginSessionsOnlyAfterGlobalRevocation(t *testing.T) {
	adapter := &logoutSessionAdapterStub{principal: authctx.Principal{SessionID: "session-1"}}
	handler := &Handler{
		jwtManager: authorizationContextJWTManager{}, sessionAuth: adapter, sessionLogout: adapter,
		cookie: cookieConfig{name: "bp_session"}, clock: authorizationContextClock{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil)
	request.AddCookie(&http.Cookie{Name: "bp_session", Value: "signed-session"})
	response := httptest.NewRecorder()

	handler.Logout(response, request)

	if response.Code != http.StatusFound || adapter.calls != 1 {
		t.Fatalf("logout status=%d calls=%d body=%s", response.Code, adapter.calls, response.Body.String())
	}
	if got := response.Header().Get("Clear-Site-Data"); got != `"cookies", "storage"` {
		t.Fatalf("Clear-Site-Data=%q", got)
	}
	if cookie := response.Header().Get("Set-Cookie"); !strings.Contains(cookie, "bp_session=") || !strings.Contains(cookie, "Max-Age=0") {
		t.Fatalf("expired session cookie missing: %q", cookie)
	}
}

type authorizationContextJWTManager struct{ rejectingExternalJWTManager }

func (authorizationContextJWTManager) Issuer() string { return "https://platform.example.com" }
