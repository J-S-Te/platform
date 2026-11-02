package identityhttp

import (
	"bytes"
	"context"
	"errors"
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

// userScopeServiceStub 只桩接用户/账号管理与范围判定所需的方法；嵌入接口占位使任何
// 未预期的调用直接 panic，避免用例在未察觉的情况下走过未桩接的路径。
type userScopeServiceStub struct {
	managementApplicationService

	scopeOrgUnits map[string][]string
	scopeErrors   map[string]error
	scopeCalls    []string

	listUserQuery    application.PageRequest
	listAccountQuery application.PageRequest
	listUserTotal    int64

	getUserCalls      int
	updateUserCalls   int
	deleteUserCalls   int
	updateAccountCall int
	listAccountItems  int
}

func (stub *userScopeServiceStub) ListUsers(_ context.Context, _ string, query application.PageRequest) (application.PageResult[application.UserView], error) {
	stub.listUserQuery = query
	return application.PageResult[application.UserView]{Items: []application.UserView{{ID: "usr-1", DisplayName: "范围内用户"}}, Page: query.Page, PageSize: query.PageSize, Total: stub.listUserTotal}, nil
}

func (stub *userScopeServiceStub) GetUser(context.Context, string, string) (application.UserView, error) {
	stub.getUserCalls++
	return application.UserView{ID: "usr-1", DisplayName: "范围内用户"}, nil
}

func (stub *userScopeServiceStub) UpdateUser(context.Context, application.UserUpdateInput) (application.UserView, error) {
	stub.updateUserCalls++
	return application.UserView{ID: "usr-1", DisplayName: "范围内用户"}, nil
}

func (stub *userScopeServiceStub) DeleteUser(context.Context, application.UserDeleteInput) error {
	stub.deleteUserCalls++
	return nil
}

func (stub *userScopeServiceStub) ListAccounts(_ context.Context, _ string, query application.PageRequest) (application.PageResult[domain.Account], error) {
	stub.listAccountQuery = query
	items := make([]domain.Account, 0, stub.listAccountItems)
	for index := 0; index < stub.listAccountItems; index++ {
		items = append(items, domain.Account{ID: "acc-1", Status: domain.StatusActive})
	}
	return application.PageResult[domain.Account]{Items: items, Page: query.Page, PageSize: query.PageSize, Total: int64(len(items))}, nil
}

func (stub *userScopeServiceStub) UpdateAccount(context.Context, application.AccountUpdateInput) (domain.Account, error) {
	stub.updateAccountCall++
	return domain.Account{ID: "acc-1", Status: domain.StatusActive}, nil
}

func (stub *userScopeServiceStub) UserManagementScopeOrgUnitIDs(_ context.Context, _ string, userID string) ([]string, error) {
	stub.scopeCalls = append(stub.scopeCalls, "user:"+userID)
	if err, ok := stub.scopeErrors["user:"+userID]; ok {
		return nil, err
	}
	return stub.scopeOrgUnits["user:"+userID], nil
}

func (stub *userScopeServiceStub) AccountManagementScopeOrgUnitIDs(_ context.Context, _ string, accountID string) ([]string, error) {
	stub.scopeCalls = append(stub.scopeCalls, "account:"+accountID)
	if err, ok := stub.scopeErrors["account:"+accountID]; ok {
		return nil, err
	}
	return stub.scopeOrgUnits["account:"+accountID], nil
}

// scopedAuthorizer 按权限码返回可配置范围；未配置的权限码返回空范围（与真实解析一致：
// 没有任何绑定就不隐含权限）。
type scopedAuthorizer struct {
	scopes map[string]managementscope.Scope
}

func (authorizer scopedAuthorizer) Resolve(_ context.Context, _ managementscope.Subject, permissionCode string) (managementscope.Scope, error) {
	return authorizer.scopes[permissionCode], nil
}

func (scopedAuthorizer) ResourceContext(context.Context, string, string, string) (managementscope.ResourceContext, error) {
	return managementscope.ResourceContext{}, nil
}

func newUserScopeHandler(t *testing.T, service *userScopeServiceStub, authorizer managementscope.Authorizer) *ManagementHandler {
	t.Helper()
	handler, err := NewManagementHandler(service, slog.New(slog.NewTextHandler(io.Discard, nil)), authorizer)
	if err != nil {
		t.Fatalf("NewManagementHandler() error = %v", err)
	}
	return handler
}

func userScopeRequest(t *testing.T, method, target, body, pathKey, pathValue string) *http.Request {
	t.Helper()
	var payload io.Reader = http.NoBody
	if body != "" {
		payload = bytes.NewBufferString(body)
	}
	request := httptest.NewRequest(method, target, payload)
	request.SetPathValue(pathKey, pathValue)
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "operator-1"},
	}))
}

func organizationScope(ids ...string) managementscope.Scope {
	return managementscope.Scope{OrgUnitIDs: ids}
}

