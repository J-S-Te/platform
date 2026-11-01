package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	identity "github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/gin-gonic/gin"
)

type sessionDependencyAuthenticator struct{ err error }

// Authenticate 返回指定的认证依赖结果，测试HTTP层是否正确区分失效与故障。
// 参数：请求上下文与Cookie令牌。
// 返回值：空主体及预设错误。
func (a sessionDependencyAuthenticator) Authenticate(context.Context, string) (authctx.Principal, error) {
	return authctx.Principal{}, a.err
}

// TestSessionDependencyFailurePreservesAuthentication 验证数据库或中央服务故障不强制登出。
// 参数：t为测试上下文。
// 返回值：无；明确未认证应401，未知依赖错误应503，业务处理器均不得执行。
func TestSessionDependencyFailurePreservesAuthentication(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
	}{{identity.ErrUnauthenticated, 401}, {errors.New("temporary database fault"), 503}} {
		router := gin.New()
		router.Use(Authentication(sessionDependencyAuthenticator{test.err}, "bp_session"))
		router.GET("/", func(c *gin.Context) { t.Error("protected handler ran on failed authentication") })
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: "bp_session", Value: "test-token"})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("dependency classification: got%d want%d", w.Code, test.status)
		}
	}
}
