package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	filehttp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/interfaces/http"
)

func TestBindingReleaseRouteRequiresMachineAuthentication(t *testing.T) {
	// 未认证请求不会执行任何服务逻辑；如果路由不存在，
	// 只会返回 404，无法验证实际的机器认证边界。
	h := routes(&filehttp.Handler{}, &filehttp.UploadV2Handler{}, tokenMiddleware{}, nil, nil, nil)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/files/file-1/bindings/release", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("release route status=%d, want 401, body=%s", w.Code, w.Body.String())
	}
}
