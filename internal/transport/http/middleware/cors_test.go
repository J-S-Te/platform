package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 审计导出下载必须携带 X-Audit-Export-Ticket 一次性票据头；跨源控制台在预检阶段
// 必须被明确放行该头，否则浏览器会在发出下载请求前丢弃它。
func TestCORSAllowsAuditExportTicketHeaderOnPreflight(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(CORS([]string{"https://console.example.com"}))
	router.GET("/api/v1/audit/export-jobs/job-1/download", func(context *gin.Context) { context.Status(http.StatusOK) })

	request := httptest.NewRequest(http.MethodOptions, "/api/v1/audit/export-jobs/job-1/download", nil)
	request.Header.Set("Origin", "https://console.example.com")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	request.Header.Set("Access-Control-Request-Headers", "x-audit-export-ticket")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", response.Code)
	}
	if allowed := response.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(allowed, "X-Audit-Export-Ticket") {
		t.Fatalf("Access-Control-Allow-Headers = %q, want X-Audit-Export-Ticket", allowed)
	}
}
