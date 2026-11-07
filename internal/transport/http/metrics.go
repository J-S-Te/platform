package httptransport

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OperationalMetrics 提供不依赖外部 SDK 的最低限度 Prometheus 指标。
// 只输出聚合运行数据，不包含租户、用户、业务单号或 URL 参数，避免高基数和隐私泄漏。
type OperationalMetrics struct {
	database *gorm.DB

	// events 是事件级失败计数器（登录失败 reason、审计写入失败 stage，见
	// internal/shared/metrics）。它与业务失败发生点共用进程默认注册表：处理器在失败
	// 现场直接记账，/metrics 只负责把同一份计数渲染出来，不做二次推导。
	events *eventmetrics.Registry

	// HTTP 请求面按 {method,route,status} 拆序列累计（见 httpSeriesKey）：
	// 只有带标签才能表达 5xx 率与按路由的尾延迟，而标签维度又必须受约束，
	// 否则任意扫描路径会把序列数打爆。
	httpMu     sync.RWMutex
	httpSeries map[httpSeriesKey]*httpSeries

	authzFailures    atomic.Uint64
	oidcFailures     atomic.Uint64
	collectionErrors atomic.Uint64

	// 队列/可靠性计数的短 TTL 抓取缓存（安全审查 SEC-B4）：未认证抓取已被
	// RequireMetricsAccess 拒绝，但已认证抓取同样不能每次 scrape 都放大为
	// 全量 COUNT——10 秒内复用上次快照，过期在下一次抓取时刷新一次。
	queueMu      sync.Mutex
	queueTTL     time.Duration
	queueExpires time.Time
	queueCache   *queueMetricsSnapshot
	// gatherQueues 可注入（测试）；nil 时使用数据库实现。
	gatherQueues func(context.Context) queueMetricsSnapshot
	// gatherBackchannel 可注入（测试）；nil 时使用 backchannel.Gauges 查询数据库。
	// 采集失败只跳过对应 gauge，绝不以 0 冒充"没有缺口"（BIZ-XEVT2-07）。
	gatherBackchannel func(context.Context) ([]backchannel.Gauge, error)
	// gatherSecrets 可注入（测试）；nil 时按 queueMetricSpecs 模式查询
	// platform_oauth_client_credential（BIZ-RUNTIME-05 密钥龄期）。
	// 采集失败同样只跳过 gauge 并计数，不输出伪 0：把"查不到"伪装成"龄期 0 天"会
	// 让 90 天密钥龄期告警永久静默。
	gatherSecrets func(context.Context) ([]queueCountValue, error)
}

// defaultQueueMetricsTTL 是队列统计默认抓取缓存时长（SEC-B4）。
const defaultQueueMetricsTTL = 10 * time.Second

// metricsPath 是 /metrics 抓取端点路径，注册与自我计量豁免共用同一常量。
const metricsPath = "/metrics"

// HTTP 请求面指标名。名字保持原样（改名会打断既有规则与看板），本轮只补标签维度并把
// 延迟从 summary 改成 histogram。三条指标共用同一组标签 {method,route,status}，
// 因此可以互相比值（例如 5xx 错误率），也可以按 route 聚合后算分位。
const (
	metricHTTPRequestsTotal   = "platform_http_requests_total"
	metricHTTPErrorsTotal     = "platform_http_errors_total"
	metricHTTPDurationSeconds = "platform_http_request_duration_seconds"
)

// maxHTTPSeries 是 HTTP 指标序列数的硬上限（基数保护）。
//
// 三个标签维度都可能被未匹配请求放大：若 route 退回 Request.URL.Path，任意扫描路径
// 都会新建一条序列，/metrics 正文大小与 Prometheus 内存会被单个来源线性放大，同时
// 把 URL 里的查询串（token、手机号等）复制进指标。因此：
//   - route 只取 gin 注册模式（FullPath），空值固定 "__unmatched__"；
//   - method 走白名单，未知方法记 "OTHER"；
//   - status 只分 1xx/2xx/3xx/4xx/5xx 五个桶（另有 "other" 兜底）。
//
// 达到上限后新组合折叠进 httpOverflowSeriesKey 继续计数（不丢请求量），所以本进程
// 输出的 HTTP 序列数上界是 maxHTTPSeries + 1 个溢出序列。
const maxHTTPSeries = 1000

// 标签取值域的固定枚举：全部是代码内常量，绝不来自请求内容。
const (
	httpMethodOther    = "OTHER"
	httpRouteUnmatched = "__unmatched__"
	httpRouteOverflow  = "__overflow__"
	httpStatusOther    = "other"
)

