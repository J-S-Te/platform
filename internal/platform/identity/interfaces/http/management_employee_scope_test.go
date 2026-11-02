package identityhttp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// employeeScopeServiceStub 桩接入职聚合入口与组织/岗位名称解析所需的方法；嵌入接口占位
// 使任何未预期的调用直接 panic，避免用例静默走过未覆盖的应用服务方法。
type employeeScopeServiceStub struct {
	managementApplicationService

	createEmployeeCalls int
	lastEmployeeInput   application.EmployeeCreateInput
	batchCalls          int
	lastBatchInput      application.EmployeeBatchCreateInput
	scopeCalls          int
}

func (stub *employeeScopeServiceStub) CreateEmployee(_ context.Context, input application.EmployeeCreateInput) (application.EmployeeCreateResult, error) {
	stub.createEmployeeCalls++
	stub.lastEmployeeInput = input
	return application.EmployeeCreateResult{User: application.UserView{ID: "usr-new", DisplayName: input.DisplayName}}, nil
}

func (stub *employeeScopeServiceStub) CreateEmployeesBatch(_ context.Context, input application.EmployeeBatchCreateInput) (application.EmployeeBatchCreateResult, error) {
	stub.batchCalls++
	stub.lastBatchInput = input
	users := make([]application.UserView, 0, len(input.Items))
	for _, item := range input.Items {
		users = append(users, application.UserView{ID: "usr-" + item.DisplayName, DisplayName: item.DisplayName})
	}
	return application.EmployeeBatchCreateResult{Users: users}, nil
}

// ListOrgUnits/ListPositions 模拟服务端目录查询：批量导入按人类可读名称解析组织与岗位。
func (stub *employeeScopeServiceStub) ListOrgUnits(context.Context, string, application.PageRequest) (application.PageResult[domain.OrgUnit], error) {
	items := []domain.OrgUnit{
		{ID: "org-a", Name: "组织部A", Status: domain.StatusActive},
		{ID: "org-b", Name: "组织部B", Status: domain.StatusActive},
	}
	return application.PageResult[domain.OrgUnit]{Items: items, Page: 1, PageSize: 100, Total: int64(len(items))}, nil
}

func (stub *employeeScopeServiceStub) ListPositions(context.Context, string, application.PageRequest) (application.PageResult[domain.Position], error) {
	items := []domain.Position{
		{ID: "pos-a", OrgUnitID: "org-a", Name: "岗位A", Status: domain.StatusActive},
		{ID: "pos-b", OrgUnitID: "org-b", Name: "岗位B", Status: domain.StatusActive},
	}
	return application.PageResult[domain.Position]{Items: items, Page: 1, PageSize: 100, Total: int64(len(items))}, nil
}

func (stub *employeeScopeServiceStub) UserManagementScopeOrgUnitIDs(context.Context, string, string) ([]string, error) {
	stub.scopeCalls++
	return nil, nil
}

func (stub *employeeScopeServiceStub) AccountManagementScopeOrgUnitIDs(context.Context, string, string) ([]string, error) {
	stub.scopeCalls++
	return nil, nil
}

// employeeScopeAuthorizer 记录范围解析了哪些权限码，用于断言聚合入口是否真的做了判定。
type employeeScopeAuthorizer struct {
	scopes   map[string]managementscope.Scope
	resolved []string
}

func (authorizer *employeeScopeAuthorizer) Resolve(_ context.Context, _ managementscope.Subject, permissionCode string) (managementscope.Scope, error) {
	authorizer.resolved = append(authorizer.resolved, permissionCode)
	return authorizer.scopes[permissionCode], nil
}

func (authorizer *employeeScopeAuthorizer) ResourceContext(context.Context, string, string, string) (managementscope.ResourceContext, error) {
	return managementscope.ResourceContext{}, nil
}

func newEmployeeScopeHandler(t *testing.T, service *employeeScopeServiceStub, authorizer managementscope.Authorizer) *ManagementHandler {
	t.Helper()
	handler, err := NewManagementHandler(service, slog.New(slog.NewTextHandler(io.Discard, nil)), authorizer)
	if err != nil {
		t.Fatalf("NewManagementHandler() error = %v", err)
	}
	return handler
}

