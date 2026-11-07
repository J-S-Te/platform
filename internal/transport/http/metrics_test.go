package httptransport

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
	"github.com/J-S-Te/Basic-Platform/internal/transport/http/middleware"
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
		`platform_http_requests_total{method="GET",route="/protected",status="4xx"} 1`,
		`platform_http_errors_total{method="GET",route="/protected",status="4xx"} 1`,
		"platform_authz_failures_total 1",
		`platform_http_request_duration_seconds_count{method="GET",route="/protected",status="4xx"} 1`,
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

// BIZ-XEVT2-07 监控面：缺登记 RP 与不可投递 backlog 必须以固定 gauge 名出现在 /metrics，
// 并且和队列计数共用同一 10s TTL 快照（TTL 内重复抓取不再查库）。
func TestOperationalMetricsExposesBackchannelLogoutGauges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gathers := 0
	metrics := NewOperationalMetrics(nil)
	metrics.gatherQueues = func(context.Context) queueMetricsSnapshot {
		return queueMetricsSnapshot{}
	}
	metrics.gatherBackchannel = func(context.Context) ([]backchannel.Gauge, error) {
		gathers++
		return []backchannel.Gauge{
			{Name: backchannel.MetricUnregisteredRPs, Value: 3},
			{Name: backchannel.MetricNoEndpointBacklog, Value: 7},
		}, nil
	}
	scrape := func() string {
		recorder := httptest.NewRecorder()
		metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return recorder.Body.String()
	}

	body := scrape()
	for _, expected := range []string{
		"# TYPE platform_oauth_backchannel_unregistered_rp gauge",
		"platform_oauth_backchannel_unregistered_rp 3",
		"# TYPE platform_oauth_backchannel_logout_no_endpoint gauge",
		"platform_oauth_backchannel_logout_no_endpoint 7",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if gathers != 1 {
		t.Fatalf("back-channel gather calls = %d, want 1 on first scrape", gathers)
	}
	if body = scrape(); gathers != 1 {
		t.Fatalf("back-channel gather calls = %d, want cache hit within TTL", gathers)
	}
	metrics.queueExpires = time.Now().Add(-time.Second)
	scrape()
	if gathers != 2 {
		t.Fatalf("back-channel gather calls = %d, want 2 after TTL expiry", gathers)
	}
}

// 采集失败 ≠ 缺口为 0：DB 不可用时这两条 gauge 必须整体缺席（并把失败计入
// platform_metric_collection_errors_total），绝不输出 "0" 让告警误判为健康。
func TestOperationalMetricsSkipsBackchannelGaugesOnCollectionFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := NewOperationalMetrics(nil)
	metrics.gatherQueues = func(context.Context) queueMetricsSnapshot {
		return queueMetricsSnapshot{counts: []queueCountValue{{name: "platform_async_job_backlog", count: 2}}}
	}
	metrics.gatherBackchannel = func(context.Context) ([]backchannel.Gauge, error) {
		return nil, errors.New("dial tcp: database is down")
	}

	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()

	for _, absent := range []string{
		backchannel.MetricUnregisteredRPs,
		backchannel.MetricNoEndpointBacklog,
	} {
		if strings.Contains(body, absent) {
			t.Fatalf("failed back-channel collection leaked %q as a value:\n%s", absent, body)
		}
	}
	// 其它指标照常输出，采集失败不拖垮整个 /metrics。
	if !strings.Contains(body, "platform_async_job_backlog 2") {
		t.Fatalf("queue metrics blocked by back-channel failure:\n%s", body)
	}
	if !strings.Contains(body, "platform_metric_collection_errors_total 1") {
		t.Fatalf("back-channel collection failure was not counted:\n%s", body)
	}
}

