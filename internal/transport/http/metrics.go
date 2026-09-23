package httptransport

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// OperationalMetrics 提供不依赖外部 SDK 的最低限度 Prometheus 指标。
// 只输出聚合运行数据，不包含租户、用户、业务单号或 URL 参数，避免高基数和隐私泄漏。
type OperationalMetrics struct {
	database         *gorm.DB
	requests         atomic.Uint64
	errors           atomic.Uint64
	durationNanos    atomic.Uint64
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
}

// defaultQueueMetricsTTL 是队列统计默认抓取缓存时长（SEC-B4）。
const defaultQueueMetricsTTL = 10 * time.Second

type queueCountValue struct {
	name   string
	count  int64
	failed bool
}

// queueMetricsSnapshot 是一次队列/可靠性采集的完整结果，缓存与输出解耦。
type queueMetricsSnapshot struct {
	counts        []queueCountValue
	retries       uint64
	retriesFailed bool
	lastSuccess   float64
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
	return &OperationalMetrics{database: database}
}

// Middleware 记录请求总量、错误量、累计延迟以及认证授权失败。
func (metrics *OperationalMetrics) Middleware() gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		startedAt := time.Now()
		ginContext.Next()
		status := ginContext.Writer.Status()
		metrics.requests.Add(1)
		metrics.durationNanos.Add(uint64(time.Since(startedAt)))
		if status >= http.StatusBadRequest {
			metrics.errors.Add(1)
		}
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
	requests := metrics.requests.Load()
	fmt.Fprintf(writer, "# TYPE platform_http_requests_total counter\nplatform_http_requests_total %d\n", requests)
	fmt.Fprintf(writer, "# TYPE platform_http_errors_total counter\nplatform_http_errors_total %d\n", metrics.errors.Load())
	fmt.Fprintf(writer, "# TYPE platform_http_request_duration_seconds summary\nplatform_http_request_duration_seconds_sum %.9f\nplatform_http_request_duration_seconds_count %d\n", float64(metrics.durationNanos.Load())/float64(time.Second), requests)
	fmt.Fprintf(writer, "# TYPE platform_authz_failures_total counter\nplatform_authz_failures_total %d\n", metrics.authzFailures.Load())
	fmt.Fprintf(writer, "# TYPE platform_oidc_login_failures_total counter\nplatform_oidc_login_failures_total %d\n", metrics.oidcFailures.Load())
	metrics.writeDatabaseMetrics(writer, request.Context())
	fmt.Fprintf(writer, "# TYPE platform_metric_collection_errors_total counter\nplatform_metric_collection_errors_total %d\n", metrics.collectionErrors.Load())
}

func (metrics *OperationalMetrics) writeDatabaseMetrics(writer http.ResponseWriter, parent context.Context) {
	if metrics.database == nil && metrics.gatherQueues == nil {
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
	for _, value := range snapshot.counts {
		if value.failed {
			continue
		}
		fmt.Fprintf(writer, "# TYPE %s gauge\n%s %d\n", value.name, value.name, value.count)
	}
	if !snapshot.retriesFailed {
		fmt.Fprintf(writer, "# TYPE platform_async_job_retries_total counter\nplatform_async_job_retries_total %d\n", snapshot.retries)
		fmt.Fprintf(writer, "# TYPE platform_async_job_last_success_timestamp_seconds gauge\nplatform_async_job_last_success_timestamp_seconds %.3f\n", snapshot.lastSuccess)
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
	if metrics.gatherQueues != nil {
		return metrics.gatherQueues(ctx)
	}
	var snapshot queueMetricsSnapshot
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
	return snapshot
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