// httpDurationBucketCount 是耗时直方图的桶个数（不含 +Inf）。
const httpDurationBucketCount = 11

// httpDurationBuckets 是耗时直方图的桶上界（秒）。使用 Prometheus 官方推荐的
// HTTP 延迟桶，覆盖 5ms 到 10s：低于 5ms 的请求全部落在第一个桶，高于 10s 的只计入
// +Inf 桶（跑满超时的那一类，本身就是要看的信号）。
var httpDurationBuckets = [httpDurationBucketCount]float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// httpSeriesKey 是 HTTP 指标的唯一序列键。三个维度的取值域都被上面的约束限死，
// 因此键空间是可枚举且有上界的。
type httpSeriesKey struct {
	method string
	route  string
	status string
}

// httpOverflowSeriesKey 是序列数超限后的折叠目标：用固定的枚举值吸收所有新组合。
var httpOverflowSeriesKey = httpSeriesKey{method: httpMethodOther, route: httpRouteOverflow, status: httpStatusOther}

// labels 渲染 Prometheus 标签串；顺序固定 method→route→status，保证输出确定性。
func (key httpSeriesKey) labels() string {
	return key.labelsWith("")
}

// labelsWith 追加一个额外标签（直方图桶用 `le="..."`）。
func (key httpSeriesKey) labelsWith(extra string) string {
	rendered := `{method="` + escapeMetricLabelValue(key.method) +
		`",route="` + escapeMetricLabelValue(key.route) +
		`",status="` + escapeMetricLabelValue(key.status) + `"`
	if extra != "" {
		rendered += "," + extra
	}
	return rendered + "}"
}

// httpSeries 是一条序列的累计值。
//
// buckets[i] 直接存 "耗时 <= httpDurationBuckets[i]" 的**累计**次数，因此桶天然单调；
// +Inf 桶不单独存储，输出时直接取 requests——它按定义就是“所有耗时”的累计值，
// 这样 `+Inf == _count` 是结构上的恒等式，不依赖两处计数保持一致。
type httpSeries struct {
	requests      atomic.Uint64
	errors        atomic.Uint64
	durationNanos atomic.Uint64
	buckets       [httpDurationBucketCount]atomic.Uint64
}

// httpMethodLabel 把请求方法映射到白名单值，未知方法一律 OTHER。
func httpMethodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodHead:
		return method
	default:
		return httpMethodOther
	}
}

// httpRouteLabel 只接受 gin 注册模式；空串（未匹配路由/405）固定 "__unmatched__"。
// 这里绝不退回 Request.URL.Path：那是高基数与 PII 泄漏的入口。
func httpRouteLabel(fullPath string) string {
	if fullPath == "" {
		return httpRouteUnmatched
	}
	return fullPath
}

// httpStatusLabel 把状态码归入 1xx/2xx/3xx/4xx/5xx 五个桶；非标准码（<100 或 >599）
// 记 "other"，避免把畸形状态码变成新序列。
func httpStatusLabel(statusCode int) string {
	switch {
	case statusCode >= 100 && statusCode < 200:
		return "1xx"
	case statusCode >= 200 && statusCode < 300:
		return "2xx"
	case statusCode >= 300 && statusCode < 400:
		return "3xx"
	case statusCode >= 400 && statusCode < 500:
		return "4xx"
	case statusCode >= 500 && statusCode < 600:
		return "5xx"
	default:
		return httpStatusOther
	}
}

// httpSeriesFor 返回 key 对应的序列（必要时创建）。超过 maxHTTPSeries 个常规序列后，
// 新组合折叠到 httpOverflowSeriesKey 共享一条序列，保证基数有上界。
func (metrics *OperationalMetrics) httpSeriesFor(key httpSeriesKey) *httpSeries {
	metrics.httpMu.RLock()
	series, exists := metrics.httpSeries[key]
	metrics.httpMu.RUnlock()
	if exists {
		return series
	}
	metrics.httpMu.Lock()
	defer metrics.httpMu.Unlock()
	if series, exists = metrics.httpSeries[key]; exists {
		return series
	}
	if metrics.httpSeries == nil {
		metrics.httpSeries = make(map[httpSeriesKey]*httpSeries, 16)
	}
	if len(metrics.httpSeries) >= maxHTTPSeries && key != httpOverflowSeriesKey {
		key = httpOverflowSeriesKey
		if series, exists = metrics.httpSeries[key]; exists {
			return series
		}
	}
	series = &httpSeries{}
	metrics.httpSeries[key] = series
	return series
}

