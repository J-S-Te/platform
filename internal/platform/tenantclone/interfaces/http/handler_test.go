package tenantclonehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/tenantclone"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type cloneServiceStub struct {
	input tenantclone.Input
}

func (stub *cloneServiceStub) Clone(_ context.Context, input tenantclone.Input) (tenantclone.Result, error) {
	stub.input = input
	return tenantclone.Result{OperationID: "operation-1", Status: "COMPLETED"}, nil
}

func TestCloneAuthorizationCatalogUsesAuthenticatedTenantAsSource(t *testing.T) {
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
	}))
	response := httptest.NewRecorder()

	handler.CloneAuthorizationCatalog(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
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
}