func employeeScopeRequest(t *testing.T, method, target, body string, permissionCodes ...string) *http.Request {
	t.Helper()
	var payload io.Reader = http.NoBody
	if body != "" {
		payload = bytes.NewBufferString(body)
	}
	request := httptest.NewRequest(method, target, payload)
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant:          authctx.ReferenceName{ID: "tenant-1"},
		User:            authctx.ReferenceName{ID: "operator-1"},
		PermissionCodes: permissionCodes,
	}))
}

// IAM-01：入职聚合入口在请求带任职时同样创建任职，范围外组织必须在调用服务之前拒绝。
func TestCreateEmployeeRejectsMembershipOutsideManagementScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": organizationScope("org-a"),
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployee(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees",
		`{"user":{"display_name":"范围外员工"},"membership":{"org_unit_id":"org-b","position_id":"pos-b","membership_type":"PRIMARY"}}`,
		"platform:user:create", "platform:membership:create"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.createEmployeeCalls != 0 {
		t.Fatalf("CreateEmployee must not be called for an out-of-scope membership; calls = %d", service.createEmployeeCalls)
	}
	if len(authorizer.resolved) != 1 || authorizer.resolved[0] != "platform:membership:create" {
		t.Fatalf("resolved permissions = %v, want [platform:membership:create]", authorizer.resolved)
	}
}

func TestCreateEmployeeAllowsMembershipInsideManagementScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": organizationScope("org-a", "org-a-child"),
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployee(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees",
		`{"user":{"display_name":"范围内员工"},"membership":{"org_unit_id":"org-a-child","position_id":"pos-a","membership_type":"PRIMARY"}}`,
		"platform:user:create", "platform:membership:create"))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if service.createEmployeeCalls != 1 {
		t.Fatalf("CreateEmployee calls = %d, want 1", service.createEmployeeCalls)
	}
	if service.lastEmployeeInput.Membership == nil || service.lastEmployeeInput.Membership.OrgUnitID != "org-a-child" {
		t.Fatalf("membership input = %+v, want org-a-child", service.lastEmployeeInput.Membership)
	}
	if service.lastEmployeeInput.TenantID != "tenant-1" || service.lastEmployeeInput.OperatorID != "operator-1" {
		t.Fatalf("input tenant/operator = %s/%s, want tenant-1/operator-1", service.lastEmployeeInput.TenantID, service.lastEmployeeInput.OperatorID)
	}
}

// 不带宽度的员工创建不产生任职，因此不受任职组织范围限制：既有行为必须保持不变。
func TestCreateEmployeeWithoutMembershipSkipsMembershipScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	// 空授权器意味着任何范围解析都会得到空范围并 403；这条用例因此能证明该路径没有
	// 触发任职范围判定。
	authorizer := &employeeScopeAuthorizer{}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployee(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees",
		`{"user":{"display_name":"仅用户档案"}}`, "platform:user:create"))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if service.createEmployeeCalls != 1 {
		t.Fatalf("CreateEmployee calls = %d, want 1", service.createEmployeeCalls)
	}
	if len(authorizer.resolved) != 0 {
		t.Fatalf("resolved permissions = %v, want none", authorizer.resolved)
	}
	if service.lastEmployeeInput.Membership != nil {
		t.Fatalf("membership input = %+v, want nil", service.lastEmployeeInput.Membership)
	}
}

// TENANT 绑定解析为不受限范围：任意组织都可以入职，且不需要额外的归属查询。
func TestCreateEmployeeAllowsAnyOrganizationWithTenantScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": {Unrestricted: true},
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployee(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees",
		`{"user":{"display_name":"租户员工"},"membership":{"org_unit_id":"org-any","position_id":"pos-any","membership_type":"PRIMARY"}}`,
		"platform:user:create", "platform:membership:create"))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if service.createEmployeeCalls != 1 {
		t.Fatalf("CreateEmployee calls = %d, want 1", service.createEmployeeCalls)
	}
	if service.scopeCalls != 0 {
		t.Fatalf("unrestricted scope must not query target membership owners; calls = %d", service.scopeCalls)
	}
}

