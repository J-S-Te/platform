package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

type auditRecorderStub struct{ input application.EventInput }

func (stub *auditRecorderStub) Ingest(_ context.Context, _ string, input application.EventInput) (domain.Receipt, error) {
	stub.input = input
	return domain.Receipt{}, nil
}

// failingAuditRecorder 模拟审计管道故障（连接断开、审计库不可写等）。
type failingAuditRecorder struct{ err error }

func (recorder failingAuditRecorder) Ingest(context.Context, string, application.EventInput) (domain.Receipt, error) {
	return domain.Receipt{}, recorder.err
}

// capturingLogHandler 捕获结构化日志记录，供测试断言错误日志字段。
type capturingLogHandler struct{ records []slog.Record }

func (handler *capturingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (handler *capturingLogHandler) Handle(_ context.Context, record slog.Record) error {
	handler.records = append(handler.records, record)
	return nil
}
func (handler *capturingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return handler }
func (handler *capturingLogHandler) WithGroup(string) slog.Handler      { return handler }

func newAuditTrailTestRouter(recorder AuditRecorder, logger *slog.Logger) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(AuditTrail(recorder, logger))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		// 写入足够大的响应体，验证必开模式缓冲/提交语义；正常用例体积远小于缓冲上限。
		context.String(http.StatusCreated, "business payload")
	})
	router.GET("/api/v1/users", func(context *gin.Context) { context.String(http.StatusOK, "read payload") })
	router.GET("/api/v1/audit/events", func(context *gin.Context) { context.String(http.StatusOK, "audit read payload") })
	return router
}

func newAuditTrailRequest(router *gin.Engine, method, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	request = request.WithContext(requestctx.WithRequestID(request.Context(), "req-audit-1"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestAuditTrailUsesSessionLoginIPAndRetainsRequestIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/write", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodPost, "/write", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "user-1"},
		LoginIP: net.ParseIP("203.0.113.10"),
	}))
	request = request.WithContext(requestctx.WithClientIP(request.Context(), "198.51.100.20"))
	router.ServeHTTP(httptest.NewRecorder(), request)

	if got := recorder.input.SourceIP; got != "203.0.113.10" {
		t.Fatalf("SourceIP = %q, want session login IP", got)
	}
	if got := recorder.input.Metadata["request_source_ip"]; got != "198.51.100.20" {
		t.Fatalf("request_source_ip = %v, want current request IP", got)
	}
}

func TestAuditTrailFallsBackToCurrentRequestIPWithoutSessionLoginIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/write", func(context *gin.Context) { context.Status(http.StatusNoContent) })

	request := httptest.NewRequest(http.MethodPost, "/write", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	request = request.WithContext(requestctx.WithClientIP(request.Context(), "198.51.100.20"))
	router.ServeHTTP(httptest.NewRecorder(), request)

	if got := recorder.input.SourceIP; got != "198.51.100.20" {
		t.Fatalf("SourceIP = %q, want current request IP", got)
	}
	if _, found := recorder.input.Metadata["request_source_ip"]; found {
		t.Fatal("request_source_ip should be omitted when it matches SourceIP")
	}
}

func TestAuditRiskLevelUsesBusinessSensitivity(t *testing.T) {
	tests := []struct {
		name, method, route string
		status, want        int
	}{
		{name: "role write", method: http.MethodPut, route: "/api/v1/roles/:role_id", status: http.StatusOK, want: 3},
		{name: "audit read", method: http.MethodGet, route: "/api/v1/audit/events", status: http.StatusOK, want: 2},
		{name: "forbidden read", method: http.MethodGet, route: "/api/v1/users", status: http.StatusForbidden, want: 2},
		{name: "ordinary create", method: http.MethodPost, route: "/api/v1/dictionaries", status: http.StatusCreated, want: 1},
	}
	levels := map[string]int{"LOW": 1, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := levels[auditRiskLevel(test.method, test.route, test.status)]; got != test.want {
				t.Fatalf("risk level rank = %d, want %d", got, test.want)
			}
		})
	}
}