// BIZ-RUNTIME-05 监控面：Secret 的 status 分布（可按 status 聚合）、按 client_id 的
// ACTIVE 数量、最老 ACTIVE 龄期、超过 90 天的 ACTIVE 总数和吊销总数必须出现在 /metrics，
// 且与队列计数共用同一 10s TTL 快照。
func TestOperationalMetricsExposesClientSecretGauges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gathers := 0
	metrics := NewOperationalMetrics(nil)
	metrics.gatherQueues = func(context.Context) queueMetricsSnapshot { return queueMetricsSnapshot{} }
	metrics.gatherSecrets = func(context.Context) ([]queueCountValue, error) {
		gathers++
		return []queueCountValue{
			{name: metricClientSecretActive, labels: `{status="ACTIVE"}`, count: 4},
			{name: metricClientSecretActive, labels: `{status="REVOKED"}`, count: 9},
			{name: metricClientSecretActiveClient, labels: `{client_id="contract-client"}`, count: 3},
			{name: metricClientSecretActiveClient, labels: `{client_id="portal-client"}`, count: 1},
			{name: metricClientSecretActiveClients, count: 2},
			{name: metricClientSecretOldestActiveSeconds, count: 7776000},
			{name: metricClientSecretActiveOver90Days, count: 1},
			{name: metricClientSecretRevokedTotal, count: 9, metricType: "counter"},
		}, nil
	}
	scrape := func() string {
		recorder := httptest.NewRecorder()
		metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return recorder.Body.String()
	}

	body := scrape()
	for _, expected := range []string{
		"# TYPE platform_oauth_client_secret_active gauge",
		`platform_oauth_client_secret_active{status="ACTIVE"} 4`,
		`platform_oauth_client_secret_active{status="REVOKED"} 9`,
		"# TYPE platform_oauth_client_secret_active_client gauge",
		`platform_oauth_client_secret_active_client{client_id="contract-client"} 3`,
		`platform_oauth_client_secret_active_client{client_id="portal-client"} 1`,
		"# TYPE platform_oauth_client_secret_active_clients gauge",
		"platform_oauth_client_secret_active_clients 2",
		"# TYPE platform_oauth_client_secret_oldest_active_seconds gauge",
		"platform_oauth_client_secret_oldest_active_seconds 7776000",
		"# TYPE platform_oauth_client_secret_active_over_90d gauge",
		"platform_oauth_client_secret_active_over_90d 1",
		"# TYPE platform_oauth_client_secret_revoked_total counter",
		"platform_oauth_client_secret_revoked_total 9",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	// 带 status 标签的多条样本只允许一条 TYPE 行，否则 Prometheus 文本解析器会拒绝整个抓取。
	if count := strings.Count(body, "# TYPE platform_oauth_client_secret_active "); count != 1 {
		t.Fatalf("platform_oauth_client_secret_active TYPE lines = %d, want 1", count)
	}
	// client_id 维度同样只能声明一次 TYPE，否则整个抓取会被拒绝。
	if count := strings.Count(body, "# TYPE platform_oauth_client_secret_active_client "); count != 1 {
		t.Fatalf("platform_oauth_client_secret_active_client TYPE lines = %d, want 1", count)
	}
	if gathers != 1 {
		t.Fatalf("secret gather calls = %d, want 1 on first scrape", gathers)
	}
	if body = scrape(); gathers != 1 {
		t.Fatalf("secret gather calls = %d, want cache hit within TTL", gathers)
	}
	metrics.queueExpires = time.Now().Add(-time.Second)
	scrape()
	if gathers != 2 {
		t.Fatalf("secret gather calls = %d, want 2 after TTL expiry", gathers)
	}
}

