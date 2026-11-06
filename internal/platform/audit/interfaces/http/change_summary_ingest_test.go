package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/audit/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

const (
	auditIngestTestRequestID       = "req-audit-ingest-1"
	auditIngestTestTraceID         = "0123456789abcdef0123456789abcdef"
	auditIngestTestCorrelationID   = "corr-audit-ingest-1"
	auditIngestTestApplicationCode = "customer_and_opportunity"
	auditIngestTestEnvironmentCode = "dev"
)

// auditIngestPayload 返回可通过传输三元组校验的应用审计摄取载荷；调用方只改需要
// 覆盖的字段，其余保持契约最小值。
func auditIngestPayload() map[string]any {
	return map[string]any{
		"event_id":         "evt-change-summary-1",
		"application_code": auditIngestTestApplicationCode,
		"environment_code": auditIngestTestEnvironmentCode,
		"actor_type":       "USER",
		"occurred_at":      "2026-11-02T10:00:00Z",
		"action":           "PATCH /api/v1/iam/accounts/acct-1",
		"resource_type":    "IAM_ACCOUNT",
		"result":           "SUCCESS",
		"request_id":       auditIngestTestRequestID,
		"trace_id":         auditIngestTestTraceID,
		"correlation_id":   auditIngestTestCorrelationID,
	}
}

// auditIngestRequest 把载荷编码成请求，并注入应用主体与可信传输三元组。
func auditIngestRequest(t *testing.T, payload map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal ingest payload: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/audit/events", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	ctx := appctx.WithPrincipal(request.Context(), appctx.Principal{
		OAuthClientID:   "oauth-client-1",
		ClientID:        "customer_and_opportunity-dev-audit-publisher",
		TenantID:        "tenant-1",
		ApplicationID:   "application-1",
		ApplicationCode: auditIngestTestApplicationCode,
		EnvironmentID:   "environment-1",
		EnvironmentCode: auditIngestTestEnvironmentCode,
		Scopes:          map[string]struct{}{"audit.ingest": {}},
	})
	ctx = requestctx.WithRequestID(ctx, auditIngestTestRequestID)
	ctx = requestctx.WithTraceID(ctx, auditIngestTestTraceID)
	ctx = requestctx.WithCorrelationID(ctx, auditIngestTestCorrelationID)
	return request.WithContext(ctx)
}

// change_summary 超条数必须在摄取边界返回 422（复用统一校验错误码），而不是静默截断
// 后照收，也不是 500。
func TestIngestRejectsChangeSummaryExceedingEntryLimit(t *testing.T) {
	handler, _, _ := newExportDownloadHandler(t, domain.ExportFile{})
	changes := make([]domain.FieldChange, application.MaxChangeSummaryEntries+1)
	for index := range changes {
		changes[index] = domain.FieldChange{Field: "status", Before: "ACTIVE", After: "DISABLED"}
	}
	payload := auditIngestPayload()
	payload["change_summary"] = changes
	response := httptest.NewRecorder()

	handler.Ingest(response, auditIngestRequest(t, payload))

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", response.Code, response.Body.String())
	}
	if code := auditIngestErrorCode(t, response); code != "PLATFORM_VALIDATION_ERROR" {
		t.Fatalf("error code = %q, want PLATFORM_VALIDATION_ERROR", code)
	}
}

// change_summary 单值超长同样必须 422。
func TestIngestRejectsChangeSummaryOversizedValue(t *testing.T) {
	handler, _, _ := newExportDownloadHandler(t, domain.ExportFile{})
	payload := auditIngestPayload()
	payload["change_summary"] = []domain.FieldChange{{
		Field:  "display_name",
		Before: "旧名称",
		After:  strings.Repeat("长", application.MaxChangeSummaryValueRunes+1),
	}}
	response := httptest.NewRecorder()

	handler.Ingest(response, auditIngestRequest(t, payload))

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", response.Code, response.Body.String())
	}
}

// metadata 数组内出现凭据键时必须整条拒绝（旧实现只递归 map，数组是绕过口）。
func TestIngestRejectsSensitiveMetadataInsideArray(t *testing.T) {
	handler, _, _ := newExportDownloadHandler(t, domain.ExportFile{})
	payload := auditIngestPayload()
	payload["metadata"] = map[string]any{"items": []any{map[string]any{"password": "leaked"}}}
	response := httptest.NewRecorder()

	handler.Ingest(response, auditIngestRequest(t, payload))

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", response.Code, response.Body.String())
	}
}

// 合规载荷必须仍然被接收：本次加固不允许把正常事件一起挡掉。
func TestIngestAcceptsBoundedChangeSummary(t *testing.T) {
	handler, _, _ := newExportDownloadHandler(t, domain.ExportFile{})
	payload := auditIngestPayload()
	payload["change_summary"] = []domain.FieldChange{
		{Field: "display_name", Before: "张三", After: "李四"},
		{Field: "password", Before: "plain-secret", After: "next-plain-secret"},
	}
	response := httptest.NewRecorder()

	handler.Ingest(response, auditIngestRequest(t, payload))

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", response.Code, response.Body.String())
	}
}

func auditIngestErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode ingest response: %v; body=%s", err, response.Body.String())
	}
	return payload.Code
}
