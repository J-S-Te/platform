package httptransport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOperationalMetricsRecordsHTTPAndAuthorizationFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := NewOperationalMetrics(nil)
	router := gin.New()
	router.Use(metrics.Middleware())
	router.GET("/protected", func(context *gin.Context) { context.Status(http.StatusForbidden) })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))

	metricsResponse := httptest.NewRecorder()
	metrics.ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metricsResponse.Body.String()
	for _, expected := range []string{
		"platform_http_requests_total 1", "platform_http_errors_total 1", "platform_authz_failures_total 1",
		"platform_http_request_duration_seconds_count 1",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
}