// 采集失败不伪 0：DB 不可用时 Secret 指标整体缺席并计入采集错误；输出 0 会让
// "密钥龄期 > 90 天"告警永久静默。
func TestOperationalMetricsSkipsClientSecretGaugesOnCollectionFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := NewOperationalMetrics(nil)
	metrics.gatherQueues = func(context.Context) queueMetricsSnapshot {
		return queueMetricsSnapshot{counts: []queueCountValue{{name: "platform_async_job_backlog", count: 2}}}
	}
	metrics.gatherSecrets = func(context.Context) ([]queueCountValue, error) {
		return nil, errors.New("dial tcp: database is down")
	}

	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()

	for _, absent := range []string{
		metricClientSecretActive,
		metricClientSecretActiveClient,
		metricClientSecretActiveClients,
		metricClientSecretOldestActiveSeconds,
		metricClientSecretActiveOver90Days,
		metricClientSecretRevokedTotal,
	} {
		if strings.Contains(body, absent) {
			t.Fatalf("failed secret collection leaked %q as a value:\n%s", absent, body)
		}
	}
	if !strings.Contains(body, "platform_async_job_backlog 2") {
		t.Fatalf("queue metrics blocked by secret failure:\n%s", body)
	}
	if !strings.Contains(body, "platform_metric_collection_errors_total 1") {
		t.Fatalf("secret collection failure was not counted:\n%s", body)
	}
}

// 安全审查 SEC-B4 自我计量豁免：/metrics 的响应正文就是这些计数器，抓取自身不得
// 计入 requests/duration；否则抓取频率会直接改写被观测值（自我计量的正反馈）。
// 普通业务路径的计数语义必须保持原样——豁免只针对 /metrics 一个路径。
func TestOperationalMetricsMiddlewareSkipsSelfScrape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := NewOperationalMetrics(nil)
	router := gin.New()
	router.Use(metrics.Middleware())
	router.GET(metricsPath, gin.WrapH(metrics))
	router.GET("/protected", func(context *gin.Context) { context.Status(http.StatusForbidden) })

	scrape := func() {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, metricsPath, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("scrape status = %d, want 200", recorder.Code)
		}
	}

	// 连续两次抓取：计数必须还是 0，不能因为抓取自己而增长。
	scrape()
	scrape()
	body := httptest.NewRecorder()
	metrics.ServeHTTP(body, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	// 一条业务请求都没有时，带标签的 HTTP 指标**没有样本**（只有 TYPE 行）：
	// 计数为 0 是“没有请求”，不应该伪造出一条全零序列，也不允许出现 route="/metrics"
	// 这样的自抓取序列。
	for _, unexpected := range []string{
		"platform_http_requests_total{",
		"platform_http_errors_total{",
		"platform_http_request_duration_seconds_bucket{",
		"platform_http_request_duration_seconds_count{",
		`route="` + metricsPath + `"`,
	} {
		if strings.Contains(body.Body.String(), unexpected) {
			t.Fatalf("自抓取污染了计数，metrics 不应含 %q:\n%s", unexpected, body.Body.String())
		}
	}

	// 普通路径仍然计数（含错误与授权失败），确认豁免没有扩大到其它路由。
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
	scrape()
	body = httptest.NewRecorder()
	metrics.ServeHTTP(body, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	for _, expected := range []string{
		`platform_http_requests_total{method="GET",route="/protected",status="4xx"} 1`,
		`platform_http_errors_total{method="GET",route="/protected",status="4xx"} 1`,
		"platform_authz_failures_total 1",
		`platform_http_request_duration_seconds_count{method="GET",route="/protected",status="4xx"} 1`,
	} {
		if !strings.Contains(body.Body.String(), expected) {
			t.Fatalf("普通路径计数语义被改变，metrics missing %q:\n%s", expected, body.Body.String())
		}
	}
}

// 未授权抓取（RequireMetricsAccess 401/403）同样不计入 authz_failures：
// 令牌配错是抓取端配置问题，不该伪装成业务侧“登录/鉴权被拒”触发告警。
func TestOperationalMetricsMiddlewareSkipsRejectedScrapeAuthzFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := NewOperationalMetrics(nil)
	router := gin.New()
	router.Use(metrics.Middleware())
	router.GET(metricsPath, middleware.RequireMetricsAccess("expected-token"), gin.WrapH(metrics))

	// 无 Authorization 头：RequireMetricsAccess 返回 401（token 已配置时）。
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, metricsPath, nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", response.Code)
		}
	}
	body := httptest.NewRecorder()
	metrics.ServeHTTP(body, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	for _, unexpected := range []string{
		"platform_http_requests_total{",
		"platform_authz_failures_total 1",
		"platform_http_errors_total{",
	} {
		if strings.Contains(body.Body.String(), unexpected) {
			t.Fatalf("被拒绝的抓取污染了计数，metrics 不应含 %q:\n%s", unexpected, body.Body.String())
		}
	}
	// authz_failures 与 HTTP 序列无关，必须仍是明确的 0。
	if !strings.Contains(body.Body.String(), "platform_authz_failures_total 0") {
		t.Fatalf("authz_failures 被拒绝的抓取污染:\n%s", body.Body.String())
	}
}