// recordHTTPRequest 把一次请求记到 {method,route,status} 序列。method 走白名单、
// route 只收注册模式、status 只分桶——调用方传什么都不会产生新的取值域。
func (metrics *OperationalMetrics) recordHTTPRequest(method, route string, statusCode int, elapsed time.Duration) {
	series := metrics.httpSeriesFor(httpSeriesKey{
		method: httpMethodLabel(method),
		route:  httpRouteLabel(route),
		status: httpStatusLabel(statusCode),
	})
	series.requests.Add(1)
	if statusCode >= http.StatusBadRequest {
		series.errors.Add(1)
	}
	nanos := elapsed.Nanoseconds()
	if nanos < 0 {
		nanos = 0
	}
	series.durationNanos.Add(uint64(nanos))
	seconds := float64(nanos) / float64(time.Second)
	for index := range httpDurationBuckets {
		if seconds <= httpDurationBuckets[index] {
			series.buckets[index].Add(1)
		}
	}
}

// httpSeriesSnapshot 是抓取期的一条序列（只持有指针，不复制 atomic 字段）。
type httpSeriesSnapshot struct {
	key    httpSeriesKey
	series *httpSeries
}

// sortedHTTPSeries 在锁内复制键集合，解锁后按键排序（method→route→status），
// 保证同一状态多次抓取的输出顺序稳定，抓取期间也不阻塞新序列创建。
func (metrics *OperationalMetrics) sortedHTTPSeries() []httpSeriesSnapshot {
	metrics.httpMu.RLock()
	snapshot := make([]httpSeriesSnapshot, 0, len(metrics.httpSeries))
	for key, series := range metrics.httpSeries {
		snapshot = append(snapshot, httpSeriesSnapshot{key: key, series: series})
	}
	metrics.httpMu.RUnlock()
	sort.Slice(snapshot, func(left, right int) bool {
		if snapshot[left].key.method != snapshot[right].key.method {
			return snapshot[left].key.method < snapshot[right].key.method
		}
		if snapshot[left].key.route != snapshot[right].key.route {
			return snapshot[left].key.route < snapshot[right].key.route
		}
		return snapshot[left].key.status < snapshot[right].key.status
	})
	return snapshot
}

// writeHTTPMetrics 输出 HTTP 请求面三条指标。TYPE 行无条件声明一次（多序列只重复样本行）；
// 没有任何序列时只输出 TYPE，不伪造全零样本——"还没有请求"和"抓取被拒绝"必须能区分，
// 所以样本缺失的兜底规则改用无条件输出的 collection_errors（见 platform-alerts.yml）。
func (metrics *OperationalMetrics) writeHTTPMetrics(writer http.ResponseWriter) {
	fmt.Fprintf(writer, "# TYPE %s counter\n", metricHTTPRequestsTotal)
	fmt.Fprintf(writer, "# TYPE %s counter\n", metricHTTPErrorsTotal)
	fmt.Fprintf(writer, "# TYPE %s histogram\n", metricHTTPDurationSeconds)
	for _, entry := range metrics.sortedHTTPSeries() {
		labels := entry.key.labels()
		requests := entry.series.requests.Load()
		errors := entry.series.errors.Load()
		fmt.Fprintf(writer, "%s%s %d\n", metricHTTPRequestsTotal, labels, requests)
		fmt.Fprintf(writer, "%s%s %d\n", metricHTTPErrorsTotal, labels, errors)
		for index := range httpDurationBuckets {
			fmt.Fprintf(writer, "%s_bucket%s %d\n", metricHTTPDurationSeconds,
				entry.key.labelsWith(`le="`+formatHTTPBucketBound(httpDurationBuckets[index])+`"`),
				entry.series.buckets[index].Load())
		}
		// +Inf 桶按定义等于 _count，直接取请求数，避免两处计数不一致。
		fmt.Fprintf(writer, "%s_bucket%s %d\n", metricHTTPDurationSeconds,
			entry.key.labelsWith(`le="+Inf"`), requests)
		// _sum 沿用原有精度口径：纳秒整数累加后按秒输出 %.9f。
		fmt.Fprintf(writer, "%s_sum%s %.9f\n", metricHTTPDurationSeconds, labels,
			float64(entry.series.durationNanos.Load())/float64(time.Second))
		fmt.Fprintf(writer, "%s_count%s %d\n", metricHTTPDurationSeconds, labels, requests)
	}
}

// formatHTTPBucketBound 用最短往返表示输出桶上界（0.005、0.025、1、2.5、10），
// 避免 %f 的尾零或科学计数法让 le 值与阈值比较时失真。
func formatHTTPBucketBound(bound float64) string {
	return eventmetrics.FormatBucketBound(bound)
}