// IAM-01：受限范围下范围外用户的读取必须在返回详情之前被拒绝。
func TestGetUserRejectsTargetOutsideManagementScope(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"user:usr-outside": {"org-b"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:read": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.GetUser(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/users/usr-outside", "", "user_id", "usr-outside"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.getUserCalls != 0 {
		t.Fatalf("GetUser must not read an out-of-scope user; calls = %d", service.getUserCalls)
	}
}

func TestGetUserAllowsTargetInsideManagementScope(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"user:usr-inside": {"org-a-descendant"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:read": organizationScope("org-a", "org-a-descendant"),
	}})
	recorder := httptest.NewRecorder()

	handler.GetUser(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/users/usr-inside", "", "user_id", "usr-inside"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if service.getUserCalls != 1 {
		t.Fatalf("GetUser calls = %d, want 1", service.getUserCalls)
	}
}

// IAM-01：更新必须在任何写入之前判定范围，绝不能先写后查。
func TestUpdateUserRejectsTargetOutsideManagementScopeBeforeWrite(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"user:usr-outside": {"org-b"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:update": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.UpdateUser(recorder, userScopeRequest(t, http.MethodPatch, "/api/v1/users/usr-outside", `{"display_name":"改名","version":2}`, "user_id", "usr-outside"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.updateUserCalls != 0 {
		t.Fatalf("UpdateUser must not write an out-of-scope user; calls = %d", service.updateUserCalls)
	}
}

func TestUpdateUserAllowsTargetInsideManagementScope(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"user:usr-inside": {"org-a"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:update": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.UpdateUser(recorder, userScopeRequest(t, http.MethodPatch, "/api/v1/users/usr-inside", `{"display_name":"改名","version":2}`, "user_id", "usr-inside"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if service.updateUserCalls != 1 {
		t.Fatalf("UpdateUser calls = %d, want 1", service.updateUserCalls)
	}
}

// IAM-01：删除必须同样在写入之前判定范围。
func TestDeleteUserRejectsTargetOutsideManagementScopeBeforeWrite(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"user:usr-outside": {"org-b"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:delete": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.DeleteUser(recorder, userScopeRequest(t, http.MethodDelete, "/api/v1/users/usr-outside", `{"version":3}`, "user_id", "usr-outside"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.deleteUserCalls != 0 {
		t.Fatalf("DeleteUser must not delete an out-of-scope user; calls = %d", service.deleteUserCalls)
	}
}

func TestDeleteUserAllowsTargetInsideManagementScope(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"user:usr-inside": {"org-a"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:delete": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.DeleteUser(recorder, userScopeRequest(t, http.MethodDelete, "/api/v1/users/usr-inside", `{"version":3}`, "user_id", "usr-inside"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if service.deleteUserCalls != 1 {
		t.Fatalf("DeleteUser calls = %d, want 1", service.deleteUserCalls)
	}
}

// IAM-01：账号继承属主用户的有效任职归属，范围外账号的更新必须在写入之前被拒绝。
func TestUpdateAccountRejectsAccountOutsideManagementScopeBeforeWrite(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"account:acc-outside": {"org-b"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:account:update": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.UpdateAccount(recorder, userScopeRequest(t, http.MethodPatch, "/api/v1/accounts/acc-outside", `{"status":"DISABLED","version":2}`, "account_id", "acc-outside"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.updateAccountCall != 0 {
		t.Fatalf("UpdateAccount must not write an out-of-scope account; calls = %d", service.updateAccountCall)
	}
}

func TestUpdateAccountAllowsAccountInsideManagementScope(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"account:acc-inside": {"org-a"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:account:update": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.UpdateAccount(recorder, userScopeRequest(t, http.MethodPatch, "/api/v1/accounts/acc-inside", `{"status":"DISABLED","version":2}`, "account_id", "acc-inside"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if service.updateAccountCall != 1 {
		t.Fatalf("UpdateAccount calls = %d, want 1", service.updateAccountCall)
	}
}

// IAM-01：没有属主用户的服务账号不属于任何组织范围，受限调用者不得管理。
func TestUpdateAccountRejectsServiceAccountWithoutOwner(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"account:acc-service": nil}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:account:update": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.UpdateAccount(recorder, userScopeRequest(t, http.MethodPatch, "/api/v1/accounts/acc-service", `{"status":"DISABLED","version":2}`, "account_id", "acc-service"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.updateAccountCall != 0 {
		t.Fatalf("UpdateAccount calls = %d, want 0", service.updateAccountCall)
	}
}

// 资源级绑定不能可靠还原为人员归属：仅含 RESOURCE 范围的受限调用者必须被拒绝。
func TestResourceOnlyScopeIsRejectedForUserManagement(t *testing.T) {
	service := &userScopeServiceStub{scopeOrgUnits: map[string][]string{"user:usr-1": {"org-a"}}}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:read": {ResourceIDs: []string{"usr-1"}},
	}})
	recorder := httptest.NewRecorder()

	handler.GetUser(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/users/usr-1", "", "user_id", "usr-1"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.getUserCalls != 0 {
		t.Fatalf("GetUser calls = %d, want 0", service.getUserCalls)
	}
}