// 标签值必须转义：未转义的引号/换行会破坏整个抓取结果。
func TestEscapeMetricLabelValue(t *testing.T) {
	cases := map[string]string{
		"ACTIVE":      "ACTIVE",
		`say "hi"`:    `say \"hi\"`,
		`back\slash`:  `back\\slash`,
		"line\nbreak": "line\\nbreak",
	}
	for input, expected := range cases {
		if got := escapeMetricLabelValue(input); got != expected {
			t.Fatalf("escape(%q) = %q, want %q", input, got, expected)
		}
	}
}

// scrapeOperationalMetrics 直接调用 ServeHTTP 触发一次抓取（不经过 HTTP 中间件，
// 避免测 HTTP 指标时又被 Router 的自我计量逻辑干扰）。
func scrapeOperationalMetrics(t *testing.T, metrics *OperationalMetrics) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	return recorder.Body.String()
}

// quietMetrics 返回一个不会写入采集错误的收集器（gatherQueues 已注入空快照），
// 使多次抓取的正文完全可比。
func quietMetrics() *OperationalMetrics {
	metrics := NewOperationalMetrics(nil)
	metrics.gatherQueues = func(context.Context) queueMetricsSnapshot { return queueMetricsSnapshot{} }
	return metrics
}

// C1 标签面：requests/errors/duration 共用 {method,route,status} 序列集。route 取 gin
// 注册模式（FullPath），因此 /api/v1/users/42 与 /api/v1/users/99 落在同一条序列上，
// 真实路径与查询串永远不进 exposition。
func TestOperationalMetricsLabelsHTTPDimensions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := quietMetrics()
	router := gin.New()
	router.Use(metrics.Middleware())
	router.GET("/api/v1/users/:id", func(ginContext *gin.Context) { ginContext.Status(http.StatusInternalServerError) })
	router.POST("/api/v1/users/:id", func(ginContext *gin.Context) { ginContext.Status(http.StatusNoContent) })

	for _, target := range []string{"/api/v1/users/42?token=super-secret", "/api/v1/users/99"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	created := httptest.NewRecorder()
	router.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/v1/users/7", nil))
	if created.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204", created.Code)
	}

	body := scrapeOperationalMetrics(t, metrics)
	for _, expected := range []string{
		"# TYPE platform_http_requests_total counter",
		"# TYPE platform_http_errors_total counter",
		"# TYPE platform_http_request_duration_seconds histogram",
		`platform_http_requests_total{method="GET",route="/api/v1/users/:id",status="5xx"} 2`,
		`platform_http_errors_total{method="GET",route="/api/v1/users/:id",status="5xx"} 2`,
		`platform_http_requests_total{method="POST",route="/api/v1/users/:id",status="2xx"} 1`,
		`platform_http_errors_total{method="POST",route="/api/v1/users/:id",status="2xx"} 0`,
		`platform_http_request_duration_seconds_count{method="POST",route="/api/v1/users/:id",status="2xx"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	// 原始路径、路径参数与查询串一律不得出现在 exposition（PII 与基数泄漏防线）。
	for _, leaked := range []string{
		"/api/v1/users/42", "/api/v1/users/99", "/api/v1/users/7",
		"super-secret", "token=",
	} {
		if strings.Contains(body, leaked) {
			t.Fatalf("exposition leaked request detail %q:\n%s", leaked, body)
		}
	}
}

// 未匹配路由（404/405）的 FullPath 为空：必须固定记 "__unmatched__"，绝不退回
// Request.URL.Path——否则任意扫描路径都会新建一条序列，并把查询串写进指标。
func TestOperationalMetricsUnmatchedRouteNeverLeaksRequestPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := quietMetrics()
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.Use(metrics.Middleware())
	router.GET("/only-get", func(ginContext *gin.Context) { ginContext.Status(http.StatusOK) })

	scanned := "/wp-admin/setup.php?api_key=SECRET-VALUE"
	notFound := httptest.NewRecorder()
	router.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, scanned, nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("scan status = %d, want 404", notFound.Code)
	}
	wrongMethod := httptest.NewRecorder()
	router.ServeHTTP(wrongMethod, httptest.NewRequest(http.MethodPost, "/only-get", nil))
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method-not-allowed status = %d, want 405", wrongMethod.Code)
	}

	body := scrapeOperationalMetrics(t, metrics)
	for _, expected := range []string{
		`platform_http_requests_total{method="GET",route="__unmatched__",status="4xx"} 1`,
		`platform_http_requests_total{method="POST",route="__unmatched__",status="4xx"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	for _, leaked := range []string{"wp-admin", "setup.php", "SECRET-VALUE", "api_key", "/only-get\""} {
		if strings.Contains(body, leaked) {
			t.Fatalf("unmatched 路径泄漏 %q 到 exposition:\n%s", leaked, body)
		}
	}
}

// method 白名单：非白名单方法折叠成 OTHER，避免任意方法名制造新序列。
func TestOperationalMetricsMethodWhitelistFoldsUnknownMethod(t *testing.T) {
	metrics := quietMetrics()
	metrics.recordHTTPRequest("BREW", "/sync", http.StatusOK, time.Millisecond)
	metrics.recordHTTPRequest("PROPFIND", "/sync", http.StatusOK, time.Millisecond)
	metrics.recordHTTPRequest(http.MethodGet, "/sync", http.StatusOK, time.Millisecond)

	body := scrapeOperationalMetrics(t, metrics)
	for _, expected := range []string{
		`platform_http_requests_total{method="OTHER",route="/sync",status="2xx"} 2`,
		`platform_http_requests_total{method="GET",route="/sync",status="2xx"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	for _, unexpected := range []string{`method="BREW"`, `method="PROPFIND"`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("非白名单方法 %q 出现在 exposition:\n%s", unexpected, body)
		}
	}
}

// status 分桶与错误口径：errors_total 仍是“status >= 400”的同一序列计数，
// 因此 4xx/5xx 两条序列各自累加，1xx/2xx/3xx 恒为 0。
func TestOperationalMetricsStatusBucketsAndErrors(t *testing.T) {
	metrics := quietMetrics()
	cases := []struct {
		statusCode int
		bucket     string
		isError    bool
	}{
		{statusCode: 100, bucket: "1xx"},
		{statusCode: 204, bucket: "2xx"},
		{statusCode: 302, bucket: "3xx"},
		{statusCode: 400, bucket: "4xx", isError: true},
		{statusCode: 503, bucket: "5xx", isError: true},
	}
	for _, testCase := range cases {
		metrics.recordHTTPRequest(http.MethodGet, "/status/"+testCase.bucket, testCase.statusCode, time.Millisecond)
	}

	body := scrapeOperationalMetrics(t, metrics)
	for _, testCase := range cases {
		labels := `{method="GET",route="/status/` + testCase.bucket + `",status="` + testCase.bucket + `"}`
		if want := "platform_http_requests_total" + labels + " 1"; !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
		wantErrors := "platform_http_errors_total" + labels + " 0"
		if testCase.isError {
			wantErrors = "platform_http_errors_total" + labels + " 1"
		}
		if !strings.Contains(body, wantErrors) {
			t.Fatalf("metrics missing %q:\n%s", wantErrors, body)
		}
	}
}

// duration 是 histogram：桶必须是累计值、单调不减，le 与代码桶上界一一对应，
// +Inf 桶恒等于 _count；_sum 保持“纳秒整数累加后 %.9f”的原精度口径。
func TestOperationalMetricsDurationHistogramBuckets(t *testing.T) {
	metrics := quietMetrics()
	for _, elapsed := range []time.Duration{time.Millisecond, 7 * time.Millisecond, 300 * time.Millisecond, 20 * time.Second} {
		metrics.recordHTTPRequest(http.MethodGet, "/hist", http.StatusOK, elapsed)
	}

	body := scrapeOperationalMetrics(t, metrics)
	series := `platform_http_request_duration_seconds_bucket{method="GET",route="/hist",status="2xx"`
	bounds, counts := parseHTTPHistogram(t, body, series)
	if len(counts) != httpDurationBucketCount+1 {
		t.Fatalf("bucket 行数 = %d, want %d（含 +Inf）:\n%s", len(counts), httpDurationBucketCount+1, body)
	}
	for index := range httpDurationBuckets {
		if bounds[index] != httpDurationBuckets[index] {
			t.Fatalf("第 %d 个 le = %v, want %v", index, bounds[index], httpDurationBuckets[index])
		}
	}
	if !math.IsInf(bounds[len(bounds)-1], 1) {
		t.Fatalf("最后一个桶 le = %v, want +Inf", bounds[len(bounds)-1])
	}
	// 1ms→[0.005,+∞), 7ms→[0.01,+∞), 300ms→[0.5,+∞), 20s→只有 +Inf。
	wantCounts := []uint64{1, 2, 2, 2, 2, 2, 3, 3, 3, 3, 3, 4}
	for index := range wantCounts {
		if counts[index] != wantCounts[index] {
			t.Fatalf("桶 %v 累计值 = %d, want %d（counts=%v）:\n%s", bounds[index], counts[index], wantCounts[index], counts, body)
		}
	}
	for index := 1; index < len(counts); index++ {
		if counts[index] < counts[index-1] {
			t.Fatalf("桶非单调：le=%v 的 %d 小于前一个 %d:\n%s", bounds[index], counts[index], counts[index-1], body)
		}
	}
	for _, expected := range []string{
		`platform_http_request_duration_seconds_bucket{method="GET",route="/hist",status="2xx",le="+Inf"} 4`,
		`platform_http_request_duration_seconds_count{method="GET",route="/hist",status="2xx"} 4`,
		`platform_http_request_duration_seconds_sum{method="GET",route="/hist",status="2xx"} 20.308000000`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	// 多序列也只允许一条 TYPE 行，否则整个抓取会被 Prometheus 文本解析器拒绝。
	for _, metric := range []string{
		metricHTTPRequestsTotal, metricHTTPErrorsTotal, metricHTTPDurationSeconds,
	} {
		if declared := strings.Count(body, "# TYPE "+metric+" "); declared != 1 {
			t.Fatalf("%s 的 TYPE 行 = %d, want 1:\n%s", metric, declared, body)
		}
	}
}

// 基数硬上限：达到 maxHTTPSeries 后新组合折叠进 {method="OTHER",route="__overflow__",
// status="other"} 并继续计数（请求量不丢），既有组合仍然累加到自己的序列。
func TestOperationalMetricsHTTPSeriesOverflowFolds(t *testing.T) {
	metrics := quietMetrics()
	for index := 0; index < maxHTTPSeries; index++ {
		metrics.recordHTTPRequest(http.MethodGet, fmt.Sprintf("/route-%04d", index), http.StatusOK, time.Millisecond)
	}
	if seriesCount := seriesCountOf(metrics); seriesCount != maxHTTPSeries {
		t.Fatalf("序列数 = %d, want %d", seriesCount, maxHTTPSeries)
	}

	for index := 0; index < 3; index++ {
		metrics.recordHTTPRequest(http.MethodPost, fmt.Sprintf("/late-%04d", index), http.StatusInternalServerError, time.Millisecond)
	}
	// 已存在的组合不被折叠：仍然累加到自己那条序列。
	metrics.recordHTTPRequest(http.MethodGet, "/route-0000", http.StatusOK, time.Millisecond)

	if seriesCount := seriesCountOf(metrics); seriesCount != maxHTTPSeries+1 {
		t.Fatalf("超限后序列数 = %d, want %d（含 1 个溢出序列）", seriesCount, maxHTTPSeries+1)
	}
	body := scrapeOperationalMetrics(t, metrics)
	for _, expected := range []string{
		`platform_http_requests_total{method="OTHER",route="__overflow__",status="other"} 3`,
		`platform_http_errors_total{method="OTHER",route="__overflow__",status="other"} 3`,
		`platform_http_requests_total{method="GET",route="/route-0000",status="2xx"} 2`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	// 折叠只压缩序列数，不丢请求量：requests_total 所有样本之和必须等于请求总数。
	samples := parseHTTPSampleValues(t, body, metricHTTPRequestsTotal)
	var total uint64
	for _, sample := range samples {
		total += sample
	}
	if len(samples) != maxHTTPSeries+1 {
		t.Fatalf("requests_total 样本数 = %d, want %d", len(samples), maxHTTPSeries+1)
	}
	if want := uint64(maxHTTPSeries + 3 + 1); total != want {
		t.Fatalf("requests_total 样本之和 = %d, want %d（折叠不得丢请求量）", total, want)
	}
}

func seriesCountOf(metrics *OperationalMetrics) int {
	metrics.httpMu.RLock()
	defer metrics.httpMu.RUnlock()
	return len(metrics.httpSeries)
}

// parseHTTPHistogram 解析一条序列的 _bucket 行，返回按输出顺序的 le 与累计值。
func parseHTTPHistogram(t *testing.T, body, seriesPrefix string) ([]float64, []uint64) {
	t.Helper()
	bounds := make([]float64, 0, httpDurationBucketCount+1)
	counts := make([]uint64, 0, httpDurationBucketCount+1)
	for _, line := range strings.Split(body, "\n") {
		rest, found := strings.CutPrefix(line, seriesPrefix+`,le="`)
		if !found {
			continue
		}
		boundText, rest, found := strings.Cut(rest, `"}`)
		if !found {
			t.Fatalf("桶行格式异常: %q", line)
		}
		bound, err := strconv.ParseFloat(boundText, 64)
		if err != nil {
			t.Fatalf("le 解析失败 %q: %v", line, err)
		}
		fields := strings.Fields(rest)
		value, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatalf("桶计数解析失败 %q: %v", line, err)
		}
		bounds = append(bounds, bound)
		counts = append(counts, value)
	}
	return bounds, counts
}

// parseHTTPSampleValues 取出指定指标全部带标签样本的值（用于总量守恒断言）。
func parseHTTPSampleValues(t *testing.T, body, metric string) []uint64 {
	t.Helper()
	values := make([]uint64, 0, maxHTTPSeries+1)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatalf("样本值解析失败 %q: %v", line, err)
		}
		values = append(values, value)
	}
	return values
}

// 并发安全：中间件在每请求 goroutine 上累加、抓取在另一 goroutine 上排序输出，
// 两者必须无数据竞争且计数不丢（用 -race 运行时这条用例就是竞态探针）。
func TestOperationalMetricsConcurrentRecordAndScrape(t *testing.T) {
	metrics := quietMetrics()
	const workers = 8
	const perWorker = 50
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for index := 0; index < perWorker; index++ {
				metrics.recordHTTPRequest(http.MethodGet, "/concurrent", http.StatusOK, time.Millisecond)
			}
		}()
	}
	// 与写入并发的抓取：只要求不竞态、不 panic。
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		for scrapeIndex := 0; scrapeIndex < 10; scrapeIndex++ {
			metrics.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, metricsPath, nil))
		}
	}()
	waitGroup.Wait()

	body := scrapeOperationalMetrics(t, metrics)
	want := `platform_http_requests_total{method="GET",route="/concurrent",status="2xx"} 400`
	if !strings.Contains(body, want) {
		t.Fatalf("并发累加丢计数，metrics missing %q:\n%s", want, body)
	}
}