// OAuth Client Secret 龄期/吊销指标（BIZ-RUNTIME-05）。它们与队列计数共用同一 10s TTL
// 快照，避免每次抓取都做全表聚合。
const (
	// metricClientSecretActive 按 status 维度输出 ACTIVE/REVOKED 等状态的 Secret 数量。
	// 它只是全局分布：判定"某个客户端在 KEEP 下累积了多个 ACTIVE Secret"必须看下面带
	// client_id 的维度，不能用全局总数反推单客户端状态。
	metricClientSecretActive = "platform_oauth_client_secret_active"
	// metricClientSecretActiveClient 是按 client_id 维度的 ACTIVE Secret 数量：
	// 单客户端 > 1 即 KEEP 策略下该客户端的旧 Secret 仍在服役（RUNTIME-05 告警输入）。
	//
	// 基数控制：本指标按客户端展开序列，输出条数上限为 clientSecretClientSeriesLimit，
	// 按 ACTIVE 数量降序、client_id 升序取前 N 个客户端，使"有累积的客户端"优先进入样本。
	// 是否被截断由 metricClientSecretActiveClients（去重客户端总数）暴露，不能靠
	// "没看到样本"推断没有累积。
	metricClientSecretActiveClient = "platform_oauth_client_secret_active_client"
	// metricClientSecretActiveClients 是有 ACTIVE Secret 的去重客户端总数，
	// 用于核对按 client_id 展开的序列是否被上限截断。
	metricClientSecretActiveClients = "platform_oauth_client_secret_active_clients"
	// metricClientSecretOldestActiveSeconds 是最老 ACTIVE Secret 的龄期（秒），
	// 90 天（7776000s）为制度上限。
	metricClientSecretOldestActiveSeconds = "platform_oauth_client_secret_oldest_active_seconds"
	// metricClientSecretActiveOver90Days 是龄期超过 90 天的 ACTIVE Secret 总数；
	// 与"最老龄期"互补：最老龄期可能只被单个客户端拉高，这条计数说明遗留面有多大。
	metricClientSecretActiveOver90Days = "platform_oauth_client_secret_active_over_90d"
	// metricClientSecretRevokedTotal 是已吊销 Secret 总数，用于观察灰度收窗进度。
	metricClientSecretRevokedTotal = "platform_oauth_client_secret_revoked_total"
	// oauthClientSecretCredentialType 与仓储写入的 credential_type 保持一致。
	oauthClientSecretCredentialType = "secret"
	// credentialStatusActive 是唯一"仍可用于换令牌"的凭据状态。
	credentialStatusActive = "ACTIVE"
	// clientSecretClientSeriesLimit 是按 client_id 展开序列的硬上限（基数保护）。
	clientSecretClientSeriesLimit = 200
	// clientSecretMaxAgeSeconds 是密钥龄期制度上限：90 天。
	clientSecretMaxAgeSeconds = int64(90 * 24 * 60 * 60)
)

type queueCountValue struct {
	name   string
	count  int64
	failed bool
	// labels 是已渲染的 Prometheus 标签串（如 `{status="ACTIVE"}`），空表示无标签。
	// 标签值只能用受约束的枚举，或经基数上限保护且已转义的客户端标识（
	// platform_oauth_client_secret_active_client 的 client_id），不含租户/用户/业务单号。
	labels string
	// metricType 为空时按 gauge 输出；`_total` 累计值显式声明为 counter。
	metricType string
}

// queueMetricsSnapshot 是一次队列/可靠性采集的完整结果，缓存与输出解耦。
type queueMetricsSnapshot struct {
	counts        []queueCountValue
	retries       uint64
	retriesFailed bool
	lastSuccess   float64
	// backchannel 是 Back-Channel Logout 的缺登记/不可投递 gauge 采样（BIZ-XEVT2-07）。
	// 它与队列计数共用同一 10s TTL 快照与单飞刷新；采集失败的维度标记 failed 并被跳过，
	// 因此抓取失败时这些 gauge 直接不出现，而不是输出一个假的 0。
	backchannel []queueCountValue
	// secrets 是 OAuth Client Secret 的 status 分布/单客户端分布/龄期/吊销总数采样
	// （BIZ-RUNTIME-05）。语义同 backchannel：与队列计数同一次刷新，采集失败整体跳过并计数。
	secrets []queueCountValue
}

type queueMetricSpec struct {
	name      string
	table     string
	predicate string
	values    []string
}

