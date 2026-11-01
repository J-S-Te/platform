package sessionhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	store "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type failingVerifier struct{ err error }

// VerifySessionTokens 模拟签名服务不可用，不返回未经验证的身份。
// 参数：依次为请求、访问令牌、身份令牌、精确客户端和nonce摘要。
// 返回值：空身份及预设依赖错误。
func (v failingVerifier) VerifySessionTokens(context.Context, string, string, string, string) (store.VerifiedIdentity, error) {
	return store.VerifiedIdentity{}, v.err
}

type httpRepository struct{ Repository }

// TicketContext 提供固定测试票据约束，测试目标仍为实际HTTP错误行为。
// 参数：请求、测试票据和服务端时钟。
// 返回值：当前客户端及摘要，无错误。
func (httpRepository) TicketContext(context.Context, string, time.Time) (string, string, error) {
	return "client", "nonce", nil
}

type noCookieAuthenticator struct{}

// Authenticate 明确拒绝测试中未提供的浏览器Cookie。
// 参数：请求上下文及Cookie原文。
// 返回值：空主体及未认证错误。
func (noCookieAuthenticator) Authenticate(context.Context, string) (authctx.Principal, error) {
	return authctx.Principal{}, errors.New("unexpected cookie")
}

// TestVerifierOutageDoesNotForceLogout 验证签名依赖不可用不能冒充确定失效。
// 参数：t为测试上下文。
// 返回值：无；若响应为401或泄露依赖错误即失败。
func TestVerifierOutageDoesNotForceLogout(t *testing.T) {
	h, err := NewHandler(httpRepository{}, failingVerifier{err: errors.New("private upstream outage")}, noCookieAuthenticator{}, "bp_session")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/oauth2/session-links/complete", strings.NewReader(`{"ticket":"ticket","id_token":"id"}`))
	r.Header.Set("Authorization", "Bearer access")
	w := httptest.NewRecorder()
	h.Complete(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("dependency failure must remain503 without detail: %d %s", w.Code, w.Body.String())
	}
}

// TestProtocolRejectsMalformedInputWithoutRedirect 验证非法桥接不反射攻击目标。
// 参数：t为测试上下文。
// 返回值：无；任何非法输入进入仓储、回跳或返回敏感错误时失败。
func TestProtocolRejectsMalformedInputWithoutRedirect(t *testing.T) {
	h, _ := NewHandler(httpRepository{}, failingVerifier{}, noCookieAuthenticator{}, "bp_session")
	for _, query := range []string{"", "client_id=c&redirect_uri=https://attacker.test&state=x&nonce_challenge=x"} {
		w := httptest.NewRecorder()
		h.Begin(w, httptest.NewRequest(http.MethodGet, "/oauth2/session-links/begin?"+query, nil))
		if w.Code != 400 || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid begin: %d %v", w.Code, w.Header())
		}
	}
	for _, body := range []string{`{"ticket":"t","id_token":"id"} {}`, `{"ticket":"t","unknown":1}`, strings.Repeat("x", 33<<10)} {
		r := httptest.NewRequest(http.MethodPost, "/oauth2/session-links/complete", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer access")
		w := httptest.NewRecorder()
		h.Complete(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid JSON accepted: %d", w.Code)
		}
	}
}
