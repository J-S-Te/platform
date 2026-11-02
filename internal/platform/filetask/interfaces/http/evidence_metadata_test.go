package filetaskhttp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/filetask/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// evidenceMetadataStub 记录处理器传给服务层的访问上下文，并回放预设结果。
type evidenceMetadataStub struct {
	access   []application.EvidenceAccess
	fileIDs  []string
	metadata application.EvidenceMetadata
	err      error
}

func (stub *evidenceMetadataStub) StatEvidence(_ context.Context, access application.EvidenceAccess, fileID string) (application.EvidenceMetadata, error) {
	stub.access = append(stub.access, access)
	stub.fileIDs = append(stub.fileIDs, fileID)
	if stub.err != nil {
		return application.EvidenceMetadata{}, stub.err
	}
	return stub.metadata, nil
}

func evidenceStatRequest(principal *authctx.Principal) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/file-gateway/api/v1/files/file-1/evidence-metadata?resource_type=PROJECT_SERVICE_ITEM&resource_id=SI-EVIDENCE-1", nil)
	request.SetPathValue("file_id", "file-1")
	if principal == nil {
		return request
	}
	return request.WithContext(authctx.WithPrincipal(request.Context(), *principal))
}

func machineEvidencePrincipal() *authctx.Principal {
	return &authctx.Principal{
		SessionID:       "project_management-prod-file-gateway-writer",
		Tenant:          authctx.ReferenceName{ID: "tenant-1"},
		Account:         authctx.ReferenceName{ID: "app-project", Code: "project_management"},
		PermissionCodes: []string{"platform:file:download"},
	}
}

func newEvidenceTestHandler(t *testing.T, stub *evidenceMetadataStub) *EvidenceHandler {
	t.Helper()
	handler, err := NewEvidenceHandler(stub, slog.Default())
	if err != nil {
		t.Fatalf("NewEvidenceHandler: %v", err)
	}
	return handler
}

