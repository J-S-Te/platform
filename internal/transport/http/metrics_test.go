package httptransport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	for _, expected := range []string{
		"platform_http_requests_total 0",
		"platform_http_request_duration_seconds_count 0",
		"platform_http_errors_total 0",
	} {
		if !strings.Contains(body.Body.String(), expected) {
			t.Fatalf("自抓取污染了计数，metrics missing %q:\n%s", expected, body.Body.String())
		}
	}

	// 普通路径仍然计数（含错误与授权失败），确认豁免没有扩大到其它路由。
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/protected", nil))
	scrape()
	body = httptest.NewRecorder()
	metrics.ServeHTTP(body, httptest.NewRequest(http.MethodGet, metricsPath, nil))
	for _, expected := range []string{
		"platform_http_requests_total 1",
		"platform_http_errors_total 1",
		"platform_authz_failures_total 1",
		"platform_http_request_duration_seconds_count 1",
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
	for _, expected := range []string{
		"platform_http_requests_total 0",
		"platform_authz_failures_total 0",
		"platform_http_errors_total 0",
	} {
		if !strings.Contains(body.Body.String(), expected) {
			t.Fatalf("被拒绝的抓取污染了计数，metrics missing %q:\n%s", expected, body.Body.String())
		}
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
