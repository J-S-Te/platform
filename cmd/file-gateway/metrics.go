package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
)

// file-gateway 请求级指标。与平台 /metrics 同为手写 exposition（不引入 prometheus
// client），并由同一组共享标签转义/桶边界函数渲染，避免两套文本协议规则漂移。
const (
	metricFileGatewayHTTPRequestsTotal   = "file_gateway_http_requests_total"
	metricFileGatewayHTTPDurationSeconds = "file_gateway_http_request_duration_seconds"
)

// 路由维度只允许固定模板枚举：上传 / 下载 / 票据 / 其他。
//
// 禁止使用原始请求路径：file-gateway 的路径里带 file_id、upload_id、票据与查询串，
// 一旦落进标签，单次扫描就能把序列数打爆，并把票据/文件名复制进指标。标签由注册点
// 显式给出（见 routes() 的 track 调用），未标注的路由统一记 other。
const (
	fileGatewayRouteUpload   = "upload"
	fileGatewayRouteDownload = "download"
	fileGatewayRouteTicket   = "ticket"
	fileGatewayRouteOther    = "other"
)

// fileGatewayRoutes 是 route 的固定输出顺序。
var fileGatewayRoutes = [...]string{
	fileGatewayRouteUpload,
	fileGatewayRouteDownload,
	fileGatewayRouteTicket,
	fileGatewayRouteOther,
}

// 状态码只分 2xx/4xx/5xx 三个桶；1xx/3xx/畸形码归 other（网关自身不产生重定向，
// 出现 3xx 本身就是异常信号，但不能让它新建序列）。
const (
	fileGatewayStatus2xx   = "2xx"
	fileGatewayStatus4xx   = "4xx"
	fileGatewayStatus5xx   = "5xx"
	fileGatewayStatusOther = "other"
)

var fileGatewayStatuses = [...]string{
	fileGatewayStatus2xx,
	fileGatewayStatus4xx,
	fileGatewayStatus5xx,
	fileGatewayStatusOther,
}

const fileGatewayStatusOtherIndex = 3

// fileGatewayDurationBuckets 覆盖 50ms 到 60s：低于 50ms 的请求全部落第一个桶，
// 高于 60s 的只计 +Inf（大文件传输跑到超时的那一类，本身就是要看的信号）。
var fileGatewayDurationBuckets = [...]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// fileGatewayRouteMetrics 是单个 route 的累计值。
//
// buckets[i] 直接存“耗时 <= fileGatewayDurationBuckets[i]”的累计次数，因此桶天然单调；
// +Inf 不单独存储，输出时直接取 count——它按定义就是所有耗时的累计值，
// 这样 `+Inf == _count` 是结构恒等式，不依赖两处计数保持一致。
type fileGatewayRouteMetrics struct {
	statuses      [len(fileGatewayStatuses)]atomic.Uint64
	count         atomic.Uint64
	durationNanos atomic.Uint64
	buckets       [len(fileGatewayDurationBuckets)]atomic.Uint64
}

// fileGatewayRequestMetrics 是 file-gateway 进程内的请求面指标集合。
type fileGatewayRequestMetrics struct {
	routes [len(fileGatewayRoutes)]fileGatewayRouteMetrics
}

// newFileGatewayRequestMetrics 创建请求面指标；零值不可用（数组大小为常量，可直接取地址）。
func newFileGatewayRequestMetrics() *fileGatewayRequestMetrics {
	return &fileGatewayRequestMetrics{}
}

// record 记录一次请求：route 走枚举下标，status 只分桶，耗时累加进直方图。
// 传入非法 route/status 时分别折叠到 other / other，调用方无法制造新序列。
func (metrics *fileGatewayRequestMetrics) record(route string, statusCode int, elapsed time.Duration) {
	if metrics == nil {
		return
	}
	series := &metrics.routes[fileGatewayRouteIndex(route)]
	series.statuses[fileGatewayStatusIndex(statusCode)].Add(1)
	series.count.Add(1)
	nanos := elapsed.Nanoseconds()
	if nanos < 0 {
		nanos = 0
	}
	series.durationNanos.Add(uint64(nanos))
	seconds := float64(nanos) / float64(time.Second)
	for index := range fileGatewayDurationBuckets {
		if seconds <= fileGatewayDurationBuckets[index] {
			series.buckets[index].Add(1)
		}
	}
}

// track 把业务 handler 包成“按固定 route 枚举计量”的处理器。
//
// route 由路由注册点显式给出（不从 URL 推断），因此标签取值域与路由表同源且可枚举。
// 同时把本次请求标记为“已按模板计量”，外层 wrap 的 other 兜底不会再重复计数。
func (metrics *fileGatewayRequestMetrics) track(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		startedAt := time.Now()
		recorder := &fileGatewayStatusRecorder{ResponseWriter: writer}
		next.ServeHTTP(recorder, request)
		metrics.record(route, recorder.statusCode(), time.Since(startedAt))
		if observation, ok := request.Context().Value(fileGatewayObservationKey{}).(*fileGatewayObservation); ok {
			observation.recorded = true
		}
	})
}

