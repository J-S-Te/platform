package filetaskhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// portalLocalWriter 构建仅属于门户应用的机器上传身份。
// 参数：无。
// 返回值：合成主体，不签发或使用真实凭据。
func portalLocalWriter() authctx.Principal {
	return authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"},
		Account:   authctx.ReferenceName{ID: "portal-app", Code: "customer_portal"},
		SessionID: "customer_portal-test-file-gateway"}
}

// portalLocalUploadBody 构造具有真实业务绑定含义的PDF会话声明。
// 参数：无。
// 返回值：可供边界用例单独修改的请求字段。
func portalLocalUploadBody() map[string]any {
	return map[string]any{"purpose": "portal.local_report", "original_name": "local-report.pdf",
		"media_type": "application/pdf", "size_bytes": 20 << 20, "sha256": strings.Repeat("a", 64),
		"resource_type": "PORTAL_REPORT_REQUEST", "resource_id": "12", "binding_type": "REPORT",
		"idempotency_key": "portal-local-report-1"}
}

// TestPortalLocalUploadRequiresExactMachineAndRequest 验证新用途不继承门户别名或任意资源绑定。
// 参数：t为测试上下文，SQL使用现有本地记录驱动。
// 返回值：无；成功必须创建会话，拒绝不得落库。
func TestPortalLocalUploadRequiresExactMachineAndRequest(t *testing.T) {
	cases := []struct {
		name   string
		change func(*authctx.Principal, map[string]any)
		status int
	}{
		{"valid", func(*authctx.Principal, map[string]any) {}, http.StatusCreated},
		{"other_app", func(p *authctx.Principal, _ map[string]any) { p.Account.Code = "project_management" }, http.StatusForbidden},
		{"alias", func(p *authctx.Principal, _ map[string]any) { p.Account.Code = "portal" }, http.StatusForbidden},
		{"human", func(p *authctx.Principal, _ map[string]any) { p.User.ID = "user" }, http.StatusForbidden},
		{"wrong_resource", func(_ *authctx.Principal, b map[string]any) { b["resource_type"] = "PROJECT" }, http.StatusUnprocessableEntity},
		{"wrong_binding", func(_ *authctx.Principal, b map[string]any) { b["binding_type"] = "ATTACHMENT" }, http.StatusUnprocessableEntity},
		{"zero", func(_ *authctx.Principal, b map[string]any) { b["resource_id"] = "0" }, http.StatusUnprocessableEntity},
		{"leading_zero", func(_ *authctx.Principal, b map[string]any) { b["resource_id"] = "012" }, http.StatusUnprocessableEntity},
		{"signed", func(_ *authctx.Principal, b map[string]any) { b["resource_id"] = "+12" }, http.StatusUnprocessableEntity},
		{"overflow", func(_ *authctx.Principal, b map[string]any) { b["resource_id"] = "18446744073709551616" }, http.StatusUnprocessableEntity},
		{"mime", func(_ *authctx.Principal, b map[string]any) { b["media_type"] = "image/png" }, http.StatusUnprocessableEntity},
		{"too_large", func(_ *authctx.Principal, b map[string]any) { b["size_bytes"] = (20 << 20) + 1 }, http.StatusUnprocessableEntity},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			handler, script := newActorProbeHandler(t)
			p, body := portalLocalWriter(), portalLocalUploadBody()
			item.change(&p, body)
			encoded, _ := json.Marshal(body)
			request := httptest.NewRequest(http.MethodPost, "/api/v2/upload-sessions", strings.NewReader(string(encoded)))
			request = request.WithContext(authctx.WithPrincipal(request.Context(), p))
			response := httptest.NewRecorder()
			handler.CreateSession(response, request)
			if response.Code != item.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, item.status, response.Body.String())
			}
			inserts := script.insertInto("file_upload_v2_session")
			if (len(inserts) == 1) != (item.status == http.StatusCreated) {
				t.Fatalf("unexpected session persistence: %+v", inserts)
			}
		})
	}
}

// TestPortalLocalV1RequiresV2 验证新来源不能借旧上传路径避开会话与业务绑定。
// 参数：t为测试上下文，v1使用既有正式handler和最小文件服务替身。
// 返回值：无；v1必须拒绝且不得调用文件持久化。
func TestPortalLocalV1RequiresV2(t *testing.T) {
	assertPortalLocalV1Rejected(t, "application/pdf")
}

// assertPortalLocalV1Rejected 实际调用v1 handler锁定新用途不能降级上传。
// 参数：t为测试上下文，media为当前矩阵的媒体类型。
// 返回值：无；拒绝状态和未进入文件服务必须同时成立。
func assertPortalLocalV1Rejected(t *testing.T, media string) {
	files := &stubV1FileService{file: v1PolicyReadyFile()}
	handler := newV1PolicyHandler(t, files)
	request := v1PolicyUploadRequest(t, "customer_portal", "portal.local_report",
		"local-report.pdf", media, []byte("payload"))
	p := portalLocalWriter()
	p.Account.ID = "app-1"
	request = request.WithContext(authctx.WithPrincipal(request.Context(), p))
	response := httptest.NewRecorder()
	handler.Upload(response, request)
	if response.Code != http.StatusForbidden || files.uploaded.ApplicationID != "" {
		t.Fatalf("v1 bypassed strict v2 source: status=%d input=%+v", response.Code, files.uploaded)
	}
}
