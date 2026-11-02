package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	filehttp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/interfaces/http"
)

// TestEvidenceMetadataRouteRequiresMachineAuthentication 断言只读证据元数据路由
// 已挂载且强制机器身份：未带 Bearer 的请求不会触达服务层，也不会因为路由缺失
// 被误判成"端点不存在"。
func TestEvidenceMetadataRouteRequiresMachineAuthentication(t *testing.T) {
	h := routes(&filehttp.Handler{}, &filehttp.UploadV2Handler{}, &filehttp.EvidenceHandler{}, tokenMiddleware{}, nil, nil, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/files/file-1/evidence-metadata", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("evidence metadata route status=%d, want 401", w.Code)
	}
}
