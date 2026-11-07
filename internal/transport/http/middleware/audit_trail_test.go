package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	eventmetrics "github.com/J-S-Te/Basic-Platform/internal/shared/metrics"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
	"github.com/gin-gonic/gin"
)

// auditRecorderStub 记录全部 Ingest 事件（input 保持“最后一条”语义）并接受死信落库。
type auditRecorderStub struct {
	input   application.EventInput
	inputs  []application.EventInput
	spooled []application.EventInput
}

func (stub *auditRecorderStub) Ingest(_ context.Context, _ string, input application.EventInput) (domain.Receipt, error) {
	stub.input = input
	stub.inputs = append(stub.inputs, input)
	return domain.Receipt{}, nil
}

func (stub *auditRecorderStub) Spool(_ context.Context, _ string, input application.EventInput) error {
	stub.spooled = append(stub.spooled, input)
	return nil
}

// failingAuditRecorder 模拟审计管道故障（连接断开、审计库不可写等）。
type failingAuditRecorder struct{ err error }

func (recorder failingAuditRecorder) Ingest(context.Context, string, application.EventInput) (domain.Receipt, error) {
	return domain.Receipt{}, recorder.err
}

func (recorder failingAuditRecorder) Spool(context.Context, string, application.EventInput) error {
	return recorder.err
}

// auditRecorderScript 按调用顺序控制 Ingest / Spool 的结果，用于覆盖 BIZ-RUNTIME-11 的
// “意图→结果→死信”三段路径。结果列表耗尽后一律成功。
type auditRecorderScript struct {
	ingestErrs   []error
	spoolErrs    []error
	ingested     []application.EventInput
	ingestTenant []string
	spooled      []application.EventInput
	spoolTenant  []string
}

func (script *auditRecorderScript) Ingest(_ context.Context, tenantID string, input application.EventInput) (domain.Receipt, error) {
	script.ingested = append(script.ingested, input)
	script.ingestTenant = append(script.ingestTenant, tenantID)
	if len(script.ingestErrs) == 0 {
		return domain.Receipt{}, nil
	}
	err := script.ingestErrs[0]
	script.ingestErrs = script.ingestErrs[1:]
	return domain.Receipt{}, err
}

func (script *auditRecorderScript) Spool(_ context.Context, tenantID string, input application.EventInput) error {
	script.spooled = append(script.spooled, input)
	script.spoolTenant = append(script.spoolTenant, tenantID)
	if len(script.spoolErrs) == 0 {
		return nil
	}
	err := script.spoolErrs[0]
	script.spoolErrs = script.spoolErrs[1:]
	return err
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
	request = request.WithContext(requestctx.WithClientIPSource(request.Context(), "198.51.100.20", "TRUSTED_PROXY"))
	router.ServeHTTP(httptest.NewRecorder(), request)

	if got := recorder.input.SourceIP; got != "203.0.113.10" {
		t.Fatalf("SourceIP = %q, want session login IP", got)
	}
	// 统一审计来源协议：登录地址与操作地址必须分别上报，不能被合并成一个字段。
	if got := recorder.input.LoginIP; got != "203.0.113.10" {
		t.Fatalf("LoginIP = %q, want session login IP", got)
	}
	if got := recorder.input.ClientIP; got != "198.51.100.20" {
		t.Fatalf("ClientIP = %q, want current request IP", got)
	}
	if got := recorder.input.IPSource; got != "TRUSTED_PROXY" {
		t.Fatalf("IPSource = %q, want TRUSTED_PROXY", got)
	}
	if got := recorder.input.Metadata["request_source_ip"]; got != "198.51.100.20" {
		t.Fatalf("request_source_ip = %v, want current request IP", got)
	}
}

