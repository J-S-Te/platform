package identityhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
)

// employeeBatchIssuesFromBody 解析 422 batchValidation 响应里的完整 issues 列表。
func employeeBatchIssuesFromBody(t *testing.T, recorder *httptest.ResponseRecorder) (string, []employeeBatchIssue) {
	t.Helper()
	var envelope struct {
		Code    string                         `json:"code"`
		Details employeeBatchValidationDetails `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode batch validation body: %v, body = %s", err, recorder.Body.String())
	}
	return envelope.Code, envelope.Details.Issues
}

// 逐行预校验必须一次返回所有行的问题。修复前首个错误就整批 422，导入管理员只能按
// 单条错误反复重试；这里同时覆盖缺组织、缺岗位和角色条目不完整的三种独立问题。
func TestCreateEmployeesBatchCollectsEveryRowIssueInOneResponse(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": {Unrestricted: true},
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployeesBatch(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees/batch",
		`{"items":[`+
			// 第 2 行：组织名称在目录中不存在。
			`{"display_name":"缺组织员工","line_no":2,"organization":"不存在的组织","position":"岗位A"},`+
			// 第 5 行：组织可解析，但岗位留空。
			`{"display_name":"缺岗位员工","line_no":5,"organization":"组织部A","position":""},`+
			// 第 9 行：角色条目只填了应用名称，缺少角色。
			`{"display_name":"角色不完整员工","line_no":9,"organization":"组织部A","position":"岗位A","application_roles":[{"application_name":"合同管理系统"}]},`+
			// 第 4 行（未提供 line_no，按请求顺序回退）：岗位不属于该组织。
			`{"display_name":"岗位不匹配员工","organization":"组织部A","position":"岗位B"}]}`,
		"platform:user:create", "platform:membership:create", "platform:role-binding:update"))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s, want 422", recorder.Code, recorder.Body.String())
	}
	code, issues := employeeBatchIssuesFromBody(t, recorder)
	if code != "PLATFORM_BATCH_VALIDATION_ERROR" {
		t.Fatalf("code = %q, want PLATFORM_BATCH_VALIDATION_ERROR", code)
	}
	if len(issues) != 4 {
		t.Fatalf("issues = %+v, want all 4 rows reported", issues)
	}
	expected := []struct {
		row   int
		field string
		code  string
	}{
		{row: 2, field: "organization", code: "ORGANIZATION_NOT_FOUND"},
		{row: 5, field: "position", code: "POSITION_REQUIRED"},
		{row: 9, field: "application_roles", code: "ROLE_ASSIGNMENT_INVALID"},
		{row: 4, field: "position", code: "POSITION_NOT_FOUND"},
	}
	for index, want := range expected {
		got := issues[index]
		if got.Row != want.row || got.Field != want.field || got.Code != want.code {
			t.Fatalf("issue %d = %+v, want row=%d field=%s code=%s", index, got, want.row, want.field, want.code)
		}
		if got.Message == "" {
			t.Fatalf("issue %d message must not be empty", index)
		}
	}
	// 整批仍然原子：只要存在任一 issue 就绝不调用应用服务，因此没有部分写入。
	if service.batchCalls != 0 {
		t.Fatalf("CreateEmployeesBatch must not be called when any row is invalid; calls = %d", service.batchCalls)
	}
	if len(service.lastBatchInput.Items) != 0 {
		t.Fatalf("batch input = %+v, want none", service.lastBatchInput.Items)
	}
}

// 只有一行失效时也不能先写其它合法行：仍是整批 422，服务层不被调用。
func TestCreateEmployeesBatchKeepsWholeBatchAtomicWhenOnlyOneRowIsInvalid(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": {Unrestricted: true},
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployeesBatch(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees/batch",
		`{"items":[`+
			`{"display_name":"合法员工一","organization":"组织部A","position":"岗位A"},`+
			`{"display_name":"缺组织员工","organization":"","position":"岗位A"},`+
			`{"display_name":"合法员工二","organization":"组织部B","position":"岗位B"}]}`,
		"platform:user:create", "platform:membership:create"))

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s, want 422", recorder.Code, recorder.Body.String())
	}
	_, issues := employeeBatchIssuesFromBody(t, recorder)
	if len(issues) != 1 || issues[0].Row != 2 || issues[0].Code != "ORGANIZATION_REQUIRED" {
		t.Fatalf("issues = %+v, want single row 2 ORGANIZATION_REQUIRED", issues)
	}
	if service.batchCalls != 0 {
		t.Fatalf("CreateEmployeesBatch calls = %d, want 0", service.batchCalls)
	}
}

// 应用角色权限是与逐行数据无关的整批前置条件：缺失时仍立即 403（失败关闭），
// 不会因为同时存在行数据问题而降级成 422。范围越界同理。
func TestCreateEmployeesBatchRejectsRoleBindingPermissionBeforeRowIssues(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": {Unrestricted: true},
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployeesBatch(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees/batch",
		`{"items":[`+
			`{"display_name":"缺组织但带角色","organization":"不存在的组织","position":"岗位A","application_roles":[{"application_name":"合同管理系统","role_name":"审计管理员"}]}]}`,
		"platform:user:create", "platform:membership:create"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.batchCalls != 0 {
		t.Fatalf("CreateEmployeesBatch calls = %d, want 0", service.batchCalls)
	}
}
