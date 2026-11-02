package httptransport

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/backchannel"
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
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return replacer.Replace(value)
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