// 同一会话换网络：登录地址保持不变，操作地址跟随新请求。
func TestAuditTrailKeepsLoginIPStableWhenRequestNetworkChanges(t *testing.T) {
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
	// 办公网换到 VPN：登录地址仍旧，操作地址是新的。
	request = request.WithContext(requestctx.WithClientIP(request.Context(), "10.8.0.5"))
	router.ServeHTTP(httptest.NewRecorder(), request)

	if recorder.input.LoginIP != "203.0.113.10" {
		t.Fatalf("LoginIP = %q，登录地址必须保持不变", recorder.input.LoginIP)
	}
	if recorder.input.ClientIP != "10.8.0.5" {
		t.Fatalf("ClientIP = %q，操作地址必须跟随请求", recorder.input.ClientIP)
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

// 审计导出下载契约变更：签发一次性票据（POST）与凭票据兑换文件（GET）都属于会泄露
// 安全信息的审计操作，必须各留一条通用审计轨迹；GET 下载尤其不能因为“只读”而漏记。
func TestShouldRecordAuditTrailCoversExportDownloadTickets(t *testing.T) {
	issueRoute := "/api/v1/audit/export-jobs/:job_id/download-tickets"
	downloadRoute := "/api/v1/audit/export-jobs/:job_id/download"
	if !shouldRecordAuditTrail(http.MethodPost, issueRoute) {
		t.Fatal("export download ticket issuance must produce a generic audit record")
	}
	if !shouldRecordAuditTrail(http.MethodGet, downloadRoute) {
		t.Fatal("export download must produce a generic audit record")
	}
}

// 响应体扣留口径必须只对“真正需要正文的路由”成立：审计导出下载是流式二进制响应，
// 留痕只需要方法/路径/主体/状态码，缓冲正文既不增加审计价值又会写临时文件或 503。
func TestShouldCaptureAuditResponseBodySkipsOnlyExportDownload(t *testing.T) {
	downloadRoute := "/api/v1/audit/export-jobs/:job_id/download"
	if shouldCaptureAuditResponseBody(downloadRoute) {
		t.Fatal("export download must not buffer its response body")
	}
	// 未命中路由模板时的原始路径口径同样要豁免（auditRequestRoute 会退回 URL.Path）。
	if shouldCaptureAuditResponseBody("/api/v1/audit/export-jobs/job-1/download") {
		t.Fatal("raw export download path must not buffer its response body")
	}
	for _, route := range []string{
		"/api/v1/audit/export-jobs/:job_id/download-tickets",
		"/api/v1/audit/events",
		"/api/v1/audit/events/batch",
		"/api/v1/users",
		"/api/v1/dictionaries",
	} {
		if !shouldCaptureAuditResponseBody(route) {
			t.Fatalf("%s 必须保持原有的响应体扣留语义", route)
		}
	}
}

// 审计导出下载的大响应必须原样流给客户端：不出现 spill 临时文件、不因 512MiB 硬上限
// 变成 AUDIT_RESPONSE_TOO_LARGE 503，同时仍然产生意图 + 结果两条审计记录，并在
// metadata 里如实标注 response_body=not_captured。
func TestAuditTrailExportDownloadStreamsLargeBodyWithoutBuffering(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	originalHardLimit := maxPlatformAuditSpilledResponseBytes
	// 把硬上限压到内存上限附近：任何缓冲都会立刻超限并被拒绝，便于证明下载路由没有走缓冲。
	maxPlatformAuditSpilledResponseBytes = maxPlatformAuditBufferedResponseBytes + 10
	defer func() { maxPlatformAuditSpilledResponseBytes = originalHardLimit }()
	payload := strings.Repeat("z", maxPlatformAuditBufferedResponseBytes*4)

	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.GET("/api/v1/audit/export-jobs/:job_id/download", func(context *gin.Context) {
		context.Header("Content-Type", "application/zip")
		_, _ = context.Writer.WriteString(payload)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit/export-jobs/job-1/download", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (下载不得因响应体未扣留被拒): body = %s", response.Code, response.Body.String())
	}
	if response.Body.String() != payload {
		t.Fatalf("body length = %d, want %d (下载内容必须逐字节完整)", response.Body.Len(), len(payload))
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", contentType)
	}
	// 不缓冲 ⇒ 不溢写：临时目录里不允许出现任何 spill 文件。
	assertNoSpillFilesLeft(t, tmp)

	if len(recorder.inputs) != 2 {
		t.Fatalf("audit events = %d, want 2 (intent + result)", len(recorder.inputs))
	}
	if recorder.inputs[0].Metadata["phase"] != "intent" {
		t.Fatalf("first event phase = %v, want intent", recorder.inputs[0].Metadata["phase"])
	}
	result := recorder.inputs[1]
	if result.EventType != "GET /api/v1/audit/export-jobs/:job_id/download" {
		t.Fatalf("EventType = %q, want the export download route", result.EventType)
	}
	if result.ActorID != "user-1" {
		t.Fatalf("audit actor = %q, want the downloading user", result.ActorID)
	}
	if result.Result != "SUCCESS" {
		t.Fatalf("Result = %q, want SUCCESS", result.Result)
	}
	if result.Metadata["status_code"] != http.StatusOK {
		t.Fatalf("metadata status_code = %v, want %d", result.Metadata["status_code"], http.StatusOK)
	}
	if result.Metadata["response_body"] != "not_captured" {
		t.Fatalf("metadata response_body = %v, want not_captured", result.Metadata["response_body"])
	}
	if _, found := result.Metadata["audit_rejection"]; found {
		t.Fatal("未采集正文不等于审计失败：下载事件不得带 audit_rejection")
	}
}

// 豁免只针对下载路由：同一硬上限下，审计查询路由仍然被扣留并如实拒绝超限响应。
func TestAuditTrailAuditQueryKeepsBufferingSemantics(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	originalHardLimit := maxPlatformAuditSpilledResponseBytes
	maxPlatformAuditSpilledResponseBytes = maxPlatformAuditBufferedResponseBytes + 10
	defer func() { maxPlatformAuditSpilledResponseBytes = originalHardLimit }()

	gin.SetMode(gin.TestMode)
	recorder := &auditRecorderStub{}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.GET("/api/v1/audit/events", func(context *gin.Context) {
		_, _ = context.Writer.WriteString(strings.Repeat("x", maxPlatformAuditBufferedResponseBytes+100))
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit/events", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "AUDIT_RESPONSE_TOO_LARGE") {
		t.Fatalf("body = %s, want AUDIT_RESPONSE_TOO_LARGE", response.Body.String())
	}
	if recorder.input.Metadata["audit_rejection"] != "AUDIT_RESPONSE_TOO_LARGE" {
		t.Fatalf("metadata audit_rejection = %v, want AUDIT_RESPONSE_TOO_LARGE", recorder.input.Metadata["audit_rejection"])
	}
	if _, found := recorder.input.Metadata["response_body"]; found {
		t.Fatal("被扣留的路由不应声明 response_body=not_captured")
	}
}

// 下载路由的响应已经直接流给客户端，因此“结果事件 + 死信都写不进去”时不能再补写 503：
// 那会污染一个已经完整送达的文件。此时只保留意图事件与错误日志，响应保持原样。
func TestAuditTrailExportDownloadKeepsStreamedResponseWhenAuditResultUnwritable(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	payload := "zip-bytes-payload"
	logs := &capturingLogHandler{}
	recorder := &auditRecorderScript{
		ingestErrs: []error{nil, errors.New("audit store unavailable"), errors.New("audit store unavailable")},
		spoolErrs:  []error{errors.New("audit dead letter unavailable")},
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(logs)))
	router.GET("/api/v1/audit/export-jobs/:job_id/download", func(context *gin.Context) {
		_, _ = context.Writer.WriteString(payload)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/audit/export-jobs/job-1/download", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if response.Body.String() != payload {
		t.Fatalf("body = %q, want the intact download %q (不得追加 503 报文)", response.Body.String(), payload)
	}
	if len(recorder.ingested) != 3 {
		t.Fatalf("ingest attempts = %d, want 3 (intent + result + one retry)", len(recorder.ingested))
	}
	if len(recorder.spooled) != 1 {
		t.Fatalf("spool attempts = %d, want 1", len(recorder.spooled))
	}
	warned := false
	for _, record := range logs.records {
		if record.Level == slog.LevelError && strings.Contains(record.Message, "already streamed") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("响应已流式提交后审计结论不可写必须记 ERROR 日志")
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

// 安全审查 AUD-2026-005 / BIZ-RUNTIME-11：必开模式下意图事件写不进去必须在业务链之前
// 拒绝请求——客户端收到 503 AUDIT_WRITE_REJECTED，业务 handler 根本不执行，也就没有
// 已生效却拿失败响应的重试陷阱。
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
		t.Fatal("business response must not exist when the intent event is rejected before the handler")
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

// 缓冲超限（进入溢写临时文件）时同样没有直写例外：结果事件与死信都写不进去时仍然拒绝
// 请求，扣留的大响应被丢弃，客户端拿到 503 AUDIT_RESULT_WRITE_REJECTED，绝不出现
// “审计无结论的成功”。
func TestAuditTrailRequiredModeSpillsOversizedResponseAndRejectsWhenResultUnwritable(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	logs := &capturingLogHandler{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/dictionaries", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()
	overflowRouter := gin.New()
	// 意图事件写入成功；结果写入（含一次同步重试）与死信落库全部失败。
	recorder := &auditRecorderScript{
		ingestErrs: []error{nil, errors.New("audit store unavailable"), errors.New("audit store unavailable")},
		spoolErrs:  []error{errors.New("audit dead letter unavailable")},
	}
	overflowRouter.Use(AuditTrail(recorder, slog.New(logs)))
	overflowRouter.POST("/api/v1/dictionaries", func(context *gin.Context) {
		context.Header("X-Business-Header", "value")
		context.Status(http.StatusOK)
		_, _ = context.Writer.WriteString(strings.Repeat("x", maxPlatformAuditBufferedResponseBytes+1))
	})
	overflowRouter.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("oversized response status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "AUDIT_RESULT_WRITE_REJECTED") {
		t.Fatalf("body = %s, want AUDIT_RESULT_WRITE_REJECTED", response.Body.String())
	}
	if strings.Contains(response.Body.String(), strings.Repeat("x", 32)) {
		t.Fatal("spilled business response must be discarded when the audit outcome is rejected")
	}
	if response.Header().Get("X-Business-Header") != "" {
		t.Fatal("business headers must not leak to the client when the request is rejected")
	}
	assertNoSpillFilesLeft(t, tmp)
	warned := false
	for _, record := range logs.records {
		if record.Level == slog.LevelInfo && strings.Contains(record.Message, "spilling to temporary file") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("spilling to temporary file must be logged")
	}
}

// 溢写 + 审计成功：扣留的大响应必须原样（逐字节）提交给客户端，临时文件随即清理。
func TestAuditTrailRequiredModeSpillsOversizedResponseAndFlushesIntactWhenIngestSucceeds(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	recorder := &auditRecorderStub{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/dictionaries", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()
	overflowRouter := gin.New()
	overflowRouter.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	overflowRouter.POST("/api/v1/dictionaries", func(context *gin.Context) {
		context.Header("X-Business-Header", "value")
		context.Status(http.StatusCreated)
		_, _ = context.Writer.WriteString(strings.Repeat("x", maxPlatformAuditBufferedResponseBytes+1))
		_, _ = context.Writer.WriteString("tail")
	})
	overflowRouter.ServeHTTP(response, request)
	want := strings.Repeat("x", maxPlatformAuditBufferedResponseBytes+1) + "tail"
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusCreated)
	}
	if response.Body.String() != want {
		t.Fatalf("spilled response not replayed intact: got %d bytes, want %d", len(response.Body.String()), len(want))
	}
	if response.Header().Get("X-Business-Header") != "value" {
		t.Fatal("business headers must survive the spill/flush round trip")
	}
	if recorder.input.Result != "SUCCESS" {
		t.Fatalf("audit Result = %q, want SUCCESS", recorder.input.Result)
	}
	assertNoSpillFilesLeft(t, tmp)
}

// 超过硬上限的响应无法被完整扣留：审计必须如实记录失败（保留 handler 状态码供追溯），
// 客户端拿到 503 AUDIT_RESPONSE_TOO_LARGE，绝不出现“审计记成功、客户端拿 503”的错位。
func TestAuditTrailRequiredModeRejectsResponseBeyondSpillHardLimit(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	originalHardLimit := maxPlatformAuditSpilledResponseBytes
	maxPlatformAuditSpilledResponseBytes = maxPlatformAuditBufferedResponseBytes + 10
	defer func() { maxPlatformAuditSpilledResponseBytes = originalHardLimit }()
	recorder := &auditRecorderStub{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/dictionaries", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()
	overflowRouter := gin.New()
	overflowRouter.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	overflowRouter.POST("/api/v1/dictionaries", func(context *gin.Context) {
		_, _ = context.Writer.WriteString(strings.Repeat("x", maxPlatformAuditBufferedResponseBytes+100))
	})
	overflowRouter.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "AUDIT_RESPONSE_TOO_LARGE") {
		t.Fatalf("body = %s, want AUDIT_RESPONSE_TOO_LARGE", response.Body.String())
	}
	if recorder.input.Result != "FAILURE" {
		t.Fatalf("audit Result = %q, want FAILURE", recorder.input.Result)
	}
	if recorder.input.Metadata["audit_rejection"] != "AUDIT_RESPONSE_TOO_LARGE" {
		t.Fatalf("audit metadata audit_rejection = %v, want AUDIT_RESPONSE_TOO_LARGE", recorder.input.Metadata["audit_rejection"])
	}
	if recorder.input.Metadata["handler_status_code"] != http.StatusOK {
		t.Fatalf("audit metadata handler_status_code = %v, want %d", recorder.input.Metadata["handler_status_code"], http.StatusOK)
	}
	// 业务链已执行完：超大响应只影响审计扣留，不改变业务事实是否生效，
	// 因此必须如实说明 operation_applied，不得再对已生效的写操作谎报“未被确认”。
	body := response.Body.String()
	if !strings.Contains(body, `"operation_applied":"true"`) {
		t.Fatalf("body = %s, want operation_applied=true (the business fact is committed)", body)
	}
	if !strings.Contains(body, `"next_action":"do_not_retry"`) {
		t.Fatalf("body = %s, want next_action=do_not_retry", body)
	}
	if strings.Contains(body, "未被确认") {
		t.Fatalf("body = %s, must not claim the operation is unconfirmed after commit", body)
	}
	assertNoSpillFilesLeft(t, tmp)
}

// 溢写临时文件必须在请求结束时清理：提交、拒绝（含 ingest 失败与超硬上限）路径均不留痕。
func assertNoSpillFilesLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read TMPDIR %q: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("spill temp files left behind in %s: %d entries", dir, len(entries))
	}
}

// BIZ-RUNTIME-11：审计必开模式下，意图事件写不进去时业务链根本不会执行；快速失败窗口
// 让后续重试也在业务链之前被拒绝，绝不重复执行非幂等业务写入。
func TestAuditTrailRequiredModeFailsFastBeforeBusinessWriteAfterAuditFailure(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	businessCalls := 0
	router := gin.New()
	router.Use(AuditTrail(failingAuditRecorder{err: errors.New("audit store unavailable")}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		businessCalls++
		context.String(http.StatusCreated, "business payload")
	})

	first := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "AUDIT_WRITE_REJECTED") {
		t.Fatalf("first failure status=%d body=%s; want 503 AUDIT_WRITE_REJECTED", first.Code, first.Body.String())
	}
	if businessCalls != 0 {
		t.Fatalf("intent failure must reject before the business handler, calls = %d, want 0", businessCalls)
	}

	retry := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if retry.Code != http.StatusServiceUnavailable || !strings.Contains(retry.Body.String(), "AUDIT_UNAVAILABLE") {
		t.Fatalf("retry status=%d body=%s; want 503 AUDIT_UNAVAILABLE", retry.Code, retry.Body.String())
	}
	if businessCalls != 0 {
		t.Fatalf("retry must be rejected before the business write, handler calls = %d, want 0", businessCalls)
	}
	if strings.Contains(retry.Body.String(), "business payload") {
		t.Fatal("retry must not replay a business response")
	}
}

// 快速失败只用于审计必开模式：非必开模式保持既有“审计失败仍返回业务响应”的行为，
// 且不会因为一次失败而挡住后续请求。
func TestAuditTrailNonRequiredModeDoesNotBlockBusinessWritesAfterAuditFailure(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	businessCalls := 0
	router := gin.New()
	router.Use(AuditTrail(failingAuditRecorder{err: errors.New("audit store unavailable")}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		businessCalls++
		context.String(http.StatusCreated, "business payload")
	})
	for attempt := 1; attempt <= 2; attempt++ {
		response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
		if response.Code != http.StatusCreated || response.Body.String() != "business payload" {
			t.Fatalf("attempt %d status=%d body=%s; want 201 business payload", attempt, response.Code, response.Body.String())
		}
	}
	if businessCalls != 2 {
		t.Fatalf("non-required mode must keep executing business writes, calls = %d, want 2", businessCalls)
	}
}

// 快速失败窗口到期后必须重新放行，避免一次审计抖动把写入口永久关闭。
func TestAuditGateReopensAfterCooldown(t *testing.T) {
	gate := &auditGate{}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	if gate.blocked(now) {
		t.Fatal("fresh gate must not block requests")
	}
	gate.recordFailure(now)
	if !gate.blocked(now) {
		t.Fatal("gate must block requests inside the cooldown window")
	}
	if gate.blocked(now.Add(auditAvailabilityCooldown)) {
		t.Fatal("gate must reopen once the cooldown elapsed")
	}
	gate.recordFailure(now)
	gate.recordSuccess()
	if gate.blocked(now) {
		t.Fatal("successful audit write must clear the fast-fail window")
	}
}

// 审计恢复后（窗口到期且写入成功）必须关闭快速失败窗口，业务流程继续正常完成。
func TestAuditTrailRequiredModeResumesAfterGateReopens(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	recorder := &auditRecorderStub{}
	gate := &auditGate{}
	now := time.Now().UTC()
	gate.recordFailure(now)
	businessCalls := 0
	router := gin.New()
	router.Use(auditTrailHandler(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)), AuditSource{ApplicationCode: "platform", EnvironmentCode: "dev"}, gate))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		businessCalls++
		context.String(http.StatusCreated, "business payload")
	})

	blocked := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if blocked.Code != http.StatusServiceUnavailable || businessCalls != 0 {
		t.Fatalf("gated request status=%d calls=%d; want 503 before business write", blocked.Code, businessCalls)
	}

	// 冷却窗口到期：允许探测，审计成功后窗口立即关闭，后续请求照常执行。
	gate.mu.Lock()
	gate.reopenAt = time.Now().UTC().Add(-time.Second)
	gate.mu.Unlock()
	if response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries"); response.Code != http.StatusCreated {
		t.Fatalf("probe after cooldown status=%d body=%s; want 201", response.Code, response.Body.String())
	}
	if businessCalls != 1 {
		t.Fatalf("probe after cooldown business calls = %d, want 1", businessCalls)
	}
	if gate.blocked(time.Now().UTC()) {
		t.Fatal("successful audit write must reopen the gate")
	}
	if response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries"); response.Code != http.StatusCreated {
		t.Fatalf("post-recovery status=%d body=%s; want 201", response.Code, response.Body.String())
	}
}

