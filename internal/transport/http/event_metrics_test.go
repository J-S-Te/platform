package httptransport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
	"github.com/gin-gonic/gin"
)

// 事件级计数器（登录失败 reason、审计写入失败 stage）必须出现在平台 /metrics 的同一份
// 手写 exposition 里，并始终输出全部受约束枚举（含 0），供 rate()/absent() 使用。
func TestOperationalMetricsExposesEventCounters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	metrics := NewOperationalMetrics(nil)
	registry := eventmetrics.NewRegistry()
	metrics.events = registry
	registry.RecordLoginFailure(eventmetrics.LoginFailureRateLimited)
	registry.RecordLoginFailure("tenant-42")
	registry.RecordAuditWriteFailure(eventmetrics.AuditStageIntent)

	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, expected := range []string{
		"# TYPE platform_login_failures_total counter",
		`platform_login_failures_total{reason="rate_limited"} 1`,
		`platform_login_failures_total{reason="other"} 1`,
		`platform_login_failures_total{reason="invalid_credentials"} 0`,
		"# TYPE platform_audit_write_failures_total counter",
		`platform_audit_write_failures_total{stage="intent"} 1`,
		`platform_audit_write_failures_total{stage="result"} 0`,
		`platform_audit_write_failures_total{stage="dead_letter"} 0`,
		`platform_audit_write_failures_total{stage="result_streamed"} 0`,
		`platform_audit_write_failures_total{stage="unavailable"} 0`,
		// 原有启发式指标保留（C2 只新增事件级指标，不移除既有口径）。
		"platform_oidc_login_failures_total",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, `reason="tenant-42"`) {
		t.Fatalf("unknown login failure reason leaked into labels:\n%s", body)
	}
}
