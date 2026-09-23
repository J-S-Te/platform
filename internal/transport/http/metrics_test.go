package httptransport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// 安全审查 SEC-B4：队列计数带10s TTL 缓存——TTL 内重复抓取不重复执行 DB COUNT，
// 过期后下一次抓取刷新一次（注入 gatherQueues 以观测采集次数）。
func TestOperationalMetricsCachesQueueCountsWithinTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gathers := 0
	metrics := NewOperationalMetrics(nil)
	metrics.gatherQueues = func(context.Context) queueMetricsSnapshot {
		gathers++
		return queueMetricsSnapshot{counts: []queueCountValue{{name: "platform_async_job_backlog", count: 7}}}
	}
	scrape := func() string {
		recorder := httptest.NewRecorder()
		metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return recorder.Body.String()
	}

	body := scrape()
	if gathers != 1 {
		t.Fatalf("gather calls = %d, want 1 on first scrape", gathers)
	}
	if !strings.Contains(body, "platform_async_job_backlog 7") {
		t.Fatalf("metrics missing cached queue count:\n%s", body)
	}
	if body = scrape(); gathers != 1 {
		t.Fatalf("gather calls = %d, want cache hit (TTL 内不重复 COUNT)", gathers)
	}

	// 模拟 TTL 过期：下一次抓取必须刷新快照。
	metrics.queueExpires = time.Now().Add(-time.Second)
	scrape()
	if gathers != 2 {
		t.Fatalf("gather calls = %d, want 2 after TTL expiry", gathers)
	}
}