// BIZ-RUNTIME-11 步骤 1+2：必开模式一次请求产生“意图 + 结果”两条事件，顺序与关联可核验。
func TestAuditTrailRequiredModeWritesIntentBeforeHandlerAndResultAfter(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	recorder := &auditRecorderStub{}
	router := newAuditTrailTestRouter(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusCreated || response.Body.String() != "business payload" {
		t.Fatalf("status=%d body=%s; want 201 business payload", response.Code, response.Body.String())
	}
	if len(recorder.inputs) != 2 {
		t.Fatalf("audit events = %d, want intent + result", len(recorder.inputs))
	}
	intent, result := recorder.inputs[0], recorder.inputs[1]
	if intent.Metadata["phase"] != "intent" || intent.Result != "REQUESTED" {
		t.Fatalf("intent event phase=%v result=%q; want intent/REQUESTED", intent.Metadata["phase"], intent.Result)
	}
	if intent.EventID == "" || intent.EventID == result.EventID {
		t.Fatalf("intent event id = %q, result event id = %q; want distinct non-empty ids", intent.EventID, result.EventID)
	}
	if _, recorded := intent.Metadata["status_code"]; recorded {
		t.Fatal("intent event must not claim a status code before the business handler runs")
	}
	if result.Metadata["phase"] != "result" || result.Metadata["correlates_to"] != intent.EventID {
		t.Fatalf("result event phase=%v correlates_to=%v; want result/%s", result.Metadata["phase"], result.Metadata["correlates_to"], intent.EventID)
	}
	if result.Result != "SUCCESS" || result.Metadata["status_code"] != http.StatusCreated {
		t.Fatalf("result event result=%q status_code=%v; want SUCCESS/%d", result.Result, result.Metadata["status_code"], http.StatusCreated)
	}
	if len(recorder.spooled) != 0 {
		t.Fatalf("dead letters = %d, want 0 when both events are written", len(recorder.spooled))
	}
}

// BIZ-RUNTIME-11 步骤 2：结果事件同步重试后仍失败 → 落审计死信，并放行业务成功响应
// （意图事件已在库，合规成立；不再对已生效的操作谎报“未被确认”）。
func TestAuditTrailRequiredModeDeadLettersResultAndReleasesBusinessResponse(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	businessCalls := 0
	recorder := &auditRecorderScript{ingestErrs: []error{nil, errors.New("audit store unavailable"), errors.New("audit store unavailable")}}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		businessCalls++
		context.String(http.StatusCreated, "business payload")
	})

	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusCreated || response.Body.String() != "business payload" {
		t.Fatalf("status=%d body=%s; want 201 business payload after result spooled", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "AUDIT_WRITE_REJECTED") {
		t.Fatal("a written intent event must not produce AUDIT_WRITE_REJECTED")
	}
	if businessCalls != 1 {
		t.Fatalf("business calls = %d, want 1", businessCalls)
	}
	// 意图 1 次 + 结果 1 次 + 同步重试 1 次。
	if len(recorder.ingested) != 3 || len(recorder.spooled) != 1 {
		t.Fatalf("ingest calls = %d, spooled = %d; want 3/1", len(recorder.ingested), len(recorder.spooled))
	}
	intent, result := recorder.ingested[0], recorder.ingested[1]
	deadLetter := recorder.spooled[0]
	if deadLetter.EventID != result.EventID {
		t.Fatalf("dead letter event id = %q, want the failed result event %q", deadLetter.EventID, result.EventID)
	}
	if deadLetter.Metadata["phase"] != "result" || deadLetter.Metadata["correlates_to"] != intent.EventID {
		t.Fatalf("dead letter phase=%v correlates_to=%v; want result/%s", deadLetter.Metadata["phase"], deadLetter.Metadata["correlates_to"], intent.EventID)
	}
	if deadLetter.Result != "SUCCESS" || deadLetter.Metadata["status_code"] != http.StatusCreated {
		t.Fatalf("dead letter result=%q status_code=%v; want SUCCESS/%d", deadLetter.Result, deadLetter.Metadata["status_code"], http.StatusCreated)
	}
	if recorder.spoolTenant[0] != "tenant-1" {
		t.Fatalf("dead letter tenant = %q, want tenant-1", recorder.spoolTenant[0])
	}
}