func TestShouldRecordAuditTrailNeverSuppressesBusinessAuditRoutes(t *testing.T) {
	// 安全审查 SEC-D4a：业务审计路由的通用轨迹不再抑制——业务审计摄取失败时，
	// 成功的授权变更必须仍然有（且仅有这一条兜底路径以外的）通用记录，不允许零记录。
	userRoute := "/api/v1/users/:user_id/applications/:application_code/access"
	subjectRoute := "/api/v1/authorization-subjects/:subject_type/:subject_id/applications/:application_code/access"
	catalogRoute := "/api/v1/applications/:application_id/authorization-catalog"
	if !shouldRecordAuditTrail(http.MethodPut, userRoute) {
		t.Fatal("user application access PUT must produce a generic audit record (SEC-D4a)")
	}
	if !shouldRecordAuditTrail(http.MethodDelete, userRoute) {
		t.Fatal("user application access DELETE must produce a generic audit record (SEC-D4a)")
	}
	if !shouldRecordAuditTrail(http.MethodDelete, subjectRoute) {
		t.Fatal("subject access deletion must produce a generic audit record (SEC-D4a)")
	}
	if !shouldRecordAuditTrail(http.MethodPut, subjectRoute) {
		t.Fatal("subject access update must produce a generic audit record (SEC-D4a)")
	}
	if !shouldRecordAuditTrail(http.MethodPut, catalogRoute) {
		t.Fatal("authorization catalog publication must produce a generic audit record (SEC-D4a)")
	}
}