// queueMetricSpecs 集中原有7个队列计数查询，采集阶段统一执行、输出阶段只读快照。
var queueMetricSpecs = []queueMetricSpec{
	{name: "platform_keycloak_outbox_backlog", table: "keycloak_authorization_outbox", predicate: "status IN ?", values: []string{"PENDING", "PROCESSING", "RETRY"}},
	{name: "platform_keycloak_outbox_dead", table: "keycloak_authorization_outbox", predicate: "status IN ?", values: []string{"FAILED", "DEAD"}},
	{name: "platform_async_job_backlog", table: "async_job", predicate: "status IN ?", values: []string{"PENDING", "RUNNING"}},
	{name: "platform_async_job_dead", table: "async_job", predicate: "status IN ?", values: []string{"FAILED", "DEAD"}},
	{name: "platform_oidc_backchannel_logout_backlog", table: "platform_oauth_backchannel_logout_outbox", predicate: "status IN ?", values: []string{"PENDING", "PROCESSING", "RETRY"}},
	{name: "platform_oidc_backchannel_logout_failed", table: "platform_oauth_backchannel_logout_outbox", predicate: "status IN ?", values: []string{"FAILED"}},
	{name: "platform_oidc_backchannel_logout_delivered", table: "platform_oauth_backchannel_logout_outbox", predicate: "status IN ?", values: []string{"DELIVERED"}},
}

// NewOperationalMetrics 创建进程内 HTTP 指标收集器；数据库参数用于按需采集连接池和队列积压。
func NewOperationalMetrics(database *gorm.DB) *OperationalMetrics {
	return &OperationalMetrics{database: database, events: eventmetrics.Shared()}
}

// eventMetrics 返回事件级计数注册表；nil 时回退进程默认注册表（装配遗漏只丢计数）。
func (metrics *OperationalMetrics) eventMetrics() *eventmetrics.Registry {
	if metrics == nil {
		return eventmetrics.Shared()
	}
	return eventmetrics.RegistryOr(metrics.events)
}

// Middleware 记录按 {method,route,status} 拆分的请求总量、错误量、耗时直方图，
// 以及认证授权失败。
//
// route 取 ginContext.FullPath()（注册模式），不取 Request.URL.Path：注册模式的
// 取值域由路由表决定，而真实路径可被任意扫描串撑成无限序列并把查询串写进指标。
// 未匹配路由（404）与 405 的 FullPath 为空，统一记 "__unmatched__"。
//
// /metrics 抓取自身有意不计量：这个端点的响应正文就是这些计数器，若把抓取本身算进
// requests_total / duration，抓取频率（以及抓取端重试）会直接改写被观测值，形成自我
// 计量的正反馈；同时 RequireMetricsAccess 在未授权时返回的 401/403 也会被当成业务
// 侧认证授权失败计入 authz_failures，让“令牌配错”伪装成“用户登录被拒”触发告警。
// 因此 /metrics 只在链路上透传，不做任何计数；其它路径语义保持不变。
func (metrics *OperationalMetrics) Middleware() gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		if ginContext.Request != nil && ginContext.Request.URL.Path == metricsPath {
			ginContext.Next()
			return
		}
		startedAt := time.Now()
		ginContext.Next()
		status := ginContext.Writer.Status()
		method := ""
		if ginContext.Request != nil {
			method = ginContext.Request.Method
		}
		metrics.recordHTTPRequest(method, ginContext.FullPath(), status, time.Since(startedAt))
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			metrics.authzFailures.Add(1)
			path := ginContext.Request.URL.Path
			if strings.HasPrefix(path, "/oauth2/") || strings.HasPrefix(path, "/authorize") || strings.HasPrefix(path, "/api/v1/auth/") {
				metrics.oidcFailures.Add(1)
			}
		}
	}
}

// ServeHTTP 输出 Prometheus 文本协议。队列统计采用短超时并独立降级，监控采集失败不会影响业务请求。
func (metrics *OperationalMetrics) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	metrics.writeHTTPMetrics(writer)
	fmt.Fprintf(writer, "# TYPE platform_authz_failures_total counter\nplatform_authz_failures_total %d\n", metrics.authzFailures.Load())
	fmt.Fprintf(writer, "# TYPE platform_oidc_login_failures_total counter\nplatform_oidc_login_failures_total %d\n", metrics.oidcFailures.Load())
	// 事件级失败计数（C2）：platform_oidc_login_failures_total 是“401/403 + 路径前缀”的
	// 启发式，只说明某类路径上出现过拒绝；这两条指标在失败发生点记账，能区分原因与阶段。
	metrics.eventMetrics().Write(writer)
	metrics.writeDatabaseMetrics(writer, request.Context())
	fmt.Fprintf(writer, "# TYPE platform_metric_collection_errors_total counter\nplatform_metric_collection_errors_total %d\n", metrics.collectionErrors.Load())
}