// BIZ-RUNTIME-11：意图 + 结果 + 死信全不可写时才拒绝，且必须明确 operation_applied 语义，
// 绝不写成“未被确认”，也不引导对已生效写操作的无脑重试。
func TestAuditTrailRequiredModeRejectsWithOperationAppliedWhenResultAndDeadLetterFail(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	businessCalls := 0
	recorder := &auditRecorderScript{
		ingestErrs: []error{nil, errors.New("audit store unavailable"), errors.New("audit store unavailable")},
		spoolErrs:  []error{errors.New("audit dead letter unavailable")},
	}
	router := gin.New()
	router.Use(AuditTrail(recorder, slog.New(slog.NewTextHandler(io.Discard, nil))))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		businessCalls++
		context.String(http.StatusCreated, "business payload")
	})

	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if businessCalls != 1 {
		t.Fatalf("business calls = %d, want the handler to run before the result event", businessCalls)
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "AUDIT_RESULT_WRITE_REJECTED") {
		t.Fatalf("body = %s, want AUDIT_RESULT_WRITE_REJECTED", body)
	}
	if !strings.Contains(body, `"operation_applied":"true"`) {
		t.Fatalf("body = %s, want operation_applied=true (the business fact is committed)", body)
	}
	if !strings.Contains(body, `"next_action":"do_not_retry"`) {
		t.Fatalf("body = %s, want next_action=do_not_retry for an applied write", body)
	}
	if !strings.Contains(body, `"handler_status_code":"201"`) {
		t.Fatalf("body = %s, want handler_status_code=201", body)
	}
	if strings.Contains(body, "未被确认") {
		t.Fatalf("body = %s, must not claim the operation is unconfirmed once it was applied", body)
	}
	if strings.Contains(body, "business payload") {
		t.Fatal("rejected response must not leak the buffered business payload")
	}
}

