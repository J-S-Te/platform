package sessionhttp

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	identity "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	store "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type loginRepository struct {
	Repository
	registerErr, stateErr error
	stateTenant           string
	issued                int
	parent                string
}

// RegisteredClient 提供已核验回调边界，隔离本测试的HTTP登录分支。
// 参数：上下文、调用方客户端标识及回调地址。
// 返回值：固定租户客户端或登记拒绝错误。
func (r *loginRepository) RegisteredClient(context.Context, string, string) (store.Client, error) {
	return store.Client{ID: "contract-prod-web", TenantID: "tenant"}, r.registerErr
}

// PlatformState 返回当前原生登录的中央锚点，检查入口不会冒充真实输入。
// 参数：ctx为请求，iam为原生会话，now为时钟，interact为交互写入开关。
// 返回值：固定父周期或预设失效/依赖错误。
func (r *loginRepository) PlatformState(ctx context.Context, iam string, now time.Time, interact bool) (store.Context, error) {
	if iam != "native" || interact {
		return store.Context{}, errors.New("unexpected native session or activity")
	}
	return store.Context{SessionID: "canonical-parent", TenantID: r.stateTenant}, r.stateErr
}

// Begin 记录票据签发，确保未登录时不会生成中性票据。
// 参数：请求、登记客户端、摘要、父周期及当前时钟。
// 返回值：测试票据，无错误。
func (r *loginRepository) Begin(_ context.Context, _ store.Client, _ string, parent string, _ time.Time) (string, error) {
	r.issued++
	r.parent = parent
	return "issued-ticket", nil
}

type loginAuthenticator struct{ err error }

// Authenticate 提供原生身份边界，不从查询参数读取身份。
// 参数：请求及浏览器Cookie原文。
// 返回值：原生会话标识或预设认证错误。
func (a loginAuthenticator) Authenticate(context.Context, string) (authctx.Principal, error) {
	return authctx.Principal{SessionID: "native"}, a.err
}

// TestBeginRequiresPlatformLogin 覆盖平台先登录及不放宽依赖和租户校验。
// 参数：t为测试上下文。
// 返回值：无；未登录提前签票、丢失原请求或错误降级时失败。
func TestBeginRequiresPlatformLogin(t *testing.T) {
	query := url.Values{
		"client_id":           {"contract-prod-web"},
		"redirect_uri":        {"https://business.test/auth/callback"},
		"state":               {"original-state-12345"},
		"nonce_challenge":     {base64.RawURLEncoding.EncodeToString(make([]byte, 32))},
		"return_to":           {"https://attacker.test"},
		"session_link_ticket": {"untrusted-ticket"},
	}
	for _, tc := range []struct {
		name                      string
		cookie                    bool
		authErr, stateErr, regErr error
		tenant                    string
		status                    int
		login                     bool
	}{
		{name: "no-cookie", status: 302, login: true},
		{name: "expired-cookie", cookie: true, authErr: identity.ErrUnauthenticated, status: 302, login: true},
		{name: "expired-anchor", cookie: true, stateErr: domain.ErrInactive, status: 302, login: true},
		{name: "auth-outage", cookie: true, authErr: errors.New("private auth outage"), status: 503},
		{name: "store-outage", cookie: true, stateErr: errors.New("private store outage"), status: 503},
		{name: "other-tenant", cookie: true, tenant: "other", status: 409},
		{name: "anchor-conflict", cookie: true, stateErr: domain.ErrBindingConflict, status: 409},
		{name: "unregistered-callback", regErr: domain.ErrValidation, status: 400},
		{name: "registry-outage", regErr: errors.New("private registry outage"), status: 503},
		{name: "authenticated", cookie: true, tenant: "tenant", status: 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &loginRepository{registerErr: tc.regErr, stateErr: tc.stateErr, stateTenant: tc.tenant}
			h, err := NewHandler(repo, failingVerifier{}, loginAuthenticator{err: tc.authErr}, "bp_session")
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/oauth2/session-links/begin?"+query.Encode(), nil)
			if tc.cookie {
				r.AddCookie(&http.Cookie{Name: "bp_session", Value: "existing-cookie"})
			}
			w := httptest.NewRecorder()
			h.Begin(w, r)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("status=%d, expected=%d; private error leaked=%v", w.Code, tc.status, strings.Contains(w.Body.String(), "private"))
			}
			if tc.login {
				assertPlatformLoginReturn(t, w, query)
			} else if tc.name == "authenticated" {
				target, _ := url.Parse(w.Header().Get("Location"))
				if repo.issued != 1 || repo.parent != "canonical-parent" || target.Query().Get("session_link_ticket") != "issued-ticket" {
					t.Fatal("authenticated begin did not bind the existing canonical parent")
				}
			} else if w.Header().Get("Location") != "" {
				t.Fatal("failure unexpectedly redirected")
			}
			if tc.name != "authenticated" && repo.issued != 0 {
				t.Fatal("ticket issued without a valid platform anchor")
			}
		})
	}
}

// assertPlatformLoginReturn 核验登录只返回固定同源Begin及必要的原请求参数。
// 参数：t为测试上下文，w为实际响应，original为原请求。
// 返回值：无；开放跳转、额外参数或缺少隐私响应头时失败。
func assertPlatformLoginReturn(t *testing.T, w *httptest.ResponseRecorder, original url.Values) {
	t.Helper()
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil || target.Scheme != "" || target.Host != "" || target.Path != "/login" {
		t.Fatal("begin did not redirect to same-origin platform login")
	}
	back, err := url.Parse(target.Query().Get("return_to"))
	if err != nil || back.Scheme != "" || back.Host != "" || back.Path != "/oauth2/session-links/begin" {
		t.Fatal("login return target is not the fixed same-origin begin")
	}
	if len(back.Query()) != 4 {
		t.Fatal("untrusted extra query parameters were preserved")
	}
	for _, key := range []string{"client_id", "redirect_uri", "state", "nonce_challenge"} {
		if back.Query().Get(key) != original.Get(key) {
			t.Fatalf("original %s was not preserved", key)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("login redirect lacks privacy headers")
	}
}