func (metrics *OperationalMetrics) writeDatabaseMetrics(writer http.ResponseWriter, parent context.Context) {
	if metrics.database == nil && metrics.gatherQueues == nil && metrics.gatherBackchannel == nil && metrics.gatherSecrets == nil {
		metrics.collectionErrors.Add(1)
		return
	}
	if metrics.database != nil {
		sqlDatabase, err := metrics.database.DB()
		if err != nil {
			metrics.collectionErrors.Add(1)
			return
		}
		// 连接池统计取自进程内句柄，开销可忽略，保持每次抓取实时。
		stats := sqlDatabase.Stats()
		fmt.Fprintf(writer, "# TYPE platform_database_open_connections gauge\nplatform_database_open_connections %d\n", stats.OpenConnections)
		fmt.Fprintf(writer, "# TYPE platform_database_in_use_connections gauge\nplatform_database_in_use_connections %d\n", stats.InUse)
		fmt.Fprintf(writer, "# TYPE platform_database_wait_count_total counter\nplatform_database_wait_count_total %d\n", stats.WaitCount)
	}

	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	// 队列/可靠性计数走10s TTL 快照（SEC-B4）：TTL 内的重复抓取零 DB 查询。
	snapshot := metrics.queueSnapshot(ctx)
	writeMetricSeries(writer, snapshot.counts)
	// Back-Channel Logout 缺口 gauge：只在采集成功时输出（采集失败已在采集阶段跳过并计数），
	// 与队列计数共用同一快照，避免抓取时额外放大为全量查询。
	writeMetricSeries(writer, snapshot.backchannel)
	// OAuth Client Secret 龄期/吊销采样：同样的"失败即缺席"语义。
	writeMetricSeries(writer, snapshot.secrets)
	if !snapshot.retriesFailed {
		fmt.Fprintf(writer, "# TYPE platform_async_job_retries_total counter\nplatform_async_job_retries_total %d\n", snapshot.retries)
		fmt.Fprintf(writer, "# TYPE platform_async_job_last_success_timestamp_seconds gauge\nplatform_async_job_last_success_timestamp_seconds %.3f\n", snapshot.lastSuccess)
	}
}

// writeMetricSeries 输出一组样本，并按指标名只打印一次 TYPE 行：带 status 标签的 gauge
// 会产出多条样本，重复的 TYPE 行会让 Prometheus 文本解析器直接拒绝整个抓取结果。
func writeMetricSeries(writer http.ResponseWriter, values []queueCountValue) {
	declared := make(map[string]bool, len(values))
	for _, value := range values {
		if value.failed {
			continue
		}
		if !declared[value.name] {
			metricType := value.metricType
			if metricType == "" {
				metricType = "gauge"
			}
			fmt.Fprintf(writer, "# TYPE %s %s\n", value.name, metricType)
			declared[value.name] = true
		}
		fmt.Fprintf(writer, "%s%s %d\n", value.name, value.labels, value.count)
	}
}

// queueSnapshot 返回 TTL 内缓存的队列快照；过期后在锁内刷新一次，保证并发抓取
// 不会同时触发多轮 COUNT（单飞刷新）。
func (metrics *OperationalMetrics) queueSnapshot(ctx context.Context) queueMetricsSnapshot {
	metrics.queueMu.Lock()
	defer metrics.queueMu.Unlock()
	if metrics.queueCache != nil && time.Now().Before(metrics.queueExpires) {
		return *metrics.queueCache
	}
	snapshot := metrics.gatherQueueMetrics(ctx)
	metrics.queueCache = &snapshot
	ttl := metrics.queueTTL
	if ttl <= 0 {
		ttl = defaultQueueMetricsTTL
	}
	metrics.queueExpires = time.Now().Add(ttl)
	return snapshot
}

func (metrics *OperationalMetrics) gatherQueueMetrics(ctx context.Context) queueMetricsSnapshot {
	var snapshot queueMetricsSnapshot
	if metrics.gatherQueues != nil {
		snapshot = metrics.gatherQueues(ctx)
	} else if metrics.database != nil {
		for _, spec := range queueMetricSpecs {
			var count int64
			if err := metrics.database.WithContext(ctx).Table(spec.table).Where(spec.predicate, spec.values).Count(&count).Error; err != nil {
				metrics.collectionErrors.Add(1)
				snapshot.counts = append(snapshot.counts, queueCountValue{name: spec.name, failed: true})
				continue
			}
			snapshot.counts = append(snapshot.counts, queueCountValue{name: spec.name, count: count})
		}
		metrics.gatherAsyncJobReliability(ctx, &snapshot)
	}
	// Back-Channel Logout 缺口采样与队列计数在同一快照内完成：同一次 TTL 刷新、同一次
	// 单飞执行，采集失败不会阻塞其余指标。
	snapshot.backchannel = metrics.gatherBackchannelGauges(ctx)
	// OAuth Client Secret 龄期采样共用同一快照与 TTL（BIZ-RUNTIME-05）。
	snapshot.secrets = metrics.gatherSecretGauges(ctx)
	return snapshot
}