// 非必开模式行为不变：每次请求仍只有一条终态通用轨迹事件，没有 phase/correlates_to，
// 也不走死信路径。
func TestAuditTrailNonRequiredModeWritesSingleEventWithoutPhase(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	recorder := &auditRecorderStub{}
	router := newAuditTrailTestRouter(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")
	if response.Code != http.StatusCreated || response.Body.String() != "business payload" {
		t.Fatalf("status=%d body=%s; want 201 business payload", response.Code, response.Body.String())
	}
	if len(recorder.inputs) != 1 {
		t.Fatalf("audit events = %d, want exactly one generic event in non-required mode", len(recorder.inputs))
	}
	event := recorder.inputs[0]
	if _, recorded := event.Metadata["phase"]; recorded {
		t.Fatalf("non-required mode event must not carry metadata.phase: %v", event.Metadata["phase"])
	}
	if _, recorded := event.Metadata["correlates_to"]; recorded {
		t.Fatalf("non-required mode event must not carry metadata.correlates_to: %v", event.Metadata["correlates_to"])
	}
	if event.Result != "SUCCESS" || event.Metadata["status_code"] != http.StatusCreated {
		t.Fatalf("event result=%q status_code=%v; want SUCCESS/%d", event.Result, event.Metadata["status_code"], http.StatusCreated)
	}
	if len(recorder.spooled) != 0 {
		t.Fatalf("dead letters = %d, want 0 in non-required mode", len(recorder.spooled))
	}
}

// SEC-D4b 平台侧必开判定：显式键可解析时用解析值，空值回退 APP_ENV=production，
// 显式值非法（如 "notabool"）按失败关闭返回 true——强制审计被配错不得静默关闭，
// 与 project_management / Settlement 的同类实现同语义。
func TestPlatformAuditRequiredFailsClosedOnInvalidExplicitValue(t *testing.T) {
	cases := []struct {
		name         string
		explicit     string
		appEnv       string
		wantRequired bool
	}{
		{name: "非法显式值按失败关闭", explicit: "notabool", appEnv: "dev", wantRequired: true},
		{name: "空值回退 production 判定", explicit: "", appEnv: "production", wantRequired: true},
		{name: "空值非 production 不要求必开", explicit: "", appEnv: "dev", wantRequired: false},
		{name: "显式 false 在 production 下关闭必开", explicit: "false", appEnv: "production", wantRequired: false},
		{name: "显式 true 在非 production 下要求必开", explicit: "true", appEnv: "dev", wantRequired: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("PLATFORM_AUDIT_REQUIRED", testCase.explicit)
			t.Setenv("APP_ENV", testCase.appEnv)
			if required := platformAuditRequired(); required != testCase.wantRequired {
				t.Fatalf("platformAuditRequired() = %v, want %v (PLATFORM_AUDIT_REQUIRED=%q, APP_ENV=%q)",
					required, testCase.wantRequired, testCase.explicit, testCase.appEnv)
			}
		})
	}
}