// TestEvidenceMetadataRequiresVerifiedApplicationIdentity 断言未认证请求不会进入服务层。
func TestEvidenceMetadataRequiresVerifiedApplicationIdentity(t *testing.T) {
	stub := &evidenceMetadataStub{}
	handler := newEvidenceTestHandler(t, stub)
	response := httptest.NewRecorder()
	handler.StatFile(response, evidenceStatRequest(nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401 body=%s", response.Code, response.Body.String())
	}
	if len(stub.access) != 0 {
		t.Fatalf("unauthenticated request reached the service layer")
	}
}

// TestEvidenceMetadataReturnsServerDerivedMetadata 断言成功响应只包含服务端核验到的
// 元数据（含真实 SHA-256/大小/绑定标记），且访问上下文取自已验签主体。
func TestEvidenceMetadataReturnsServerDerivedMetadata(t *testing.T) {
	stub := &evidenceMetadataStub{metadata: application.EvidenceMetadata{
		FileID: "file-1", TenantID: "tenant-1", ApplicationID: "app-project", Status: "READY",
		MediaType: "image/png", OriginalName: "field.png",
		SHA256: strings.Repeat("a", 64), SizeBytes: 128, BindingActive: true,
		CreatedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
	}}
	handler := newEvidenceTestHandler(t, stub)
	response := httptest.NewRecorder()
	handler.StatFile(response, evidenceStatRequest(machineEvidencePrincipal()))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if len(stub.access) != 1 {
		t.Fatalf("service calls = %d", len(stub.access))
	}
	if passed := stub.access[0]; passed.TenantID != "tenant-1" || passed.ApplicationID != "app-project" {
		t.Fatalf("access identity = %+v", passed)
	}
	if len(stub.access[0].PermissionCodes) != 1 || stub.access[0].PermissionCodes[0] != "platform:file:download" {
		t.Fatalf("permission codes = %+v", stub.access[0].PermissionCodes)
	}
	if stub.fileIDs[0] != "file-1" {
		t.Fatalf("file id = %q", stub.fileIDs[0])
	}
	// 绑定校验必须精确到业务对象：handler 把查询参数原样交给服务层，而不是只传应用身份。
	if passed := stub.access[0]; passed.ResourceType != "PROJECT_SERVICE_ITEM" || passed.ResourceID != "SI-EVIDENCE-1" {
		t.Fatalf("resource binding = %q/%q", passed.ResourceType, passed.ResourceID)
	}
	var envelope struct {
		Code string `json:"code"`
		Data struct {
			FileID        string `json:"file_id"`
			TenantID      string `json:"tenant_id"`
			ApplicationID string `json:"application_id"`
			Status        string `json:"status"`
			SHA256        string `json:"sha256"`
			SizeBytes     uint64 `json:"size_bytes"`
			BindingActive bool   `json:"binding_active"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Code != "OK" || envelope.Data.FileID != "file-1" || envelope.Data.TenantID != "tenant-1" ||
		envelope.Data.ApplicationID != "app-project" || envelope.Data.Status != "READY" ||
		envelope.Data.SHA256 != strings.Repeat("a", 64) || envelope.Data.SizeBytes != 128 || !envelope.Data.BindingActive {
		t.Fatalf("envelope = %s", response.Body.String())
	}
	// 元数据响应不得包含文件内容或存储路径。
	if strings.Contains(response.Body.String(), "/content") || strings.Contains(response.Body.String(), "storage_relative_path") {
		t.Fatalf("response leaks storage location: %s", response.Body.String())
	}
}

// TestEvidenceMetadataMapsServiceFailures 断言服务层拒绝被映射为稳定的 HTTP 状态：
// 跨租户/跨应用/不存在统一 404，未绑定 409，权限缺失 403，输入不合法 422，其它 500。
func TestEvidenceMetadataMapsServiceFailures(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{name: "not found or cross tenant", err: application.ErrNotFound, status: http.StatusNotFound},
		{name: "no active binding", err: application.ErrConflict, status: http.StatusConflict},
		{name: "missing scope", err: application.ErrForbidden, status: http.StatusForbidden},
		{name: "invalid input", err: application.ErrValidation, status: http.StatusUnprocessableEntity},
		{name: "storage failure", err: errors.New("database is unavailable"), status: http.StatusInternalServerError},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := &evidenceMetadataStub{err: testCase.err}
			handler := newEvidenceTestHandler(t, stub)
			response := httptest.NewRecorder()
			handler.StatFile(response, evidenceStatRequest(machineEvidencePrincipal()))
			if response.Code != testCase.status {
				t.Fatalf("status = %d want %d body=%s", response.Code, testCase.status, response.Body.String())
			}
		})
	}
}

// TestEvidenceMetadataRejectsMissingApplicationIdentity 断言缺少应用身份的主体
// （非 filetask 机器身份）不会被当作可核验调用方。
func TestEvidenceMetadataRejectsMissingApplicationIdentity(t *testing.T) {
	stub := &evidenceMetadataStub{}
	handler := newEvidenceTestHandler(t, stub)
	principal := authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}}
	response := httptest.NewRecorder()
	handler.StatFile(response, evidenceStatRequest(&principal))
	if response.Code != http.StatusUnauthorized || len(stub.access) != 0 {
		t.Fatalf("status = %d calls = %d", response.Code, len(stub.access))
	}
}

// TestEvidenceMetadataRequiresResourceIdentifiers 断言缺少 resource_type/resource_id 的
// 请求直接 422：对象级绑定是必要条件，不能退化成"只校验应用级绑定"。
func TestEvidenceMetadataRequiresResourceIdentifiers(t *testing.T) {
	for _, target := range []string{
		"/file-gateway/api/v1/files/file-1/evidence-metadata",
		"/file-gateway/api/v1/files/file-1/evidence-metadata?resource_type=PROJECT_SERVICE_ITEM",
		"/file-gateway/api/v1/files/file-1/evidence-metadata?resource_type=%20&resource_id=SI-EVIDENCE-1",
		"/file-gateway/api/v1/files/file-1/evidence-metadata?resource_type=PROJECT_SERVICE_ITEM&resource_id=",
	} {
		t.Run(target, func(t *testing.T) {
			stub := &evidenceMetadataStub{}
			handler := newEvidenceTestHandler(t, stub)
			request := httptest.NewRequest(http.MethodGet, target, nil)
			request.SetPathValue("file_id", "file-1")
			request = request.WithContext(authctx.WithPrincipal(request.Context(), *machineEvidencePrincipal()))
			response := httptest.NewRecorder()
			handler.StatFile(response, request)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d want 422 body=%s", response.Code, response.Body.String())
			}
			if len(stub.access) != 0 {
				t.Fatalf("request without resource identifiers reached the service layer")
			}
		})
	}
}

// TestEvidenceMetadataPassesOptionalBindingType 断言可选 binding_type 被下推，
// 缺失时保持空值（服务端只按资源维度过滤）。
func TestEvidenceMetadataPassesOptionalBindingType(t *testing.T) {
	stub := &evidenceMetadataStub{metadata: application.EvidenceMetadata{FileID: "file-1", TenantID: "tenant-1", ApplicationID: "app-project", Status: "READY", SHA256: strings.Repeat("a", 64), SizeBytes: 128, BindingActive: true, CreatedAt: time.Now().UTC()}}
	handler := newEvidenceTestHandler(t, stub)
	request := httptest.NewRequest(http.MethodGet, "/file-gateway/api/v1/files/file-1/evidence-metadata?resource_type=PROJECT_SERVICE_ITEM&resource_id=SI-EVIDENCE-1&binding_type=FIELD_EVIDENCE", nil)
	request.SetPathValue("file_id", "file-1")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), *machineEvidencePrincipal()))
	response := httptest.NewRecorder()
	handler.StatFile(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if len(stub.access) != 1 || stub.access[0].BindingType != "FIELD_EVIDENCE" {
		t.Fatalf("binding type = %+v", stub.access)
	}
}

// TestNewEvidenceHandlerFailsClosedOnMissingDependencies 断言依赖缺失时构造失败，
// 不允许把空处理器挂到路由上。
func TestNewEvidenceHandlerFailsClosedOnMissingDependencies(t *testing.T) {
	if _, err := NewEvidenceHandler(nil, slog.Default()); err == nil {
		t.Fatal("nil service accepted")
	}
	if _, err := NewEvidenceHandler(&evidenceMetadataStub{}, nil); err == nil {
		t.Fatal("nil logger accepted")
	}
}
