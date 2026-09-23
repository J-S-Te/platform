package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 安全审查 SEC-B4：无凭据访问 /metrics 必须被拒——
// 未配置令牌时仅回环对端放行；配置令牌后只有持 Bearer 令牌的抓取端放行。
func TestRequireMetricsAccessRejectsUnauthenticatedScrapes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		token      string
		remoteAddr string
		authHeader string
		wantStatus int
	}{
		{name: "无令牌配置且回环对端放行", remoteAddr: "127.0.0.1:5678", wantStatus: http.StatusNoContent},
		{name: "无令牌配置且非回环对端拒绝", remoteAddr: "172.31.255.1:5678", wantStatus: http.StatusForbidden},
		{name: "配置令牌后无凭据拒绝", token: "s3cr3t-metrics-token", remoteAddr: "127.0.0.1:5678", wantStatus: http.StatusUnauthorized},
		{name: "配置令牌后错误凭据拒绝", token: "s3cr3t-metrics-token", authHeader: "Bearer wrong-token", remoteAddr: "127.0.0.1:5678", wantStatus: http.StatusUnauthorized},
		{name: "配置令牌后正确凭据放行", token: "s3cr3t-metrics-token", authHeader: "Bearer s3cr3t-metrics-token", remoteAddr: "198.51.100.7:5678", wantStatus: http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/metrics", RequireMetricsAccess(test.token), func(context *gin.Context) {
				context.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			request.RemoteAddr = test.remoteAddr
			if test.authHeader != "" {
				request.Header.Set("Authorization", test.authHeader)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}
