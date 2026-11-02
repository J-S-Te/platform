package filetaskhttp

import (
	"database/sql/driver"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// BIZ-RUNTIME-10：幂等键命中终态失败会话（FAILED/REJECTED）时，CreateSession 不再以
// 200 回放失败会话，而是 409 FILE_UPLOAD_SESSION_FAILED，要求客户端换新的幂等键重生。
// 非失败状态（CREATED/UPLOADING/READY）与不同幂等键保持既有语义。

// sessionRowColumns 与 uploadV2Session 的字段保持一致，供 fakeRows 按列名扫描。
func sessionRowColumns() []string {
	return []string{
		"id", "tenant_id", "application_id", "application_code", "authenticated_client_id",
		"actor_user_id", "file_id", "version_id", "namespace", "purpose", "policy_version",
		"original_name", "declared_media_type", "classification", "expected_size",
		"expected_sha256", "resource_type", "resource_id", "binding_type", "display_name",
		"idempotency_key", "upload_mode", "ticket_hash", "ticket_expires_at", "ticket_used_at",
		"retention_until", "status", "failure_code", "created_at", "updated_at", "completed_at",
	}
}

// idempotentSessionRow 构造与 createSessionRequest 相同请求指纹的既有会话行，
// 仅状态可调；失败状态额外带 failure_code。
func idempotentSessionRow(status string) []driver.Value {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var failureCode driver.Value
	switch status {
	case "FAILED":
		failureCode = "UPLOAD_FAILED"
	case "REJECTED":
		failureCode = "STATIC_VALIDATION_FAILED"
	}
	return []driver.Value{
		"01K10C00000000000000000EX1", "tenant-1", "app-1", "contract_management", "machine-client-1",
		nil, "01K10C00000000000000000FL1", "01K10C00000000000000000VR1", "contract", "template", v2PolicyVersion,
		"template.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "INTERNAL", int64(1024),
		[]byte(strings.Repeat("\xaa", 32)), "CONTRACT", "contract-1", "PRIMARY", "",
		"idem-actor-1", "PROXY", nil, nil, nil,
		nil, status, failureCode, now, now, nil,
	}
}

// newIdempotencyProbeHandler 用脚本驱动返回既有会话行（statuses 为空表示查不到），
// 并复用既有请求构造器与文件服务桩。
func newIdempotencyProbeHandler(t *testing.T, statuses []string) (*UploadV2Handler, *auditScript) {
	t.Helper()
	script := &auditScript{rows: func(query string) (driver.Rows, error) {
		if !strings.Contains(query, "file_upload_v2_session") {
			return nil, errors.New("unexpected query: " + query)
		}
		if len(statuses) == 0 {
			return &fakeRows{columns: sessionRowColumns()}, nil
		}
		values := make([][]driver.Value, 0, len(statuses))
		for _, status := range statuses {
			values = append(values, idempotentSessionRow(status))
		}
		return &fakeRows{columns: sessionRowColumns(), values: values}, nil
	}}
	handler, err := NewUploadV2Handler(newAuditTestDB(t, script), &recordingV2FileService{}, func(time.Time) (string, error) {
		return "01K10C00000000000000000NEW", nil
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewUploadV2Handler: %v", err)
	}
	return handler, script
}

// sessionInsertCount 只统计上传会话落库，排除审计等其它 INSERT。
func sessionInsertCount(script *auditScript) int {
	count := 0
	for _, item := range script.inserts() {
		if strings.Contains(item.query, "file_upload_v2_session") {
			count++
		}
	}
	return count
}

func TestCreateSessionIdempotentHitOnFailedSessionConflicts(t *testing.T) {
	for _, status := range []string{"FAILED", "REJECTED"} {
		t.Run(status, func(t *testing.T) {
			handler, script := newIdempotencyProbeHandler(t, []string{status})
			response := httptest.NewRecorder()

			handler.CreateSession(response, createSessionRequest(authenticatedHumanPrincipal(), ""))

			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409, body = %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "FILE_UPLOAD_SESSION_FAILED") {
				t.Fatalf("error code = %s, want FILE_UPLOAD_SESSION_FAILED", response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "新的 Idempotency-Key") {
				t.Fatalf("错误提示必须引导使用新幂等键: %s", response.Body.String())
			}
			// 失败会话不得以 data 负载回放，避免客户端误判为可继续。
			for _, leaked := range []string{"upload_id", "01K10C00000000000000000EX1", "01K10C00000000000000000FL1", "sha256", "actor_user_id", "machine-client-1", "idem-actor-1"} {
				if strings.Contains(response.Body.String(), leaked) {
					t.Fatalf("409 响应不得回放会话字段 %q: %s", leaked, response.Body.String())
				}
			}
			if count := sessionInsertCount(script); count != 0 {
				t.Fatalf("终态失败命中时不得创建新会话，insert count = %d", count)
			}
		})
	}
}

func TestCreateSessionIdempotentHitOnActiveSessionReplays(t *testing.T) {
	for _, status := range []string{"CREATED", "UPLOADING", "READY"} {
		t.Run(status, func(t *testing.T) {
			handler, script := newIdempotencyProbeHandler(t, []string{status})
			response := httptest.NewRecorder()

			handler.CreateSession(response, createSessionRequest(authenticatedHumanPrincipal(), ""))

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body = %s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			if !strings.Contains(body, "01K10C00000000000000000EX1") || !strings.Contains(body, `"status":"`+status+`"`) {
				t.Fatalf("幂等回放必须返回原会话: %s", body)
			}
			if count := sessionInsertCount(script); count != 0 {
				t.Fatalf("幂等命中不得创建新会话，insert count = %d", count)
			}
		})
	}
}

func TestCreateSessionNewIdempotencyKeyCreatesSession(t *testing.T) {
	handler, script := newIdempotencyProbeHandler(t, nil)
	response := httptest.NewRecorder()

	handler.CreateSession(response, createSessionRequest(authenticatedHumanPrincipal(), ""))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", response.Code, response.Body.String())
	}
	if count := sessionInsertCount(script); count != 1 {
		t.Fatalf("未命中幂等键时必须创建新会话，insert count = %d", count)
	}
}

// 只有终态失败状态视为不可重放；其余状态（含空值）保持既有回放语义。
func TestTerminalUploadSessionFailureOnlyCoversFailedStates(t *testing.T) {
	for _, item := range []struct {
		status string
		want   bool
	}{
		{"FAILED", true}, {"failed", true}, {"REJECTED", true}, {" REJECTED ", true},
		{"CREATED", false}, {"UPLOADING", false}, {"READY", false}, {"", false},
	} {
		if got := terminalUploadSessionFailure(item.status); got != item.want {
			t.Fatalf("terminalUploadSessionFailure(%q)=%v want=%v", item.status, got, item.want)
		}
	}
}
