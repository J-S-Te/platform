package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// 阻塞类缺项在提交时必须映射为 409：响应体带稳定错误码与分项 missing
// （code/message/next_action），不能退化成 500 或伪装 202 受理。
func TestSubsystemAccessHandlerMapsMissingPrerequisitesToConflict(t *testing.T) {
	stub := &subsystemAccessServiceStub{
		preview: application.SubsystemAccessPreview{
			Decision: application.SubsystemAccessDecision{Action: application.SubsystemAccessActionUpdate, Reason: "已接入但配置与期望不一致"},
			Missing: []application.SubsystemAccessMissing{
				{Code: "LICENSE_MISSING", Message: "缺少商业授权", NextAction: "在「商业授权」完成初始化并导入签发产物"},
			},
			PreviewVersion: "pv-1",
		},
		submitErr: &application.SubsystemAccessPrerequisitesMissingError{Missing: []application.SubsystemAccessMissing{
			{Code: "LICENSE_MISSING", Message: "缺少商业授权", NextAction: "在「商业授权」完成初始化并导入签发产物"},
			{Code: "RUNTIME_APPROVAL_MISSING", Message: "缺少运行批准", NextAction: "生成批准候选并完成审核发布"},
		}},
	}
	handler := subsystemAccessHandlerFor(t, stub)
	principal := accessPrincipal(application.PermissionApplicationUpdate, application.PermissionApplicationEnvironmentUpdate,
		application.PermissionLoginTargetUpdate, application.PermissionOAuthClientDisable)
	response := httptest.NewRecorder()
	handler.Submit(response, subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/operations",
		`{"application_code":"settlement","environment":"dev","preview_version":"pv-1","idempotency_key":"idem-1"}`, &principal))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", response.Code, response.Body.String())
	}
	if stub.submitCalls != 1 {
		t.Fatalf("submit calls = %d, want 1 (handler must surface the service rejection)", stub.submitCalls)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`"code":"SUBSYSTEM_ACCESS_PREREQUISITES_MISSING"`,
		`"missing":[`,
		`"code":"LICENSE_MISSING"`,
		`"message":"缺少商业授权"`,
		`"next_action":"在「商业授权」完成初始化并导入签发产物"`,
		`"code":"RUNTIME_APPROVAL_MISSING"`,
		"下一步",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body missing %s: %s", expected, body)
		}
	}
}
