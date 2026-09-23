package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 安全审查 SEC-B10：GET /oauth2/logout 的导航来源校验——跨站 GET 必须拒绝（403），
// 同站/直接导航（same-origin/none）必须放行，非浏览器客户端（无 Origin/Sec-Fetch-Site）兼容。
func TestRequireSameOriginNavigationBlocksCrossSiteGET(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const issuer = "https://platform.example.com"
	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{name: "跨站 Sec-Fetch-Site 拒绝", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, wantStatus: http.StatusForbidden},
		{name: "同站子域 Sec-Fetch-Site 拒绝", headers: map[string]string{"Sec-Fetch-Site": "same-site"}, wantStatus: http.StatusForbidden},
		{name: "跨站 Origin 拒绝", headers: map[string]string{"Origin": "https://evil.example.net"}, wantStatus: http.StatusForbidden},
		{name: "同源导航放行", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, wantStatus: http.StatusNoContent},
		{name: "地址栏直接导航放行", headers: map[string]string{"Sec-Fetch-Site": "none"}, wantStatus: http.StatusNoContent},
		{name: "同源 Origin 放行", headers: map[string]string{"Origin": "https://platform.example.com"}, wantStatus: http.StatusNoContent},
		{name: "非浏览器客户端无校验头放行", headers: nil, wantStatus: http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/oauth2/logout", RequireSameOriginNavigation(issuer), func(context *gin.Context) {
				context.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodGet, "/oauth2/logout", nil)
			for key, value := range test.headers {
				request.Header.Set(key, value)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}