// 资源级绑定无法还原组织归属：只有 RESOURCE 范围时创建任职必须默认拒绝。
func TestCreateEmployeeRejectsResourceOnlyMembershipScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": {ResourceIDs: []string{"membership-1"}},
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployee(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees",
		`{"user":{"display_name":"资源级员工"},"membership":{"org_unit_id":"org-a","position_id":"pos-a","membership_type":"PRIMARY"}}`,
		"platform:user:create", "platform:membership:create"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.createEmployeeCalls != 0 {
		t.Fatalf("CreateEmployee calls = %d, want 0", service.createEmployeeCalls)
	}
}

// IAM-01：批量导入逐行创建任职，任一行越范围即整批拒绝，不能出现部分写入。
func TestCreateEmployeesBatchRejectsWholeBatchWhenAnyRowOutsideScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": organizationScope("org-a"),
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployeesBatch(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees/batch",
		`{"items":[{"display_name":"范围内员工","organization":"组织部A","position":"岗位A"},`+
			`{"display_name":"范围外员工","organization":"组织部B","position":"岗位B"}]}`))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.batchCalls != 0 {
		t.Fatalf("CreateEmployeesBatch must not be called when any row is out of scope; calls = %d", service.batchCalls)
	}
	if len(authorizer.resolved) != 1 || authorizer.resolved[0] != "platform:membership:create" {
		t.Fatalf("resolved permissions = %v, want [platform:membership:create]", authorizer.resolved)
	}
}

func TestCreateEmployeesBatchAllowsRowsInsideManagementScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": organizationScope("org-a"),
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployeesBatch(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees/batch",
		`{"items":[{"display_name":"员工一","organization":"组织部A","position":"岗位A"},`+
			`{"display_name":"员工二","organization":"组织部A","position":"岗位A"}]}`))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if service.batchCalls != 1 {
		t.Fatalf("CreateEmployeesBatch calls = %d, want 1", service.batchCalls)
	}
	if len(service.lastBatchInput.Items) != 2 {
		t.Fatalf("batch items = %d, want 2", len(service.lastBatchInput.Items))
	}
	for index, item := range service.lastBatchInput.Items {
		if item.Membership == nil || item.Membership.OrgUnitID != "org-a" {
			t.Fatalf("item %d membership = %+v, want org-a", index, item.Membership)
		}
	}
}

// TENANT 绑定下批量导入保持原行为：任意组织都可以导入。
func TestCreateEmployeesBatchAllowsAnyOrganizationWithTenantScope(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:membership:create": {Unrestricted: true},
	}}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployeesBatch(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees/batch",
		`{"items":[{"display_name":"员工一","organization":"组织部B","position":"岗位B"}]}`))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", recorder.Code, recorder.Body.String())
	}
	if service.batchCalls != 1 {
		t.Fatalf("CreateEmployeesBatch calls = %d, want 1", service.batchCalls)
	}
	if len(service.lastBatchInput.Items) != 1 || service.lastBatchInput.Items[0].Membership == nil || service.lastBatchInput.Items[0].Membership.OrgUnitID != "org-b" {
		t.Fatalf("batch items = %+v, want single org-b membership", service.lastBatchInput.Items)
	}
	if service.scopeCalls != 0 {
		t.Fatalf("unrestricted scope must not query target membership owners; calls = %d", service.scopeCalls)
	}
}

// 没有任何 platform:membership:create 绑定时批量导入失败关闭，不得绕过组织范围。
func TestCreateEmployeesBatchRejectsWithoutMembershipScopeBinding(t *testing.T) {
	service := &employeeScopeServiceStub{}
	authorizer := &employeeScopeAuthorizer{}
	handler := newEmployeeScopeHandler(t, service, authorizer)
	recorder := httptest.NewRecorder()

	handler.CreateEmployeesBatch(recorder, employeeScopeRequest(t, http.MethodPost, "/api/v1/employees/batch",
		`{"items":[{"display_name":"员工一","organization":"组织部A","position":"岗位A"}]}`))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.batchCalls != 0 {
		t.Fatalf("CreateEmployeesBatch calls = %d, want 0", service.batchCalls)
	}
}
