package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

// 安全（SEC-B3+B9）负向测试：断言 HTTP details 与收件箱通知都不携带错误原文——
// 特别是 SQL 关键词、MySQL 错误号与文件路径只能进结构化日志。

const sqlLeakCause = "Error 1064 (42000): You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near 'FROM oauth_token_family' at line 1 (/var/lib/mysql/platform-db.sql)"

func TestWriteKeycloakObservationBlockedExposesOnlyStableReasonCode(t *testing.T) {
	t.Parallel()

	cause := errors.New(sqlLeakCause)
	code := keycloakObservationReasonCode(cause)
	if code != "OBSERVATION_STATE_UNAVAILABLE" {
		t.Fatalf("reason code = %q, want OBSERVATION_STATE_UNAVAILABLE", code)
	}

	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-environment", nil)
	request = request.WithContext(requestctx.WithRequestID(request.Context(), "01KZRME97X5XBWB3H1E74KZTSX"))
	response := httptest.NewRecorder()
	writeKeycloakObservationBlocked(response, request, code)

	if response.Code != stdhttp.StatusConflict {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, forbidden := range []string{"SQL syntax", "1064", "MySQL", "oauth_token_family", "/var/lib/mysql", "\"reason\""} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("details 泄漏内部细节 %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, "OBSERVATION_STATE_UNAVAILABLE") || !strings.Contains(body, "reason_code") {
		t.Fatalf("details 缺少稳定 reason_code: %s", body)
	}
}

func TestKeycloakObservationReasonCodeClassifiesKnownGates(t *testing.T) {
	t.Parallel()
	for message, want := range map[string]string{
		"Keycloak observation window has not completed":  "OBSERVATION_WINDOW_NOT_COMPLETED",
		"Keycloak observation has not started":           "OBSERVATION_NOT_STARTED",
		"Keycloak observation already exists":            "OBSERVATION_ALREADY_EXISTS",
		"Keycloak observation duration must be positive": "OBSERVATION_INVALID_DURATION",
	} {
		if got := keycloakObservationReasonCode(errors.New(message)); got != want {
			t.Fatalf("code for %q = %q, want %q", message, got, want)
		}
	}
}

func TestProvisioningWriteErrorDetailsExcludeRawCause(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"https://platform.example.com", slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	cause := fmt.Errorf("%w: %s", application.ErrSubsystemProvisioningUnavailable, sqlLeakCause)
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-onboard", nil)
	request = request.WithContext(requestctx.WithRequestID(request.Context(), "01KZRME97X5XBWB3H1E74KZTSX"))
	response := httptest.NewRecorder()

	handler.writeError(response, request, cause)

	if response.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	details, ok := body["details"].(map[string]any)
	if !ok {
		t.Fatalf("details 缺失: %s", response.Body.String())
	}
	if details["next_action"] == nil || details["next_action"] == "" || details["error_code"] == nil || details["error_code"] == "" {
		t.Fatalf("details = %#v, want next_action + error_code", details)
	}
	if _, leaked := details["detail"]; leaked {
		t.Fatalf("details 不得再携带原始 detail 字段: %#v", details)
	}
	for _, forbidden := range []string{"SQL syntax", "1064", "MySQL", "oauth_token_family", "/var/lib/mysql"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("details 泄漏内部细节 %q: %s", forbidden, response.Body.String())
		}
	}
	// cause 仍必须进结构化日志，保持可排障。
	if !strings.Contains(logs.String(), "SQL syntax") || !strings.Contains(logs.String(), "subsystem automatic provisioning failed") {
		t.Fatalf("结构化日志必须保留原始 cause: %s", logs.String())
	}
}

func TestSubsystemLifecycleFailureDetailStripsSqlAndPaths(t *testing.T) {
	t.Parallel()

	sqlDetail := subsystemLifecycleFailureDetail(errors.New(sqlLeakCause))
	if strings.Contains(sqlDetail, "SQL syntax") || strings.Contains(sqlDetail, "oauth_token_family") || strings.Contains(sqlDetail, "MySQL") {
		t.Fatalf("通知摘要泄漏 SQL 细节: %q", sqlDetail)
	}
	if !strings.Contains(sqlDetail, "PROVISIONING_") {
		t.Fatalf("通知摘要缺少稳定错误码: %q", sqlDetail)
	}

	pathDetail := subsystemLifecycleFailureDetail(errors.New("deployment helper is unavailable (/opt/platform/subsystem-provisioner/bin/agent)"))
	if strings.Contains(pathDetail, "/opt/platform") {
		t.Fatalf("通知摘要泄漏文件路径: %q", pathDetail)
	}
	if !strings.HasPrefix(pathDetail, "PROVISIONING_AGENT_UNREACHABLE") {
		t.Fatalf("通知摘要前缀 = %q", pathDetail)
	}
}

func TestNotifySubsystemLifecycleSanitizesRawDetail(t *testing.T) {
	t.Parallel()
	sink := &recordingNotificationSink{}
	handler, err := NewSubsystemOnboardingHandlerWithNotifications(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"https://platform.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)), sink,
	)
	if err != nil {
		t.Fatal(err)
	}

	// 防御纵深：即使调用方误传错误原文，入库前也必须被脱敏。
	handler.notifySubsystemLifecycle(context.Background(), "tenant-1", "user-1", "合同管理系统", "contract_management", "prod", false, sqlLeakCause)
	handler.notifySubsystemLifecycle(context.Background(), "tenant-1", "user-1", "合同管理系统", "contract_management", "prod", false, "PROVISIONING_FAILED: SELECT * FROM cfg_item WHERE id=1 failed at /etc/platform/agent.conf")

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(sink.calls))
	}
	for i, call := range sink.calls {
		upper := strings.ToUpper(call.Detail)
		for _, forbidden := range []string{"SELECT", "FROM CFG", "SQL SYNTAX", "OAUTH_TOKEN_FAMILY", "/VAR/LIB", "/ETC/PLATFORM"} {
			if strings.Contains(upper, forbidden) {
				t.Fatalf("call %d 通知详情泄漏 %q: %q", i, forbidden, call.Detail)
			}
		}
		if call.Detail == "" {
			t.Fatalf("call %d 通知详情为空", i)
		}
	}
}