// gatherSecretGauges 采集 OAuth Client Secret 的 status 分布、单客户端 ACTIVE 数量、
// 最老 ACTIVE 龄期、超过 90 天的 ACTIVE 总数和吊销总数。采集失败不写 0，而是把这些
// 维度全部标记 failed（输出阶段跳过）并计入采集错误：把"查不到"输出成"龄期 0 秒/没有
// 累积"会让 90 天龄期与单客户端累积告警永久静默。
func (metrics *OperationalMetrics) gatherSecretGauges(ctx context.Context) []queueCountValue {
	gather := metrics.gatherSecrets
	if gather == nil {
		if metrics.database == nil {
			return nil
		}
		database := metrics.database
		gather = func(ctx context.Context) ([]queueCountValue, error) {
			return collectClientSecretMetrics(ctx, database, time.Now().UTC())
		}
	}
	values, err := gather(ctx)
	if err != nil {
		metrics.collectionErrors.Add(1)
		slog.Default().Warn("collect oauth client secret metrics", "error", err)
		return []queueCountValue{
			{name: metricClientSecretActive, failed: true},
			{name: metricClientSecretActiveClient, failed: true},
			{name: metricClientSecretActiveClients, failed: true},
			{name: metricClientSecretOldestActiveSeconds, failed: true},
			{name: metricClientSecretActiveOver90Days, failed: true},
			{name: metricClientSecretRevokedTotal, failed: true},
		}
	}
	return values
}

// collectClientSecretMetrics 用三条廉价聚合查询产出 Secret 指标：
//   - 按 status 分组的 Secret 总数（全局分布）；
//   - 按 client_id 分组的 ACTIVE Secret 数量（受 clientSecretClientSeriesLimit 限流），
//     并从同一结果派生去重客户端数、最老龄期和超过 90 天的 ACTIVE 总数；
//   - 已吊销 Secret 总数（灰度收窗进度）。
//
// 没有任何 ACTIVE Secret 时"最老龄期"无定义，该 gauge 直接不输出（避免把"查不到"
// 当成"很新"）；而去重客户端数与超龄计数仍然输出真实的 0，这样样本缺席就只剩
// "采集失败/鉴权失效"一种解释，交给 absent() 告警识别。
func collectClientSecretMetrics(ctx context.Context, database *gorm.DB, now time.Time) ([]queueCountValue, error) {
	type statusCount struct {
		Status string `gorm:"column:status"`
		Count  int64  `gorm:"column:secret_count"`
	}
	var counts []statusCount
	if err := database.WithContext(ctx).Table("platform_oauth_client_credential").
		Select("status, COUNT(*) AS secret_count").
		Where("credential_type = ?", oauthClientSecretCredentialType).
		Group("status").
		Order("status").
		Scan(&counts).Error; err != nil {
		return nil, err
	}
	values := make([]queueCountValue, 0, len(counts)+3)

	// 按 client_id 分组：一次查询同时给出单客户端 ACTIVE 数量、最老龄期与超龄计数。
	// ORDER BY 与下面的截断共同保证"累积最多的客户端"优先出现在样本里，
	// 因此基数上限不会掩盖 >1 的告警条件。
	type clientCount struct {
		OAuthClientID   string     `gorm:"column:oauth_client_id"`
		SecretCount     int64      `gorm:"column:secret_count"`
		OldestCreatedAt *time.Time `gorm:"column:oldest_created_at"`
		Over90Days      int64      `gorm:"column:over_90d"`
	}
	cutoff := now.Add(-time.Duration(clientSecretMaxAgeSeconds) * time.Second)
	var clients []clientCount
	if err := database.WithContext(ctx).Table("platform_oauth_client_credential").
		Select("oauth_client_id, COUNT(*) AS secret_count, MIN(created_at) AS oldest_created_at, SUM(CASE WHEN created_at < ? THEN 1 ELSE 0 END) AS over_90d", cutoff).
		Where("credential_type = ? AND status = ? AND revoked_at IS NULL", oauthClientSecretCredentialType, credentialStatusActive).
		Group("oauth_client_id").
		Order("secret_count DESC, oauth_client_id ASC").
		Scan(&clients).Error; err != nil {
		return nil, err
	}
	if len(clients) > 0 {
		var over90 int64
		var oldest *time.Time
		emitted := len(clients)
		if emitted > clientSecretClientSeriesLimit {
			emitted = clientSecretClientSeriesLimit
		}
		for index, row := range clients {
			over90 += row.Over90Days
			if row.OldestCreatedAt != nil && (oldest == nil || row.OldestCreatedAt.Before(*oldest)) {
				oldest = row.OldestCreatedAt
			}
			if index >= emitted {
				continue
			}
			values = append(values, queueCountValue{
				name:   metricClientSecretActiveClient,
				count:  row.SecretCount,
				labels: `{client_id="` + escapeMetricLabelValue(row.OAuthClientID) + `"}`,
			})
		}
		if oldest != nil {
			age := int64(now.Sub(oldest.UTC()) / time.Second)
			if age < 0 {
				age = 0
			}
			values = append(values, queueCountValue{name: metricClientSecretOldestActiveSeconds, count: age})
		}
		values = append(values, queueCountValue{name: metricClientSecretActiveOver90Days, count: over90})
	} else {
		// 没有 ACTIVE Secret 时"超龄 0"是真实事实（不是采集失败的伪 0），因此仍然输出：
		// 样本缺席只代表采集失败或 /metrics 鉴权失效，交给 absent() 告警识别。
		values = append(values, queueCountValue{name: metricClientSecretActiveOver90Days, count: 0})
	}
	// 去重客户端总数始终输出（含 0）：它是按 client_id 展开序列是否被上限截断的判据。
	values = append(values, queueCountValue{name: metricClientSecretActiveClients, count: int64(len(clients))})

	var revoked int64
	if err := database.WithContext(ctx).Table("platform_oauth_client_credential").
		Where("credential_type = ? AND status = ?", oauthClientSecretCredentialType, "REVOKED").
		Count(&revoked).Error; err != nil {
		return nil, err
	}
	values = append(values, queueCountValue{name: metricClientSecretRevokedTotal, count: revoked, metricType: "counter"})
	return values, nil
}

