package identityhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type feedbackDependencies struct {
	application.ManagementRepository
	application.MobileProtection
	application.IDGenerator
}

// Now 提供资料校验使用的固定时刻，不查询真实运行环境。
// 参数：无。
// 返回值：隔离用例的当前时间。
func (feedbackDependencies) Now() time.Time {
	return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
}

// TestManagementExplainsMembershipRequirement 验证缺任职仍保持冲突契约但说明处理入口。
// 参数：t 为隔离 HTTP 测试上下文。
// 返回值：无；只剩通用冲突或错误码变化时失败。
func TestManagementExplainsMembershipRequirement(t *testing.T) {
	handler := bootstrapAdminTestHandler(t)
	response := httptest.NewRecorder()
	handler.writeError(response, httptest.NewRequest(http.MethodPost, "/api/v1/users", nil),
		application.ErrMembershipRequired)
	envelope := decodeErrorEnvelope(t, response)
	if response.Code != 409 || envelope.Code != "IAM_CONFLICT" ||
		!strings.Contains(envelope.Message, "任职") || !strings.Contains(envelope.Message, "入职") {
		t.Fatalf("缺任职没有处理指引：%d，%+v", response.Code, envelope)
	}
}

// TestManagementRejectsJSONWithSafeReason 验证真实管理入口公开协议分类而不回显内容。
// 参数：t 为测试上下文，输入均是构造的无业务数据载荷。
// 返回值：无；原因丢失、严格解码失效或诊断泄漏时失败。
func TestManagementRejectsJSONWithSafeReason(t *testing.T) {
	handler := bootstrapAdminTestHandler(t)
	for _, test := range []struct{ body, reason string }{
		{`{"private_secret_fixture":"hidden-value"}`, "unknown_field"},
		{`{"email":123}`, "invalid_type"},
		{`{"display_name":`, "invalid_json"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(test.body))
		request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
			Tenant: authctx.ReferenceName{ID: "t"}, User: authctx.ReferenceName{ID: "operator"},
		}))
		response := httptest.NewRecorder()
		handler.CreateUser(response, request)
		var envelope struct {
			Code    string            `json:"code"`
			Details map[string]string `json:"details"`
		}
		if response.Code != 422 || json.Unmarshal(response.Body.Bytes(), &envelope) != nil ||
			envelope.Code != "PLATFORM_VALIDATION_ERROR" || envelope.Details["reason"] != test.reason {
			t.Errorf("协议校验原因丢失：%d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "private_secret_fixture") ||
			strings.Contains(response.Body.String(), "hidden-value") {
			t.Fatal("回执不应包含未知字段或输入值")
		}
	}
}

// TestManagementFieldFeedbackCrossesApplicationAndHTTP 验证真实应用校验到管理回执的字段接线。
// 参数：t 为隔离契约测试上下文，仓储与加密均不得被非法输入触发。
// 返回值：无；字段丢失、错误码变化或输入值泄漏时失败。
func TestManagementFieldFeedbackCrossesApplicationAndHTTP(t *testing.T) {
	dependencies := feedbackDependencies{}
	service, err := application.NewManagementService(
		dependencies, dependencies, dependencies, dependencies,
	)
	if err != nil {
		t.Fatal(err)
	}
	email := "bad-private-email"
	_, failure := service.UpdateUser(context.Background(), application.UserUpdateInput{
		TenantID: "t", OperatorID: "operator", UserID: "u", Version: 1,
		DisplayName: "隔离测试人员", Email: &email,
	})
	handler := bootstrapAdminTestHandler(t)
	response := httptest.NewRecorder()
	handler.writeError(response, httptest.NewRequest(http.MethodPatch, "/api/v1/users/u", nil), failure)
	envelope := decodeErrorEnvelope(t, response)
	if response.Code != 422 || envelope.Code != "PLATFORM_VALIDATION_ERROR" ||
		!strings.Contains(envelope.Message, "邮箱格式") {
		t.Fatalf("字段回执不准确：%d %+v", response.Code, envelope)
	}
	issues := envelope.Details.(map[string]any)["issues"].([]any)
	if issues[0].(map[string]any)["field"] != "email" || strings.Contains(response.Body.String(), email) {
		t.Fatalf("字段定位错误或输入泄漏：%s", response.Body.String())
	}
}

// TestManagementDoesNotExposeArbitraryValidationCause 验证普通包装异常不能伪装为安全业务原因。
// 参数：t 为隔离 HTTP 测试上下文。
// 返回值：无；底层诊断进入公开消息或详情时失败。
func TestManagementDoesNotExposeArbitraryValidationCause(t *testing.T) {
	response := httptest.NewRecorder()
	bootstrapAdminTestHandler(t).writeError(response,
		httptest.NewRequest(http.MethodPatch, "/api/v1/users/u", nil),
		fmt.Errorf("%w: SQLSTATE private-diagnostic", application.ErrValidation))
	envelope := decodeErrorEnvelope(t, response)
	if response.Code != 422 || envelope.Details != nil ||
		strings.Contains(response.Body.String(), "private-diagnostic") {
		t.Fatalf("未标记的异常不应成为业务提示：%s", response.Body.String())
	}
}
