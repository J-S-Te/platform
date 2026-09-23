package tenantclonehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/tenantclone"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type cloneServiceStub struct {
	input     tenantclone.Input
	callCount int
}

func (stub *cloneServiceStub) Clone(_ context.Context, input tenantclone.Input) (tenantclone.Result, error) {
	stub.input = input
	stub.callCount++
	return tenantclone.Result{OperationID: "operation-1", Status: "COMPLETED"}, nil
}

// 安全（SEC-D1）负向测试：目标租户与调用方租户不一致且未持有平台级跨租户权限时
// 必须 403，且不得触达克隆服务——旧断言把“写入他人租户 200”当作正确行为，已纠正。
func TestCloneAuthorizationCatalogRejectsCrossTenantWithoutPlatformPermission(t *testing.T) {
	service := &cloneServiceStub{}
	handler, err := NewHandler(service)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/target/authorization-catalog-clone", nil)
	request.SetPathValue("tenant_id", "target")
	request.Header.Set("Idempotency-Key", "request-1")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "source"},
		User:   authctx.ReferenceName{ID: "operator"},
		// 未持有任何跨租户权威。
		PermissionCodes: []string{"platform:application:create", "platform:role:create"},
	}))
	response := httptest.NewRecorder()

	handler.CloneAuthorizationCatalog(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body = %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	if service.callCount != 0 {
		t.Fatalf("Clone() 调用次数 = %d, 跨租户被拒后不应触达服务", service.callCount)
	}
	if !strings.Contains(response.Body.String(), "TENANT_CLONE_CROSS_TENANT_FORBIDDEN") {
		t.Fatalf("body = %s, want TENANT_CLONE_CROSS_TENANT_FORBIDDEN", response.Body.String())
	}
}

// 正向测试：持有平台级跨租户权限的主体仍可向其他租户克隆，源租户取自认证主体。
func TestCloneAuthorizationCatalogAllowsCrossTenantWithPlatformPermission(t *testing.T) {
	service := &cloneServiceStub{}
	handler, err := NewHandler(service)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/target/authorization-catalog-clone", nil)
	request.SetPathValue("tenant_id", "target")
	request.Header.Set("Idempotency-Key", "request-1")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "source"},
		User:   authctx.ReferenceName{ID: "operator"},
		PermissionCodes: []string{
			"platform:application:create",
			"platform:role:create",
			tenantclone.PermissionCrossTenantClone,
		},
	}))
	response := httptest.NewRecorder()

	handler.CloneAuthorizationCatalog(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if service.callCount != 1 {
		t.Fatalf("Clone() 调用次数 = %d, want 1", service.callCount)
	}
	if service.input.SourceTenantID != "source" || service.input.TargetTenantID != "target" || service.input.IdempotencyKey != "request-1" || service.input.OperatorID != "operator" {
		t.Fatalf("Clone() input = %+v", service.input)
	}
}

func TestCloneAuthorizationCatalogRequiresIdempotencyKey(t *testing.T) {
	service := &cloneServiceStub{}
	handler, _ := NewHandler(service)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/target/authorization-catalog-clone", nil)
	request.SetPathValue("tenant_id", "target")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "source"}}))
	response := httptest.NewRecorder()

	handler.CloneAuthorizationCatalog(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if service.callCount != 0 {
		t.Fatalf("Clone() 调用次数 = %d, want 0", service.callCount)
	}
}