// 无范围绑定（TENANT）时行为保持不变：不额外查询范围上下文，读取照常成功。
func TestUnrestrictedScopeKeepsUserManagementBehaviour(t *testing.T) {
	service := &userScopeServiceStub{}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:read": {Unrestricted: true},
	}})
	recorder := httptest.NewRecorder()

	handler.GetUser(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/users/usr-any", "", "user_id", "usr-any"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if len(service.scopeCalls) != 0 {
		t.Fatalf("unrestricted scope must not resolve target scope contexts; calls = %v", service.scopeCalls)
	}
	if service.getUserCalls != 1 {
		t.Fatalf("GetUser calls = %d, want 1", service.getUserCalls)
	}
}

// 没有任何有效绑定时解析出空范围，入口仍然直接拒绝（既有行为不变）。
func TestEmptyManagementScopeIsRejected(t *testing.T) {
	service := &userScopeServiceStub{}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{})
	recorder := httptest.NewRecorder()

	handler.ListUsers(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/users", "", "user_id", "unused"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if service.listUserQuery.ScopeRestricted {
		t.Fatal("rejected request must not reach the service")
	}
}

// 范围上下文读取失败必须失败关闭：不得继续写入，目标不存在也不能变成存在性探测。
func TestManagementScopeContextFailureFailsClosed(t *testing.T) {
	notFound := &userScopeServiceStub{scopeErrors: map[string]error{"user:usr-missing": application.ErrNotFound}}
	handler := newUserScopeHandler(t, notFound, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:delete": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()
	handler.DeleteUser(recorder, userScopeRequest(t, http.MethodDelete, "/api/v1/users/usr-missing", `{"version":3}`, "user_id", "usr-missing"))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing target status = %d, want 403", recorder.Code)
	}
	if notFound.deleteUserCalls != 0 {
		t.Fatalf("DeleteUser calls = %d, want 0", notFound.deleteUserCalls)
	}

	broken := &userScopeServiceStub{scopeErrors: map[string]error{"user:usr-1": errors.New("storage unavailable")}}
	handler = newUserScopeHandler(t, broken, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:update": organizationScope("org-a"),
	}})
	recorder = httptest.NewRecorder()
	handler.UpdateUser(recorder, userScopeRequest(t, http.MethodPatch, "/api/v1/users/usr-1", `{"display_name":"改名","version":2}`, "user_id", "usr-1"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("storage failure status = %d, want 500", recorder.Code)
	}
	if broken.updateUserCalls != 0 {
		t.Fatalf("UpdateUser calls = %d, want 0", broken.updateUserCalls)
	}
}

// 列表必须把受限范围下推到查询契约，而不是先取全租户再在内存过滤。
func TestListUsersPushesRestrictedScopeIntoQuery(t *testing.T) {
	service := &userScopeServiceStub{listUserTotal: 7}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:read": organizationScope("org-a", "org-b"),
	}})
	recorder := httptest.NewRecorder()

	handler.ListUsers(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/users?page=2&page_size=5", "", "user_id", "unused"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	query := service.listUserQuery
	if !query.ScopeRestricted {
		t.Fatal("restricted scope must be pushed into the page request")
	}
	if len(query.AllowedOrgUnitIDs) != 2 || query.AllowedOrgUnitIDs[0] != "org-a" || query.AllowedOrgUnitIDs[1] != "org-b" {
		t.Fatalf("allowed organizations = %v, want [org-a org-b]", query.AllowedOrgUnitIDs)
	}
	if query.Page != 2 || query.PageSize != 5 {
		t.Fatalf("page = %d/%d, want 2/5", query.Page, query.PageSize)
	}
}

func TestListUsersKeepsUnrestrictedQueryUnchanged(t *testing.T) {
	service := &userScopeServiceStub{}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:read": {Unrestricted: true},
	}})
	recorder := httptest.NewRecorder()

	handler.ListUsers(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/users", "", "user_id", "unused"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if service.listUserQuery.ScopeRestricted || len(service.listUserQuery.AllowedOrgUnitIDs) != 0 {
		t.Fatalf("unrestricted query must stay unchanged; got %+v", service.listUserQuery)
	}
}

func TestListAccountsPushesRestrictedScopeIntoQuery(t *testing.T) {
	service := &userScopeServiceStub{listAccountItems: 1}
	handler := newUserScopeHandler(t, service, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:account:read": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.ListAccounts(recorder, userScopeRequest(t, http.MethodGet, "/api/v1/accounts", "", "account_id", "unused"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if !service.listAccountQuery.ScopeRestricted {
		t.Fatal("restricted account scope must be pushed into the page request")
	}
	if len(service.listAccountQuery.AllowedOrgUnitIDs) != 1 || service.listAccountQuery.AllowedOrgUnitIDs[0] != "org-a" {
		t.Fatalf("allowed organizations = %v, want [org-a]", service.listAccountQuery.AllowedOrgUnitIDs)
	}
}
