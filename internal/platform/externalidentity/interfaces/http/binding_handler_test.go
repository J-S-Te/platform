package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/externalidentity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/appctx"
)

type bindingServiceStub struct {
	bindInput    application.BindInput
	disableInput application.BindInput
	statusResult domain.BindingResult
	statusErr    error
	// portalApplicationCode 模拟实例配置；默认回落到包级默认值。
	portalApplicationCode string
	statusApplicationCode string
}

func (stub *bindingServiceStub) PortalApplicationCode() string {
	if stub.portalApplicationCode != "" {
		return stub.portalApplicationCode
	}
	return application.PortalApplicationCode
}

func (*bindingServiceStub) Provision(context.Context, appctx.Principal, application.RequestProof, application.ProvisionInput) (application.ProvisionResult, error) {
	return application.ProvisionResult{}, errors.New("not used")
}
func (*bindingServiceStub) AssignPortalRole(context.Context, appctx.Principal, application.RequestProof, application.RoleInput) (domain.RoleResult, error) {
	return domain.RoleResult{}, errors.New("not used")
}
func (*bindingServiceStub) RevokePortalRole(context.Context, appctx.Principal, application.RequestProof, application.RoleInput) (domain.RoleResult, error) {
	return domain.RoleResult{}, errors.New("not used")
}
func (stub *bindingServiceStub) BindCustomer(_ context.Context, _ appctx.Principal, _ application.RequestProof, input application.BindInput) (domain.BindingResult, error) {
	stub.bindInput = input
	return domain.BindingResult{PlatformUserID: input.PlatformUserID, ApplicationCode: application.PortalApplicationCode, Status: domain.BindingActive}, nil
}
func (stub *bindingServiceStub) DisableCustomerBinding(_ context.Context, _ appctx.Principal, _ application.RequestProof, input application.BindInput) (domain.BindingResult, error) {
	stub.disableInput = input
	return domain.BindingResult{PlatformUserID: input.PlatformUserID, ApplicationCode: application.PortalApplicationCode, Status: domain.BindingDisabled}, nil
}
func (stub *bindingServiceStub) CustomerBindingStatus(_ context.Context, _ string, _ string, applicationCode string) (domain.BindingResult, error) {
	// 记录查询使用的应用码：读取必须与服务实例配置一致，不能硬编码默认门户编码。
	stub.statusApplicationCode = applicationCode
	return stub.statusResult, stub.statusErr
}

