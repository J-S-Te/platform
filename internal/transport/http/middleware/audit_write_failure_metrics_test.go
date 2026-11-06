package middleware

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
	"github.com/gin-gonic/gin"
)

func newAuditMetricTestRouter(t *testing.T, recorder AuditRecorder, registry *eventmetrics.Registry) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gate := &auditGate{events: registry}
	router := gin.New()
	router.Use(auditTrailHandler(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)), AuditSource{ApplicationCode: "platform", EnvironmentCode: "dev"}, gate))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		context.String(http.StatusCreated, "business payload")
	})
	return router
}

// 意图事件写入失败：stage=intent 计数一次；随后被快速失败窗口拒绝的请求不再尝试写入，
// 只有“审计不可用”这一后果被计数，失败关闭语义保持不变。
func TestAuditTrailCountsIntentWriteFailureAndUnavailableRejection(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	registry := eventmetrics.NewRegistry()
	router := newAuditMetricTestRouter(t, failingAuditRecorder{err: errors.New("audit store unavailable")}, registry)

	first := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if first.Code != http.StatusServiceUnavailable {
		t.Fatalf("intent failure status = %d, want 503", first.Code)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageIntent); got != 1 {
		t.Fatalf("intent stage count = %d, want 1", got)
	}

	blocked := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if blocked.Code != http.StatusServiceUnavailable || !strings.Contains(blocked.Body.String(), "AUDIT_UNAVAILABLE") {
		t.Fatalf("blocked status=%d body=%s, want 503 AUDIT_UNAVAILABLE", blocked.Code, blocked.Body.String())
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageUnavailable); got != 1 {
		t.Fatalf("unavailable stage count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageIntent); got != 1 {
		t.Fatalf("rejected request must not retry the intent write, intent count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResult); got != 0 {
		t.Fatalf("rejected request has no result event, result count = %d, want 0", got)
	}
}

// 结果事件同步重试后仍失败、但死信落库成功：stage=result 计数，dead_letter 不计，
// 业务响应照常放行（审计事实已在库，不改失败关闭语义）。
func TestAuditTrailCountsResultWriteFailureWithoutDeadLetter(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	registry := eventmetrics.NewRegistry()
	script := &auditRecorderScript{ingestErrs: []error{nil, errors.New("audit result unavailable"), errors.New("audit result unavailable")}}
	router := newAuditMetricTestRouter(t, script, registry)

	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusCreated {
		t.Fatalf("dead-letter fallback status = %d, want 201; body=%s", response.Code, response.Body.String())
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResult); got != 1 {
		t.Fatalf("result stage count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageDeadLetter); got != 0 {
		t.Fatalf("dead_letter stage count = %d, want 0", got)
	}
	if len(script.spooled) != 1 {
		t.Fatalf("dead letter spool count = %d, want 1", len(script.spooled))
	}
}

// 意图已在库，但结果事件与死信都写不进去：stage=result 与 stage=dead_letter 各计一次，
// 请求被 503 拒绝（既有语义），指标不改变该判定。
func TestAuditTrailCountsResultAndDeadLetterWriteFailures(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	registry := eventmetrics.NewRegistry()
	script := &auditRecorderScript{
		ingestErrs: []error{nil, errors.New("audit result unavailable"), errors.New("audit result unavailable")},
		spoolErrs:  []error{errors.New("audit dead letter unavailable")},
	}
	router := newAuditMetricTestRouter(t, script, registry)

	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "AUDIT_RESULT_WRITE_REJECTED") {
		t.Fatalf("status=%d body=%s, want 503 AUDIT_RESULT_WRITE_REJECTED", response.Code, response.Body.String())
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResult); got != 1 {
		t.Fatalf("result stage count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageDeadLetter); got != 1 {
		t.Fatalf("dead_letter stage count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageIntent); got != 0 {
		t.Fatalf("intent stage count = %d, want 0", got)
	}
}

// 非必开模式的单条终态事件写入失败：保持“尽力而为 + 日志”语义，并按 stage=result 计数。
func TestAuditTrailCountsBestEffortResultFailure(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	registry := eventmetrics.NewRegistry()
	router := newAuditMetricTestRouter(t, failingAuditRecorder{err: errors.New("audit store unavailable")}, registry)

	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusCreated || response.Body.String() != "business payload" {
		t.Fatalf("status=%d body=%s, want 201 business payload", response.Code, response.Body.String())
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResult); got != 1 {
		t.Fatalf("result stage count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageIntent); got != 0 {
		t.Fatalf("intent stage count = %d, want 0", got)
	}
}
