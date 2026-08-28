package httptransport

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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
	if metrics.database == nil {
		metrics.collectionErrors.Add(1)
		return
	}
	sqlDatabase, err := metrics.database.DB()
	if err != nil {
		metrics.collectionErrors.Add(1)
		return
	}
	stats := sqlDatabase.Stats()
	fmt.Fprintf(writer, "# TYPE platform_database_open_connections gauge\nplatform_database_open_connections %d\n", stats.OpenConnections)
	fmt.Fprintf(writer, "# TYPE platform_database_in_use_connections gauge\nplatform_database_in_use_connections %d\n", stats.InUse)
	fmt.Fprintf(writer, "# TYPE platform_database_wait_count_total counter\nplatform_database_wait_count_total %d\n", stats.WaitCount)

	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	metrics.writeQueueCount(ctx, writer, "platform_keycloak_outbox_backlog", "keycloak_authorization_outbox", "status IN ?", []string{"PENDING", "PROCESSING", "RETRY"})
	metrics.writeQueueCount(ctx, writer, "platform_keycloak_outbox_dead", "keycloak_authorization_outbox", "status IN ?", []string{"FAILED", "DEAD"})
	metrics.writeQueueCount(ctx, writer, "platform_async_job_backlog", "async_job", "status IN ?", []string{"PENDING", "RUNNING"})
	metrics.writeQueueCount(ctx, writer, "platform_async_job_dead", "async_job", "status IN ?", []string{"FAILED", "DEAD"})
	metrics.writeQueueCount(ctx, writer, "platform_oidc_backchannel_logout_backlog", "platform_oauth_backchannel_logout_outbox", "status IN ?", []string{"PENDING", "PROCESSING", "RETRY"})
	metrics.writeQueueCount(ctx, writer, "platform_oidc_backchannel_logout_failed", "platform_oauth_backchannel_logout_outbox", "status IN ?", []string{"FAILED"})
	metrics.writeQueueCount(ctx, writer, "platform_oidc_backchannel_logout_delivered", "platform_oauth_backchannel_logout_outbox", "status IN ?", []string{"DELIVERED"})
	metrics.writeAsyncJobReliability(ctx, writer)
}

func (metrics *OperationalMetrics) writeAsyncJobReliability(ctx context.Context, writer http.ResponseWriter) {
	var row struct {
		Retries         uint64   `gorm:"column:retries"`
		LastSuccessUnix *float64 `gorm:"column:last_success_unix"`
	}
	err := metrics.database.WithContext(ctx).Table("async_job").
		Select("COALESCE(SUM(retry_count), 0) AS retries, UNIX_TIMESTAMP(MAX(last_succeeded_at)) AS last_success_unix").
		Scan(&row).Error
	if err != nil {
		metrics.collectionErrors.Add(1)
		return
	}
	fmt.Fprintf(writer, "# TYPE platform_async_job_retries_total counter\nplatform_async_job_retries_total %d\n", row.Retries)
	lastSuccess := float64(0)
	if row.LastSuccessUnix != nil {
		lastSuccess = *row.LastSuccessUnix
	}
	fmt.Fprintf(writer, "# TYPE platform_async_job_last_success_timestamp_seconds gauge\nplatform_async_job_last_success_timestamp_seconds %.3f\n", lastSuccess)
}

func (metrics *OperationalMetrics) writeQueueCount(ctx context.Context, writer http.ResponseWriter, metricName, table, predicate string, values []string) {
	var count int64
	if err := metrics.database.WithContext(ctx).Table(table).Where(predicate, values).Count(&count).Error; err != nil {
		metrics.collectionErrors.Add(1)
		return
	}
	fmt.Fprintf(writer, "# TYPE %s gauge\n%s %d\n", metricName, metricName, count)
}