func newBindingHandler(stub *bindingServiceStub) *Handler {
	return &Handler{service: stub, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func newBindingHTTPRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Idempotency-Key", "key-1")
	request.Header.Set("X-Integration-Timestamp", time.Now().UTC().Format(time.RFC3339Nano))
	request.Header.Set("X-Integration-Nonce", "nonce-1")
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(appctx.WithPrincipal(request.Context(), appctx.Principal{
		OAuthClientID: "oauth-1", ClientID: "crm-external", TenantID: "tenant-a",
		ApplicationID: "crm-app", ApplicationCode: "crm", EnvironmentID: "env-1", EnvironmentCode: "prod",
		Scopes: map[string]struct{}{"portal_mapping_provision": {}},
	}))
	return request
}

func TestBindCustomerHandler(t *testing.T) {
	stub := &bindingServiceStub{}
	handler := newBindingHandler(stub)
	request := newBindingHTTPRequest(http.MethodPut, "/api/v1/internal/external-users/user-1/customer-binding", `{"customer_ref":" CRM-CUST-1 "}`)
	request.SetPathValue("platform_user_id", " user-1 ")
	response := httptest.NewRecorder()

	handler.BindCustomer(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	// 处理器只负责路径参数去空白与请求体转发；customer_ref 的规范化与校验由应用服务完成。
	if stub.bindInput.PlatformUserID != "user-1" || stub.bindInput.CustomerRef != " CRM-CUST-1 " {
		t.Fatalf("bind input = %#v", stub.bindInput)
	}
	var envelope struct {
		Data domain.BindingResult `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Data.Status != domain.BindingActive || envelope.Data.PlatformUserID != "user-1" {
		t.Fatalf("response data = %#v", envelope.Data)
	}
}

func TestDisableCustomerBindingHandler(t *testing.T) {
	stub := &bindingServiceStub{}
	handler := newBindingHandler(stub)
	request := newBindingHTTPRequest(http.MethodPost, "/api/v1/internal/external-users/user-1/customer-binding/disable", `{"customer_ref":"CRM-CUST-1"}`)
	request.SetPathValue("platform_user_id", "user-1")
	response := httptest.NewRecorder()

	handler.DisableCustomerBinding(response, request)

	if response.Code != http.StatusOK || stub.disableInput.CustomerRef != "CRM-CUST-1" {
		t.Fatalf("status = %d, input = %#v", response.Code, stub.disableInput)
	}
}

func TestGetCustomerBindingHandler(t *testing.T) {
	stub := &bindingServiceStub{statusResult: domain.BindingResult{PlatformUserID: "user-1", ApplicationCode: application.PortalApplicationCode, Status: domain.BindingDisabled}}
	handler := newBindingHandler(stub)
	request := newBindingHTTPRequest(http.MethodGet, "/api/v1/internal/external-users/user-1/customer-binding", "")
	request.SetPathValue("platform_user_id", "user-1")
	response := httptest.NewRecorder()

	handler.GetCustomerBinding(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "DISABLED") {
		t.Fatalf("response missing status: %s", response.Body.String())
	}
}

func TestGetCustomerBindingHandlerNotFound(t *testing.T) {
	stub := &bindingServiceStub{statusErr: application.ErrNotFound}
	handler := newBindingHandler(stub)
	request := newBindingHTTPRequest(http.MethodGet, "/api/v1/internal/external-users/user-1/customer-binding", "")
	request.SetPathValue("platform_user_id", "user-1")
	response := httptest.NewRecorder()

	handler.GetCustomerBinding(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestBindingHandlerRejectsInvalidProof(t *testing.T) {
	handler := newBindingHandler(&bindingServiceStub{})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/internal/external-users/user-1/customer-binding", strings.NewReader(`{"customer_ref":"CRM-CUST-1"}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetPathValue("platform_user_id", "user-1")
	request = request.WithContext(appctx.WithPrincipal(request.Context(), appctx.Principal{
		OAuthClientID: "oauth-1", ClientID: "crm-external", TenantID: "tenant-a",
		ApplicationID: "crm-app", ApplicationCode: "crm", EnvironmentID: "env-1", EnvironmentCode: "prod",
		Scopes: map[string]struct{}{"portal_mapping_provision": {}},
	}))
	// 缺少防重放请求头。
	response := httptest.NewRecorder()

	handler.BindCustomer(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", response.Code)
	}
}

// 自定义门户应用码时，写入用实例配置值；读取曾经硬编码 customer_portal，
// 导致刚写入的绑定查不到（对账 404 / 状态永远未绑定）。
func TestGetCustomerBindingUsesConfiguredPortalApplicationCode(t *testing.T) {
	stub := &bindingServiceStub{portalApplicationCode: "custom_portal", statusResult: domain.BindingResult{Status: domain.BindingActive}}
	handler := newBindingHandler(stub)
	request := newBindingHTTPRequest(http.MethodGet, "/external-identities/users/user-1/customer-binding", "")
	recorder := httptest.NewRecorder()

	handler.GetCustomerBinding(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if stub.statusApplicationCode != "custom_portal" {
		t.Fatalf("查询应用码 = %q, want custom_portal", stub.statusApplicationCode)
	}
}

func TestGetCustomerBindingFallsBackToDefaultPortalApplicationCode(t *testing.T) {
	stub := &bindingServiceStub{}
	handler := newBindingHandler(stub)
	handler.GetCustomerBinding(httptest.NewRecorder(), newBindingHTTPRequest(http.MethodGet, "/external-identities/users/user-1/customer-binding", ""))

	if stub.statusApplicationCode != application.PortalApplicationCode {
		t.Fatalf("查询应用码 = %q, want %q", stub.statusApplicationCode, application.PortalApplicationCode)
	}
}
