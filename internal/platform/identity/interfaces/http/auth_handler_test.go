package identityhttp

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestBeginOIDCLoginRedirectsToKeycloakWithoutBrokerVerification(t *testing.T) {
	handler := &Handler{
		cookie: cookieConfig{name: "bp_session", sameSite: http.SameSiteLaxMode},
		oidc:   oidcLoginConfig{enabled: true, issuer: "http://keycloak.test/realms/basic-platform", clientID: "platform-web", redirectPath: "/api/v1/auth/oidc/callback", stateCookie: "bp_oidc_state", nonceCookie: "bp_oidc_nonce", verifierCookie: "bp_oidc_verifier"},
	}
	request := httptest.NewRequest(http.MethodGet, "http://platform.test/api/v1/auth/login", nil)
	response := httptest.NewRecorder()

	handler.BeginOIDCLogin(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusFound)
	}
	location := response.Header().Get("Location")
	if !strings.HasPrefix(location, "http://keycloak.test/realms/basic-platform/protocol/openid-connect/auth?") {
		t.Fatalf("location = %q", location)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("client_id") != "platform-web" || parsed.Query().Get("response_type") != "code" || parsed.Query().Get("scope") != "openid profile" || parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("code_challenge") == "" {
		t.Fatalf("authorization query = %#v", parsed.Query())
	}
	if parsed.Query().Get("redirect_uri") != "http://platform.test/api/v1/auth/oidc/callback" {
		t.Fatalf("redirect_uri = %q", parsed.Query().Get("redirect_uri"))
	}
	cookies := map[string]*http.Cookie{}
	for _, cookie := range response.Result().Cookies() {
		cookies[cookie.Name] = cookie
	}
	stateCookie := cookies["bp_oidc_state"]
	nonceCookie := cookies["bp_oidc_nonce"]
	verifierCookie := cookies["bp_oidc_verifier"]
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatalf("state cookie = %#v", response.Result().Cookies())
	}
	if nonceCookie == nil || nonceCookie.Value == "" {
		t.Fatalf("nonce cookie = %#v", response.Result().Cookies())
	}
	if verifierCookie == nil || verifierCookie.Value == "" {
		t.Fatalf("PKCE verifier cookie = %#v", response.Result().Cookies())
	}
	if parsed.Query().Get("state") != stateCookie.Value || parsed.Query().Get("nonce") != nonceCookie.Value {
		t.Fatalf("state/nonce binding lost: query=%#v state=%q nonce=%q", parsed.Query(), stateCookie.Value, nonceCookie.Value)
	}
	// PKCE：code_verifier 必须是独立随机秘密，绝不能是 state 本身。
	if verifierCookie.Value == stateCookie.Value {
		t.Fatal("code_verifier must not equal state")
	}
	if !validOIDCCodeVerifier(verifierCookie.Value) {
		t.Fatalf("code_verifier = %q is not a valid RFC 7636 verifier", verifierCookie.Value)
	}
	if challenge := parsed.Query().Get("code_challenge"); challenge != oidcCodeChallenge(verifierCookie.Value) {
		t.Fatalf("code_challenge = %q, want S256(verifier)", challenge)
	}
	if challenge := parsed.Query().Get("code_challenge"); challenge == oidcCodeChallenge(stateCookie.Value) {
		t.Fatal("code_challenge must not be derived from state")
	}
	for _, cookie := range []*http.Cookie{stateCookie, nonceCookie, verifierCookie} {
		if !cookie.HttpOnly || cookie.Path != "/" || cookie.MaxAge != 600 || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatalf("one-time OIDC cookie attributes = %#v", cookie)
		}
	}
}

// 回调必须用独立的 verifier cookie 兑换令牌，而不是 state。
func TestOIDCCallbackExchangesCodeWithVerifierCookieValue(t *testing.T) {
	var tokenForm url.Values
	issuer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err == nil {
			tokenForm = request.PostForm
		}
		// 故意让令牌兑换失败：本用例只断言发出的 PKCE 参数。
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer issuer.Close()

	handler := oidcCallbackTestHandler(issuer.URL)
	verifier, err := randomOIDCCodeVerifier()
	if err != nil {
		t.Fatalf("generate verifier: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=auth-code&state=state-value", nil)
	request.AddCookie(&http.Cookie{Name: "bp_oidc_state", Value: "state-value"})
	request.AddCookie(&http.Cookie{Name: "bp_oidc_nonce", Value: "nonce-value"})
	request.AddCookie(&http.Cookie{Name: "bp_oidc_verifier", Value: verifier})
	recorder := httptest.NewRecorder()

	handler.OIDCCallback(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	if tokenForm == nil {
		t.Fatal("token endpoint was not called")
	}
	if got := tokenForm.Get("code_verifier"); got != verifier {
		t.Fatalf("code_verifier = %q, want the verifier cookie value", got)
	}
	if tokenForm.Get("code_verifier") == "state-value" {
		t.Fatal("code_verifier must not be the state value")
	}
	cleared := map[string]bool{}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.MaxAge == -1 && cookie.Value == "" {
			cleared[cookie.Name] = true
		}
	}
	for _, name := range []string{"bp_oidc_state", "bp_oidc_nonce", "bp_oidc_verifier"} {
		if !cleared[name] {
			t.Fatalf("cookie %q was not cleared: %#v", name, recorder.Result().Cookies())
		}
	}
}

// 缺少 verifier cookie 时必须拒绝回调，且不发起令牌兑换。
func TestOIDCCallbackRequiresVerifierCookie(t *testing.T) {
	tokenEndpointCalled := false
	issuer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		tokenEndpointCalled = true
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer issuer.Close()

	cases := []struct {
		name  string
		value string
		set   bool
	}{
		{name: "missing", set: false},
		{name: "empty", set: true, value: ""},
		{name: "too short", set: true, value: "short-verifier"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tokenEndpointCalled = false
			handler := oidcCallbackTestHandler(issuer.URL)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=auth-code&state=state-value", nil)
			request.AddCookie(&http.Cookie{Name: "bp_oidc_state", Value: "state-value"})
			request.AddCookie(&http.Cookie{Name: "bp_oidc_nonce", Value: "nonce-value"})
			if testCase.set {
				request.AddCookie(&http.Cookie{Name: "bp_oidc_verifier", Value: testCase.value})
			}
			recorder := httptest.NewRecorder()

			handler.OIDCCallback(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
			}
			if tokenEndpointCalled {
				t.Fatal("callback must not exchange the code without a valid verifier cookie")
			}
		})
	}
}

func oidcCallbackTestHandler(issuer string) *Handler {
	return &Handler{
		cookie: cookieConfig{name: "bp_session", sameSite: http.SameSiteLaxMode},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		oidc: oidcLoginConfig{
			enabled: true, issuer: issuer, clientID: "platform-web",
			redirectPath: "/api/v1/auth/oidc/callback",
			stateCookie:  "bp_oidc_state", nonceCookie: "bp_oidc_nonce", verifierCookie: "bp_oidc_verifier",
		},
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
