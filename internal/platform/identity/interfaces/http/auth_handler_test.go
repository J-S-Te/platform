package identityhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

func TestSessionCookieSetAndClearUseMatchingSecurityScope(t *testing.T) {
	t.Parallel()
	handler := &Handler{cookie: cookieConfig{name: "bp_session", secure: true, sameSite: http.SameSiteLaxMode}}

	setResponse := httptest.NewRecorder()
	handler.setSessionCookie(setResponse, "signed-token", time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC))
	setCookies := setResponse.Result().Cookies()
	if len(setCookies) != 1 {
		t.Fatalf("set cookies = %d", len(setCookies))
	}
	setCookie := setCookies[0]
	if setCookie.Name != "bp_session" || setCookie.Value != "signed-token" || setCookie.Path != "/" ||
		!setCookie.HttpOnly || !setCookie.Secure || setCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("set cookie = %#v", setCookie)
	}

	clearResponse := httptest.NewRecorder()
	handler.clearSessionCookie(clearResponse)
	clearCookies := clearResponse.Result().Cookies()
	if len(clearCookies) != 1 {
		t.Fatalf("clear cookies = %d", len(clearCookies))
	}
	clearCookie := clearCookies[0]
	if clearCookie.Name != setCookie.Name || clearCookie.Path != setCookie.Path ||
		clearCookie.HttpOnly != setCookie.HttpOnly || clearCookie.Secure != setCookie.Secure ||
		clearCookie.SameSite != setCookie.SameSite || clearCookie.MaxAge != -1 {
		t.Fatalf("clear cookie = %#v, set cookie = %#v", clearCookie, setCookie)
	}
}

func TestWriteApplicationErrorReturnsConflictForConcurrentSession(t *testing.T) {
	handler := &Handler{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	response := httptest.NewRecorder()

	handler.writeApplicationError(response, request, application.ConcurrentSessionError{
		TenantID: "tenant-1", UserID: "user-1", AccountID: "account-1", AccountName: "alice",
	})

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	var envelope httpresponse.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Code != "AUTH_CONCURRENT_SESSION" {
		t.Fatalf("code = %q, want AUTH_CONCURRENT_SESSION", envelope.Code)
	}
	if envelope.Message != "该账号已有有效会话；如原页面已关闭，可选择退出原会话并重新登录" {
		t.Fatalf("message = %q", envelope.Message)
	}
}

func TestWriteApplicationErrorReturnsConflictForWrappedConcurrentSession(t *testing.T) {
	handler := &Handler{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	response := httptest.NewRecorder()

	handler.writeApplicationError(response, request, errors.Join(errors.New("persist login session"), application.ErrConcurrentSession))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	var envelope httpresponse.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Code != "AUTH_CONCURRENT_SESSION" {
		t.Fatalf("code = %q, want AUTH_CONCURRENT_SESSION", envelope.Code)
	}
}