// fileGatewayObservationKey / fileGatewayObservation 是“本次请求是否已按模板计量”的
// 内部信号，只存在于请求上下文，不进入响应或日志。
type fileGatewayObservationKey struct{}

type fileGatewayObservation struct{ recorded bool }

// wrap 是 mux 的最外层：所有未被 track 标注的路由（健康检查、绑定、清理、404 等）
// 统一记 route="other"。它还负责让 /metrics 抓取自身不计量——否则抓取频率会直接
// 改写被观测值，形成自计量正反馈。
func (metrics *fileGatewayRequestMetrics) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request != nil && request.URL.Path == "/metrics" {
			next.ServeHTTP(writer, request)
			return
		}
		observation := &fileGatewayObservation{}
		request = request.WithContext(context.WithValue(request.Context(), fileGatewayObservationKey{}, observation))
		startedAt := time.Now()
		recorder := &fileGatewayStatusRecorder{ResponseWriter: writer}
		next.ServeHTTP(recorder, request)
		if !observation.recorded {
			metrics.record(fileGatewayRouteOther, recorder.statusCode(), time.Since(startedAt))
		}
	})
}

// write 渲染请求面指标：请求总量按 {route,status} 展开，耗时直方图按 {route} 展开。
// 全部固定组合（含 0）无条件输出：维度基数极小，恒有样本才能让 rate()/absent() 在
// 空闲进程上也有定义，而不是把“零请求”伪装成“抓取不到”。
func (metrics *fileGatewayRequestMetrics) write(writer io.Writer) {
	metrics = fileGatewayMetricsOr(metrics)
	fmt.Fprintf(writer, "# TYPE %s counter\n", metricFileGatewayHTTPRequestsTotal)
	for routeIndex, route := range fileGatewayRoutes {
		for statusIndex, status := range fileGatewayStatuses {
			fmt.Fprintf(writer, "%s{route=\"%s\",status=\"%s\"} %d\n",
				metricFileGatewayHTTPRequestsTotal,
				eventmetrics.EscapeLabelValue(route), eventmetrics.EscapeLabelValue(status),
				metrics.routes[routeIndex].statuses[statusIndex].Load())
		}
	}
	fmt.Fprintf(writer, "# TYPE %s histogram\n", metricFileGatewayHTTPDurationSeconds)
	for routeIndex, route := range fileGatewayRoutes {
		series := &metrics.routes[routeIndex]
		labels := `{route="` + eventmetrics.EscapeLabelValue(route) + `"`
		for bucketIndex := range fileGatewayDurationBuckets {
			fmt.Fprintf(writer, "%s_bucket%s,le=\"%s\"} %d\n", metricFileGatewayHTTPDurationSeconds, labels,
				eventmetrics.FormatBucketBound(fileGatewayDurationBuckets[bucketIndex]),
				series.buckets[bucketIndex].Load())
		}
		count := series.count.Load()
		fmt.Fprintf(writer, "%s_bucket%s,le=\"+Inf\"} %d\n", metricFileGatewayHTTPDurationSeconds, labels, count)
		fmt.Fprintf(writer, "%s_sum%s} %.9f\n", metricFileGatewayHTTPDurationSeconds, labels,
			float64(series.durationNanos.Load())/float64(time.Second))
		fmt.Fprintf(writer, "%s_count%s} %d\n", metricFileGatewayHTTPDurationSeconds, labels, count)
	}
}

// fileGatewayMetricsOr 让未初始化的指标（nil）退化为只输出零样本的渲染器，
// 保证装配遗漏时 /metrics 仍然合法，不会把抓取打挂。
func fileGatewayMetricsOr(metrics *fileGatewayRequestMetrics) *fileGatewayRequestMetrics {
	if metrics == nil {
		return &fileGatewayRequestMetrics{}
	}
	return metrics
}

func fileGatewayRouteIndex(route string) int {
	for index, candidate := range fileGatewayRoutes {
		if candidate == route {
			return index
		}
	}
	return len(fileGatewayRoutes) - 1
}

func fileGatewayStatusIndex(statusCode int) int {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return 0
	case statusCode >= 400 && statusCode < 500:
		return 1
	case statusCode >= 500 && statusCode < 600:
		return 2
	default:
		return fileGatewayStatusOtherIndex
	}
}

// fileGatewayStatusRecorder 只观察业务 handler 写出的状态码；Header/Body 原样透传，
// 不改变响应语义（下载流式写入仍直接落到真实连接）。
type fileGatewayStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *fileGatewayStatusRecorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *fileGatewayStatusRecorder) Write(value []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.ResponseWriter.Write(value)
}

func (recorder *fileGatewayStatusRecorder) statusCode() int {
	if recorder.status == 0 {
		return http.StatusOK
	}
	return recorder.status
}
