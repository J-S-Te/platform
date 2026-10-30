package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type accessRecoveryHTTPStub struct {
	subsystemAccessServiceStub
	calls   int
	seen    application.SubsystemAccessManualRecoveryInput
	preview application.SubsystemAccessManualRecoveryPreview
}

// PreviewManualRecovery 为HTTP权限测试提供不含敏感数据的用例替身。
// 参数：其余参数为已认证租户及URL任务标识。
// 返回值：预置事实，仅用于权限及公共DTO测试，不代表现场预检。
func (s *accessRecoveryHTTPStub) PreviewManualRecovery(context.Context, string, string) (application.SubsystemAccessManualRecoveryPreview, error) {
	s.calls++
	return s.preview, nil
}

// TestManualRecoveryHTTPPreviewKeepsPublicSchema 验证公共字段不改变旧回执序列化协议。
// 参数：t为具完整更新权限的HTTP边界测试上下文。
// 返回值：无，前端必须得到小写目标、版本及代次字段。
func TestManualRecoveryHTTPPreviewKeepsPublicSchema(t *testing.T) {
	s := &accessRecoveryHTTPStub{preview: application.SubsystemAccessManualRecoveryPreview{
		OperationID: "operation-1", OperationVersion: 5,
		Evidence: application.SubsystemAccessRecoveryEvidence{Receipt: application.SubsystemAccessDeploymentReceipt{
			ApplicationCode: "contract_management", Environment: "prod", Generation: 7,
			ManifestChecksum: strings.Repeat("a", 64)}, EvidenceDigest: strings.Repeat("b", 64)},
	}}
	h, err := NewSubsystemAccessHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	principal := accessPrincipal(application.SubsystemAccessRequiredPermissions(application.SubsystemAccessActionUpdate)...)
	request := subsystemAccessRequest(t, stdhttp.MethodGet, "/recovery-preview", "", &principal)
	response := httptest.NewRecorder()
	h.PreviewManualRecovery(response, request)
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || body.Data["application_code"] != "contract_management" ||
		body.Data["generation"] != float64(7) || body.Data["operation_version"] != float64(5) ||
		body.Data["Receipt"] != nil {
		t.Fatal("public recovery schema changed", response.Body.String())
	}
}

// AbandonManualRecovery 记录HTTP向用例传递的可信操作者及严格请求。
// 参数：input为处理器从主体与请求白名单组装的输入。
// 返回值：失败状态视图；不代表真实Agent或数据库验收。
func (s *accessRecoveryHTTPStub) AbandonManualRecovery(_ context.Context,
	input application.SubsystemAccessManualRecoveryInput) (application.SubsystemAccessOperationView, error) {
	s.calls++
	s.seen = input
	return application.SubsystemAccessOperationView{Operation: application.SubsystemAccessOperation{
		ID: input.OperationID, Status: application.SubsystemAccessStatusFailed,
		ErrorCode: application.AccessOperatorAbandoned}}, nil
}

// TestManualRecoveryHTTPRequiresFullUpdatePermissions 验证只读用户不能读取人工恢复事实。
// 参数：t为无外部访问的HTTP处理器测试上下文。
// 返回值：无，未认证401、权限不全403且不得触达恢复端口。
func TestManualRecoveryHTTPRequiresFullUpdatePermissions(t *testing.T) {
	s := &accessRecoveryHTTPStub{}
	h, err := NewSubsystemAccessHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := subsystemAccessRequest(t, stdhttp.MethodGet, "/recovery-preview", "", nil)
	response := httptest.NewRecorder()
	h.PreviewManualRecovery(response, request)
	if response.Code != 401 || s.calls != 0 {
		t.Fatal("anonymous recovery reached service")
	}
	principal := accessPrincipal()
	request = subsystemAccessRequest(t, stdhttp.MethodGet, "/recovery-preview", "", &principal)
	response = httptest.NewRecorder()
	h.PreviewManualRecovery(response, request)
	if response.Code != 403 || s.calls != 0 {
		t.Fatal("read-only recovery reached service")
	}
}

// TestManualRecoveryHTTPStrictBodyAndPrincipal 验证请求不能注入身份或尾随载荷。
// 参数：t为已具完整更新权限的HTTP边界测试上下文。
// 返回值：无，合法请求必须采用可信主体且保持失败语义。
func TestManualRecoveryHTTPStrictBodyAndPrincipal(t *testing.T) {
	s := &accessRecoveryHTTPStub{}
	h, err := NewSubsystemAccessHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	principal := accessPrincipal(application.SubsystemAccessRequiredPermissions(application.SubsystemAccessActionUpdate)...)
	body := `{"expected_version":5,"generation":7,"evidence_digest":"` + strings.Repeat("a", 64) + `","reason":"核对过原始报错、运行文件及容器，终止本次旧代次。"}`
	for _, payload := range []string{body + `{}`, strings.TrimSuffix(body, "}") + `,"operator_id":"attacker"}`} {
		request := subsystemAccessRequest(t, stdhttp.MethodPost, "/abandon", payload, &principal)
		request.SetPathValue("operation_id", "operation-1")
		response := httptest.NewRecorder()
		h.AbandonManualRecovery(response, request)
		if response.Code != 422 || s.calls != 0 {
			t.Fatal("unsafe recovery body accepted", response.Code)
		}
	}
	request := subsystemAccessRequest(t, stdhttp.MethodPost, "/abandon", body, &principal)
	request.SetPathValue("operation_id", "operation-1")
	response := httptest.NewRecorder()
	h.AbandonManualRecovery(response, request)
	if response.Code != 200 || s.seen.OperatorID != "operator-1" || s.seen.TenantID != "tenant-1" ||
		s.seen.OperationID != "operation-1" || !strings.Contains(response.Body.String(), `"status":"FAILED"`) {
		t.Fatal("trusted recovery input or failure response lost", response.Code)
	}
}
