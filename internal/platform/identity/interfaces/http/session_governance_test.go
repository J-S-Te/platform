package identityhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	sessiondomain "github.com/J-S-Te/Basic-Platform/internal/platform/sessiongovernance/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type unavailableBrowserAuthenticator struct{ err error }

// Authenticate 返回注入的认证结果，检验缺Cookie与依赖故障的区别。
// 参数：请求上下文及Cookie原文。
// 返回值：空主体及指定错误。
func (a unavailableBrowserAuthenticator) Authenticate(context.Context, string) (authctx.Principal, error) {
	return authctx.Principal{}, a.err
}

// TestCentralCallbackDependencyClassification 验证OIDC回调不将依赖故障解释为新浏览器。
// 参数：t为HTTP边界测试上下文。
// 返回值：无；缺失、失效、冲突和依赖失败分类混淆时失败。
func TestCentralCallbackDependencyClassification(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	request.AddCookie(&http.Cookie{Name: "session", Value: "opaque"})
	outage := errors.New("isolated database unavailable")
	if _, err := trustedBrowserPrincipal(request, "session", unavailableBrowserAuthenticator{err: outage}); !errors.Is(err, outage) {
		t.Fatal("dependency failure was treated as a missing browser anchor")
	}
	if value, err := trustedBrowserPrincipal(request, "session", unavailableBrowserAuthenticator{err: application.ErrUnauthenticated}); err != nil || value != nil {
		t.Fatal("confirmed expired cookie must require a fresh authentication")
	}
	for _, item := range []struct {
		err    error
		status int
	}{
		{outage, 503}, {sessiondomain.ErrInactive, 401}, {sessiondomain.ErrBindingConflict, 409},
	} {
		w := httptest.NewRecorder()
		writeCentralSessionFailure(w, request, item.err)
		if w.Code != item.status {
			t.Fatalf("status=%d want %d", w.Code, item.status)
		}
	}
}
