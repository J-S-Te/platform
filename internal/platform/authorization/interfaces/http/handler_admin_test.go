package http

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// authorizationRepositoryStub 嵌入接口占位：未覆写的方法一旦被调用即 nil panic，
// 防止用例在未察觉的情况下走过未桩接的路径。
type authorizationRepositoryStub struct {
	application.Repository
	tenantsSeen []string
	rolesResult application.PageResult[domain.Role]
	rolesErr    error
	createErr   error
	createdRole domain.Role
}

func (stub *authorizationRepositoryStub) ListRoles(_ context.Context, tenantID string, _ application.PageRequest) (application.PageResult[domain.Role], error) {
	stub.tenantsSeen = append(stub.tenantsSeen, tenantID)
	if stub.rolesErr != nil {
		return application.PageResult[domain.Role]{}, stub.rolesErr
	}
	return stub.rolesResult, nil
}

func (stub *authorizationRepositoryStub) CreateRole(_ context.Context, tenantID, _, _ string, role domain.Role, _ []string) (domain.Role, error) {
	stub.tenantsSeen = append(stub.tenantsSeen, tenantID)
	if stub.createErr != nil {
		return domain.Role{}, stub.createErr
	}
	stub.createdRole = role
	return role, nil
}

// authorizationIDsStub / authorizationClockStub 是最小依赖实现。
type authorizationIDsStub struct{}

func (authorizationIDsStub) New(time.Time) (string, error) { return "01AUTHORIZATION000000000001", nil }

type authorizationClockStub struct{}

func (authorizationClockStub) Now() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }

func authorizationHandlerFor(t *testing.T, stub *authorizationRepositoryStub) *Handler {
	t.Helper()
	service, err := application.NewService(stub, authorizationIDsStub{}, authorizationClockStub{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(service, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func authorizationRequest(t *testing.T, method, target, body string, principal *authctx.Principal) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if principal != nil {
		request = request.WithContext(authctx.WithPrincipal(request.Context(), *principal))
	}
	return request
}

func authorizationPrincipal() authctx.Principal {
	return authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "admin-1"}}
}

// 组织岗位相关的授权管理端点必须要求已认证主体。
func TestAuthorizationEndpointsRequirePrincipal(t *testing.T) {
	stub := &authorizationRepositoryStub{}
	handler := authorizationHandlerFor(t, stub)
	calls := map[string]func(http.ResponseWriter, *http.Request){
		"listResources":   handler.ListResources,
		"listPermissions": handler.ListPermissions,
		"listRoles":       handler.ListRoles,
		"listBindings":    handler.ListRoleBindings,
		"impact":          handler.PreviewRoleBindingImpact,
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			call(response, authorizationRequest(t, http.MethodGet, "/roles", "", nil))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
	if len(stub.tenantsSeen) != 0 {
		t.Fatalf("unauthenticated requests reached the repository: %v", stub.tenantsSeen)
	}
}

// 角色列表按认证主体的租户查询，且不因分页参数缺失而失败。
func TestAuthorizationListRolesBindsTenantFromPrincipal(t *testing.T) {
	stub := &authorizationRepositoryStub{rolesResult: application.PageResult[domain.Role]{Items: []domain.Role{{ID: "role-1", Name: "销售"}}, Page: 1, PageSize: 20, Total: 1}}
	handler := authorizationHandlerFor(t, stub)
	principal := authorizationPrincipal()
	response := httptest.NewRecorder()
	handler.ListRoles(response, authorizationRequest(t, http.MethodGet, "/roles", "", &principal))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	if len(stub.tenantsSeen) != 1 || stub.tenantsSeen[0] != "tenant-1" {
		t.Fatalf("repository saw tenants %v", stub.tenantsSeen)
	}
}

// 仓储错误必须映射为确定性状态码（组织岗位/权限管理验收的可预期失败语义）。
func TestAuthorizationListRolesMapsErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"validation": {application.ErrValidation, http.StatusUnprocessableEntity},
		"forbidden":  {application.ErrForbidden, http.StatusForbidden},
		"conflict":   {application.ErrConflict, http.StatusConflict},
		"internal":   {errors.New("authorization store down"), http.StatusInternalServerError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &authorizationRepositoryStub{rolesErr: testCase.err}
			handler := authorizationHandlerFor(t, stub)
			principal := authorizationPrincipal()
			response := httptest.NewRecorder()
			handler.ListRoles(response, authorizationRequest(t, http.MethodGet, "/roles", "", &principal))
			if response.Code != testCase.want {
				t.Fatalf("status = %d, want %d (%s)", response.Code, testCase.want, response.Body.String())
			}
		})
	}
}

// 创建角色的请求体不得携带 tenant_id 等未知字段（拒绝而不是静默忽略）。
func TestAuthorizationCreateRoleRejectsUnknownFields(t *testing.T) {
	stub := &authorizationRepositoryStub{}
	handler := authorizationHandlerFor(t, stub)
	principal := authorizationPrincipal()
	response := httptest.NewRecorder()
	handler.CreateRole(response, authorizationRequest(t, http.MethodPost, "/roles",
		`{"code":"SALES","name":"销售","tenant_id":"tenant-attacker"}`, &principal))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", response.Code, response.Body.String())
	}
	if stub.createdRole.Code != "" {
		t.Fatal("rejected request must not reach the repository")
	}
}