// escapeMetricLabelValue 按 Prometheus 文本协议转义标签值。status 来自受约束的枚举，
// 但转义仍然必要：一个含引号或换行的值会破坏整个抓取结果。
func escapeMetricLabelValue(value string) string {
	// 转义实现集中在 internal/shared/metrics，避免平台 /metrics 与 file-gateway 两套规则漂移。
	return eventmetrics.EscapeLabelValue(value)
}

// gatherBackchannelGauges 采集 Back-Channel Logout 的两条 gauge。失败时不计 0，而是把
// 两个维度标记为 failed（输出阶段跳过）并计入采集错误，同时 WARN 留痕：把"本应通知却
// 无端点"的采集失败伪装成 0 会重新引入静默缺口。
func (metrics *OperationalMetrics) gatherBackchannelGauges(ctx context.Context) []queueCountValue {
	gather := metrics.gatherBackchannel
	if gather == nil {
		if metrics.database == nil {
			return nil
		}
		database := metrics.database
		gather = func(ctx context.Context) ([]backchannel.Gauge, error) {
			return backchannel.Gauges(ctx, database)
		}
	}
	gauges, err := gather(ctx)
	if err != nil {
		metrics.collectionErrors.Add(1)
		slog.Default().Warn("collect back-channel logout gauges", "error", err)
		return []queueCountValue{
			{name: backchannel.MetricUnregisteredRPs, failed: true},
			{name: backchannel.MetricNoEndpointBacklog, failed: true},
		}
	}
	values := make([]queueCountValue, 0, len(gauges))
	for _, gauge := range gauges {
		values = append(values, queueCountValue{name: gauge.Name, count: gauge.Value})
	}
	return values
}

func (metrics *OperationalMetrics) gatherAsyncJobReliability(ctx context.Context, snapshot *queueMetricsSnapshot) {
	var row struct {
		Retries         uint64   `gorm:"column:retries"`
		LastSuccessUnix *float64 `gorm:"column:last_success_unix"`
	}
	err := metrics.database.WithContext(ctx).Table("async_job").
		Select("COALESCE(SUM(retry_count), 0) AS retries, UNIX_TIMESTAMP(MAX(last_succeeded_at)) AS last_success_unix").
		Scan(&row).Error
	if err != nil {
		metrics.collectionErrors.Add(1)
		snapshot.retriesFailed = true
		return
	}
	snapshot.retries = row.Retries
	if row.LastSuccessUnix != nil {
		snapshot.lastSuccess = *row.LastSuccessUnix
	}
}
