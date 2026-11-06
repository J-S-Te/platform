package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	filehttp "github.com/J-S-Te/Basic-Platform/internal/platform/filetask/interfaces/http"
)

// file-gateway 请求级指标只接受固定路由模板枚举（upload/download/ticket/other），
// 原始路径绝不进入标签；状态码只分 2xx/4xx/5xx，其余（1xx/3xx/畸形码）归 other。
func TestFileGatewayRequestMetricsBucketRoutesAndStatuses(t *testing.T) {
	metrics := newFileGatewayRequestMetrics()
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/files", metrics.track(fileGatewayRouteUpload, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
	})))
	mux.Handle("GET /api/v1/files/{file_id}/content", metrics.track(fileGatewayRouteDownload, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	})))
	mux.Handle("POST /api/v2/files/{file_id}/download-tickets", metrics.track(fileGatewayRouteTicket, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	})))
	// 未显式标注的路由（健康检查、绑定、清理等）由外层兜底记 other。
	mux.HandleFunc("GET /livez", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
	handler := metrics.wrap(mux)

	for _, testCase := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/files"},
		{http.MethodGet, "/api/v1/files/file-1/content"},
		{http.MethodPost, "/api/v2/files/file-1/download-tickets"},
		{http.MethodGet, "/livez"},
		{http.MethodGet, "/api/v2/unknown/scan?token=leaked-secret"},
	} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(testCase.method, testCase.target, nil))
	}

	body := scrapeFileGatewayMetrics(t, metrics)
	for _, expected := range []string{
		`file_gateway_http_requests_total{route="upload",status="2xx"} 1`,
		`file_gateway_http_requests_total{route="download",status="4xx"} 1`,
		`file_gateway_http_requests_total{route="ticket",status="5xx"} 1`,
		`file_gateway_http_requests_total{route="other",status="2xx"} 1`,
		`file_gateway_http_requests_total{route="other",status="4xx"} 1`,
		`file_gateway_http_request_duration_seconds_count{route="upload"} 1`,
		`file_gateway_http_request_duration_seconds_count{route="other"} 2`,
		`file_gateway_http_request_duration_seconds_count{route="download"} 1`,
		`file_gateway_http_request_duration_seconds_count{route="ticket"} 1`,
		`file_gateway_http_request_duration_seconds_bucket{route="upload",le="+Inf"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	// 禁止原始路径与查询串进入标签：那既是高基数入口，也会把下载票据/token 复制进指标。
	for _, forbidden := range []string{"/api/v1/files", "/livez", "/api/v2/unknown", "leaked-secret", `route="/api`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("raw path leaked into metrics: %q\n%s", forbidden, body)
		}
	}
}

// 耗时直方图的桶单调递增，+Inf 恒等于 _count（与平台 HTTP 面同一结构恒等式）。
func TestFileGatewayRequestMetricsDurationHistogramBuckets(t *testing.T) {
	metrics := newFileGatewayRequestMetrics()
	metrics.record(fileGatewayRouteDownload, http.StatusOK, 20*time.Millisecond)
	metrics.record(fileGatewayRouteDownload, http.StatusOK, 2*time.Second)
	metrics.record(fileGatewayRouteDownload, http.StatusFound, 45*time.Second)

	body := scrapeFileGatewayMetrics(t, metrics)
	for _, expected := range []string{
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="0.05"} 1`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="0.1"} 1`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="0.25"} 1`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="0.5"} 1`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="1"} 1`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="2.5"} 2`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="5"} 2`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="10"} 2`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="30"} 2`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="60"} 3`,
		`file_gateway_http_request_duration_seconds_bucket{route="download",le="+Inf"} 3`,
		`file_gateway_http_request_duration_seconds_count{route="download"} 3`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	// 3xx 不新增状态桶，折叠进 other；2xx 仍按 2xx 计（3 次总请求 = 2 次 2xx + 1 次 other）。
	if !strings.Contains(body, `file_gateway_http_requests_total{route="download",status="2xx"} 2`) {
		t.Fatalf("2xx bucket missing:\n%s", body)
	}
	if !strings.Contains(body, `file_gateway_http_requests_total{route="download",status="other"} 1`) {
		t.Fatalf("3xx must fold into other:\n%s", body)
	}
}

// 状态码是非标准值时不得新建序列：1xx/3xx/畸形码一律 other。
func TestFileGatewayRequestMetricsFoldNonStandardStatuses(t *testing.T) {
	metrics := newFileGatewayRequestMetrics()
	metrics.record(fileGatewayRouteOther, http.StatusContinue, time.Millisecond)
	metrics.record(fileGatewayRouteOther, http.StatusFound, time.Millisecond)
	metrics.record(fileGatewayRouteOther, 42, time.Millisecond)
	body := scrapeFileGatewayMetrics(t, metrics)
	if !strings.Contains(body, `file_gateway_http_requests_total{route="other",status="other"} 3`) {
		t.Fatalf("non-standard statuses must fold into other:\n%s", body)
	}
}

// -race：请求中间件与抓取同时执行时不得数据竞争或丢计数。
func TestFileGatewayRequestMetricsConcurrentRecordAndScrape(t *testing.T) {
	metrics := newFileGatewayRequestMetrics()
	handler := metrics.wrap(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		metrics.record(fileGatewayRouteUpload, http.StatusOK, time.Millisecond)
	}))
	const workers = 8
	const perWorker = 100
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for attempt := 0; attempt < perWorker; attempt++ {
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/any", nil))
			}
		}()
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for attempt := 0; attempt < perWorker; attempt++ {
				_ = scrapeFileGatewayMetrics(t, metrics)
			}
		}()
	}
	waitGroup.Wait()
	body := scrapeFileGatewayMetrics(t, metrics)
	if !strings.Contains(body, `file_gateway_http_requests_total{route="other",status="2xx"} 800`) {
		t.Fatalf("outer fallback count mismatch:\n%s", body)
	}
	if !strings.Contains(body, `file_gateway_http_requests_total{route="upload",status="2xx"} 800`) {
		t.Fatalf("tracked route count mismatch:\n%s", body)
	}
}

// /metrics 抓取自身不计量：否则抓取频率会直接改写被观测值，形成自计量正反馈。
func TestFileGatewayRequestMetricsSkipSelfScrape(t *testing.T) {
	metrics := newFileGatewayRequestMetrics()
	mux := new(http.ServeMux)
	mux.HandleFunc("GET /metrics", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
	metrics.wrap(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := scrapeFileGatewayMetrics(t, metrics)
	for _, route := range []string{"upload", "download", "ticket", "other"} {
		if strings.Contains(body, `file_gateway_http_requests_total{route="`+route+`",status="2xx"} 1`) {
			t.Fatalf("/metrics scrape must not count itself:\n%s", body)
		}
	}
}

// routes() 必须把请求指标接到真实 mux 上，并可通过 /metrics 暴露。
func TestFileGatewayRoutesExposeRequestMetrics(t *testing.T) {
	handler := routes(&filehttp.Handler{}, &filehttp.UploadV2Handler{}, nil, tokenMiddleware{}, nil, nil, nil)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/livez", nil))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	if !strings.Contains(body, `file_gateway_http_requests_total{route="other",status="2xx"} 1`) {
		t.Fatalf("routes() must wrap the mux with request metrics:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE file_gateway_http_request_duration_seconds histogram") {
		t.Fatalf("routes() /metrics must expose the request duration histogram:\n%s", body)
	}
}

func scrapeFileGatewayMetrics(t *testing.T, metrics *fileGatewayRequestMetrics) string {
	t.Helper()
	var output strings.Builder
	metrics.write(&output)
	return output.String()
}