// 审计导出下载是“已流出失败”的唯一场景：响应体已直接流给客户端，意图事件已在库，
// 结果事件与死信都写不进去时既不能补写 503（会污染一个已完整送达的文件），也不能只留
// 一行 ERROR 日志——那种“审计结论缺失但业务已流出”的失败必须进入
// platform_audit_write_failures_total{stage="result_streamed"} 才可能被告警发现。
func TestAuditTrailCountsStreamedResultFailureOnExportDownload(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	payload := "zip-bytes-payload"
	registry := eventmetrics.NewRegistry()
	recorder := &auditRecorderScript{
		ingestErrs: []error{nil, errors.New("audit store unavailable"), errors.New("audit store unavailable")},
		spoolErrs:  []error{errors.New("audit dead letter unavailable")},
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(auditTrailHandler(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)),
		AuditSource{ApplicationCode: "platform", EnvironmentCode: "dev"}, &auditGate{events: registry}))
	router.GET("/api/v1/audit/export-jobs/:job_id/download", func(context *gin.Context) {
		_, _ = context.Writer.WriteString(payload)
	})

	response := newAuditTrailRequest(router, http.MethodGet, "/api/v1/audit/export-jobs/job-1/download")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (已流出的下载不得被补写 503); body = %s", response.Code, response.Body.String())
	}
	if response.Body.String() != payload {
		t.Fatalf("body = %q, want the intact download %q", response.Body.String(), payload)
	}
	if len(recorder.ingested) == 0 || recorder.ingested[0].Metadata["response_body"] != "not_captured" {
		t.Fatalf("intent metadata response_body = %v, want not_captured",
			recorder.ingested[0].Metadata["response_body"])
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResultStreamed); got != 1 {
		t.Fatalf("result_streamed stage count = %d, want 1 (已流出失败必须可观测)", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResult); got != 1 {
		t.Fatalf("result stage count = %d, want 1 (结果事件写入失败仍按 result 计)", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageDeadLetter); got != 1 {
		t.Fatalf("dead_letter stage count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageUnavailable); got != 0 {
		t.Fatalf("unavailable stage count = %d, want 0 (拒绝窗口不因下载例外被打开)", got)
	}
}

// 同一失败序列在非下载路由上必须保持 fail-closed：响应被 503 拒绝，且绝不产生
// result_streamed 计数——否则这个例外会被误解成“所有路由都可以放过审计结论缺失”。
func TestAuditTrailStreamedResultStageStaysLimitedToDownloadRoutes(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	registry := eventmetrics.NewRegistry()
	recorder := &auditRecorderScript{
		ingestErrs: []error{nil, errors.New("audit store unavailable"), errors.New("audit store unavailable")},
		spoolErrs:  []error{errors.New("audit dead letter unavailable")},
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(auditTrailHandler(recorder, slog.New(slog.NewTextHandler(io.Discard, nil)),
		AuditSource{ApplicationCode: "platform", EnvironmentCode: "dev"}, &auditGate{events: registry}))
	router.POST("/api/v1/dictionaries", func(context *gin.Context) {
		context.String(http.StatusCreated, "business payload")
	})

	response := newAuditTrailRequest(router, http.MethodPost, "/api/v1/dictionaries")

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "AUDIT_RESULT_WRITE_REJECTED") {
		t.Fatalf("status=%d body=%s, want 503 AUDIT_RESULT_WRITE_REJECTED", response.Code, response.Body.String())
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResultStreamed); got != 0 {
		t.Fatalf("result_streamed stage count = %d, want 0 (例外只适用于下载路由)", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageResult); got != 1 {
		t.Fatalf("result stage count = %d, want 1", got)
	}
	if got := registry.AuditWriteFailureCount(eventmetrics.AuditStageDeadLetter); got != 1 {
		t.Fatalf("dead_letter stage count = %d, want 1", got)
	}
}
