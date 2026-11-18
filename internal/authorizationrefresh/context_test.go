package authorizationrefresh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFromRequestRestrictsFreshReads 验证新鲜读取只接受受控的只读会话入口。
// 参数：t 为隔离测试上下文。
// 返回值：无；错误接受业务或写请求时测试失败。
func TestFromRequestRestrictsFreshReads(t *testing.T) {
	for _, item := range []struct {
		method, path, header string
		want                 bool
	}{
		{http.MethodGet, "/project_management/api/v1/auth/me", "1", true},
		{http.MethodGet, "/auth/session", "1", true},
		{http.MethodGet, "/oauth2/authorization-context", "1", true},
		{http.MethodGet, "/api/v1/auth/me", "", false},
		{http.MethodGet, "/api/v1/auth/me", "true", false},
		{http.MethodPost, "/api/v1/auth/me", "1", false},
		{http.MethodGet, "/api/v1/projects", "1", false},
		{http.MethodGet, "/api/v1/auth/me/other", "1", false},
	} {
		request := httptest.NewRequest(item.method, item.path, nil)
		request.Header.Set("X-Authorization-Refresh", item.header)
		if got := Forced(FromRequest(request.Context(), request)); got != item.want {
			t.Errorf("%s %s header=%q forced=%v want=%v", item.method, item.path, item.header, got, item.want)
		}
	}
}

// TestWithForcePreservesCancellation 验证授权刷新不替换原请求的取消与上下文。
// 参数：t 为隔离测试上下文。
// 返回值：无；上下文被替换或取消丢失时测试失败。
func TestWithForcePreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	forced := WithForce(ctx)
	if !Forced(forced) || Forced(ctx) || Forced(FromRequest(ctx, nil)) {
		t.Fatal("force marker must be local to the derived context")
	}
	cancel()
	if forced.Err() != context.Canceled {
		t.Fatal("request cancellation was lost")
	}
}