func TestAuditTrailRecordsGenericEventForBusinessAuditRoute(t *testing.T) {
	// 端到端兜底断言：即使业务审计（applicationaccess 层）摄取失败，通用轨迹中间件
	// 对该路由也必须实际产出一条记录——即“业务审计失败 → 通用轨迹仍有记录”。
	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.PUT("/api/v1/users/:user_id/applications/:application_code/access", func(context *gin.Context) { context.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodPut, "/api/v1/users/user-1/applications/portal/access", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	router.ServeHTTP(httptest.NewRecorder(), request)
	if got := recorder.input.EventType; got != "PUT /api/v1/users/:user_id/applications/:application_code/access" {
		t.Fatalf("EventType = %q, want generic record for business audit route", got)
	}
	if got := recorder.input.Result; got != "SUCCESS" {
		t.Fatalf("Result = %q, want SUCCESS", got)
	}
}

// 安全审查 SEC-D5：body寻址创建没有路径 id——处理器登记的目标对象 id 必须同时进入
// 事件 ResourceID 与 metadata["resource_id"]，即“创建后审计记录含目标 id”的端到端断言。
func TestAuditTrailRecordsHandlerRegisteredResourceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/api/v1/users", func(context *gin.Context) {
		// 模拟 adaptHandler 包装的 net/http 处理器在创建成功后的登记动作。
		MarkCreatedResource(context.Request, "usr_created_1")
		context.Status(http.StatusCreated)
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	router.ServeHTTP(httptest.NewRecorder(), request)
	if got := recorder.input.ResourceID; got != "usr_created_1" {
		t.Fatalf("ResourceID = %q, want handler-registered id", got)
	}
	if got := recorder.input.Metadata["resource_id"]; got != "usr_created_1" {
		t.Fatalf("metadata resource_id = %v, want usr_created_1", got)
	}
}

// 未登记目标 id 时保持原路径参数回退，不产生空 metadata 键。
func TestAuditTrailFallsBackToPathResourceIDWithoutHandlerRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/api/v1/config/items/:item_id", func(context *gin.Context) { context.Status(http.StatusCreated) })
	request := httptest.NewRequest(http.MethodPost, "/api/v1/config/items/cfg-1", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	router.ServeHTTP(httptest.NewRecorder(), request)
	if got := recorder.input.ResourceID; got != "cfg-1" {
		t.Fatalf("ResourceID = %q, want path parameter cfg-1", got)
	}
	if _, exists := recorder.input.Metadata["resource_id"]; exists {
		t.Fatal("metadata resource_id should be absent when handler did not register one")
	}
}

func TestAuditResourceIDUsesInnermostResource(t *testing.T) {
	context := &gin.Context{Params: gin.Params{
		{Key: "user_id", Value: "user-1"},
		{Key: "application_id", Value: "application-1"},
	}}
	if got := auditResourceID(context); got != "application-1" {
		t.Fatalf("resource ID = %q, want application-1", got)
	}
}

// 安全审查 AUD-2026-005：必开模式下审计 Ingest 失败必须拒绝请求——业务响应被丢弃，
// 客户端收到 503 AUDIT_WRITE_REJECTED，绝不能“无审计的成功”。
func TestAuditTrailRequiredModeRejectsWhenIngestFails(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	router := newAuditTrailTestRouter(failingAuditRecorder{err: errors.New("audit store unavailable")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "AUDIT_WRITE_REJECTED") {
		t.Fatalf("body = %s, want AUDIT_WRITE_REJECTED", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "business payload") {
		t.Fatal("buffered business response must be discarded, not committed, when audit ingest fails")
	}
}

// 必开模式下审计成功：缓冲的业务响应原样提交，行为与非必开模式一致。
func TestAuditTrailRequiredModeFlushesBufferedResponseWhenIngestSucceeds(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	recorder := &auditRecorderStub{}
	router := newAuditTrailTestRouter(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if response.Body.String() != "business payload" {
		t.Fatalf("body = %q, want business payload", response.Body.String())
	}
	if recorder.input.Result != "SUCCESS" {
		t.Fatalf("audit Result = %q, want SUCCESS", recorder.input.Result)
	}
}

// 非必开模式保持旧行为：Ingest 失败仅输出结构化错误日志，业务响应照常返回。
func TestAuditTrailNonRequiredModeKeepsLegacyBehaviorOnIngestFailure(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	logs := &capturingLogHandler{}
	router := newAuditTrailTestRouter(failingAuditRecorder{err: errors.New("audit store unavailable")}, slog.New(logs))
	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusCreated)
	}
	if response.Body.String() != "business payload" {
		t.Fatalf("body = %q, want business payload", response.Body.String())
	}
	found := false
	for _, record := range logs.records {
		if record.Message != "write platform audit event" {
			continue
		}
		found = true
		if record.Level != slog.LevelError {
			t.Fatalf("audit failure log level = %s, want ERROR", record.Level)
		}
		var path, requestID string
		record.Attrs(func(attr slog.Attr) bool {
			switch attr.Key {
			case "path":
				path = attr.Value.String()
			case "request_id":
				requestID = attr.Value.String()
			}
			return true
		})
		if path != "/api/v1/dictionaries" {
			t.Fatalf("audit failure log path = %q, want /api/v1/dictionaries", path)
		}
		if requestID != "req-audit-1" {
			t.Fatalf("audit failure log request_id = %q, want req-audit-1", requestID)
		}
	}
	if !found {
		t.Fatal("audit ingest failure must emit a structured error log in non-required mode")
	}
}

// 必开模式只影响会被记录审计事件的请求：普通只读路由不受 Ingest 故障影响；
// 而审计查询（/api/v1/audit/* 的 GET，属于被记录的敏感读）与写请求同样受保护。
func TestAuditTrailRequiredModeOnlyGuardsRecordedRequests(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	router := newAuditTrailTestRouter(failingAuditRecorder{err: errors.New("audit store unavailable")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if response := newAuditTrailRequest(router, http.MethodGet, "/api/v1/users"); response.Code != http.StatusOK {
		t.Fatalf("unrecorded read status = %d, want %d (read routes must not be rejected)", response.Code, http.StatusOK)
	}
	if response := newAuditTrailRequest(router, http.MethodGet, "/api/v1/audit/events"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("recorded sensitive read status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
}

// 缓冲超限时降级为旧行为：响应已 write-through 提交无法撤回，只能保留错误日志并告警。
func TestAuditTrailRequiredModeDegradesToWriteThroughWhenBufferOverflows(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	logs := &capturingLogHandler{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/dictionaries", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()
	overflowRouter := gin.New()
	overflowRouter.Use(AuditTrail(failingAuditRecorder{err: errors.New("audit store unavailable")}, slog.New(logs)))
	overflowRouter.POST("/api/v1/dictionaries", func(context *gin.Context) {
		context.Header("X-Business-Header", "value")
		context.Status(http.StatusOK)
		large := strings.Repeat("x", maxPlatformAuditBufferedResponseBytes+1)
		_, _ = context.Writer.WriteString(large)
	})
	overflowRouter.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("overflowed response status = %d, want %d (write-through must keep business status)", response.Code, http.StatusOK)
	}
	if len(response.Body.String()) != maxPlatformAuditBufferedResponseBytes+1 {
		t.Fatalf("overflowed response size = %d, want %d", len(response.Body.String()), maxPlatformAuditBufferedResponseBytes+1)
	}
	if response.Header().Get("X-Business-Header") != "value" {
		t.Fatal("business headers must survive write-through degradation")
	}
	warned := false
	for _, record := range logs.records {
		if record.Level == slog.LevelWarn && strings.Contains(record.Message, "degrading to write-through") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("buffer overflow must emit a warning log when degrading")
	}
}
