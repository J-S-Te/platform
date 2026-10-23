package settingshttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	settingsapplication "github.com/J-S-Te/Basic-Platform/internal/platform/settings/application"
	settingsdomain "github.com/J-S-Te/Basic-Platform/internal/platform/settings/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// settingsServiceStub 记录调用参数并返回预设结果。
type settingsServiceStub struct {
	tenants     []string
	updateErr   error
	applyErr    error
	applyCalls  int
	updateCalls int
}

func (stub *settingsServiceStub) record(tenantID string) {
	stub.tenants = append(stub.tenants, tenantID)
}

func (stub *settingsServiceStub) GetPlatformSettings(_ context.Context, tenantID string) (settingsdomain.PlatformSettings, error) {
	stub.record(tenantID)
	return settingsdomain.PlatformSettings{}, nil
}

func (stub *settingsServiceStub) UpdatePlatformSettings(_ context.Context, input settingsapplication.PlatformSettingsUpdateInput) (settingsdomain.PlatformSettings, error) {
	stub.updateCalls++
	stub.record(input.TenantID)
	if stub.updateErr != nil {
		return settingsdomain.PlatformSettings{}, stub.updateErr
	}
	return settingsdomain.PlatformSettings{}, nil
}

func (stub *settingsServiceStub) GetNotificationSettings(_ context.Context, tenantID string) (settingsdomain.NotificationSettings, error) {
	stub.record(tenantID)
	return settingsdomain.NotificationSettings{}, nil
}

func (stub *settingsServiceStub) UpdateNotificationSettings(_ context.Context, input settingsapplication.NotificationSettingsUpdateInput) (settingsdomain.NotificationSettings, error) {
	stub.updateCalls++
	stub.record(input.TenantID)
	if stub.updateErr != nil {
		return settingsdomain.NotificationSettings{}, stub.updateErr
	}
	return settingsdomain.NotificationSettings{}, nil
}

func (stub *settingsServiceStub) GetAccessSettings(_ context.Context, tenantID string) (settingsdomain.AccessSettings, error) {
	stub.record(tenantID)
	return settingsdomain.AccessSettings{}, nil
}

func (stub *settingsServiceStub) UpdateAccessSettings(_ context.Context, input settingsapplication.AccessSettingsUpdateInput) (settingsdomain.AccessSettings, error) {
	stub.updateCalls++
	stub.record(input.TenantID)
	if stub.updateErr != nil {
		return settingsdomain.AccessSettings{}, stub.updateErr
	}
	return settingsdomain.AccessSettings{}, nil
}

func (stub *settingsServiceStub) ApplyAccessSettings(_ context.Context, tenantID string, _ settingsapplication.AccessApplier) (settingsdomain.AccessSettings, error) {
	stub.applyCalls++
	stub.record(tenantID)
	if stub.applyErr != nil {
		return settingsdomain.AccessSettings{}, stub.applyErr
	}
	return settingsdomain.AccessSettings{}, nil
}

// settingsApplierStub 是访问设置应用器端口的最小实现。
type settingsApplierStub struct{ applications int }

func (stub settingsApplierStub) ApplyAccess(context.Context, settingsapplication.AccessApplyInput) error {
	return nil
}

func settingsHandlerFor(t *testing.T, stub *settingsServiceStub) *Handler {
	t.Helper()
	handler, err := NewHandler(stub, settingsApplierStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func settingsRequest(t *testing.T, method, target, body string, principal *authctx.Principal) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if principal != nil {
		request = request.WithContext(authctx.WithPrincipal(request.Context(), *principal))
	}
	return request
}

func settingsPrincipal() authctx.Principal {
	return authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "admin-1"}}
}

// 七个基础配置端点都必须要求已认证主体。
func TestSettingsEndpointsRequirePrincipal(t *testing.T) {
	stub := &settingsServiceStub{}
	handler := settingsHandlerFor(t, stub)
	calls := map[string]func(http.ResponseWriter, *http.Request){
		"getPlatform":        handler.GetPlatformSettings,
		"updatePlatform":     handler.UpdatePlatformSettings,
		"getNotification":    handler.GetNotificationSettings,
		"updateNotification": handler.UpdateNotificationSettings,
		"getAccess":          handler.GetAccessSettings,
		"updateAccess":       handler.UpdateAccessSettings,
		"applyAccess":        handler.ApplyAccessSettings,
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			call(response, settingsRequest(t, http.MethodGet, "/settings/platform", `{}`, nil))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
	if len(stub.tenants) != 0 {
		t.Fatalf("unauthenticated requests reached the service: %v", stub.tenants)
	}
}

// 读取类端点按认证主体的租户查询。
func TestSettingsReadsBindTenantFromPrincipal(t *testing.T) {
	stub := &settingsServiceStub{}
	handler := settingsHandlerFor(t, stub)
	principal := settingsPrincipal()
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"platform":     handler.GetPlatformSettings,
		"notification": handler.GetNotificationSettings,
		"access":       handler.GetAccessSettings,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			call(response, settingsRequest(t, http.MethodGet, "/settings/platform", "", &principal))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
			}
		})
	}
	for _, tenant := range stub.tenants {
		if tenant != "tenant-1" {
			t.Fatalf("service saw tenant %q", tenant)
		}
	}
	if len(stub.tenants) != 3 {
		t.Fatalf("service calls = %d, want 3", len(stub.tenants))
	}
}

// 写入类端点的领域错误必须映射为确定性状态码。
func TestSettingsUpdatesMapDomainErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"validation":       {settingsapplication.ErrValidation, http.StatusUnprocessableEntity},
		"not found":        {settingsapplication.ErrNotFound, http.StatusNotFound},
		"version conflict": {settingsapplication.ErrVersionConflict, http.StatusConflict},
		"internal":         {errors.New("settings store down"), http.StatusInternalServerError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &settingsServiceStub{updateErr: testCase.err}
			handler := settingsHandlerFor(t, stub)
			principal := settingsPrincipal()
			response := httptest.NewRecorder()
			handler.UpdatePlatformSettings(response, settingsRequest(t, http.MethodPut, "/settings/platform",
				`{"organization_name":"示例公司","organization_alias":"demo","timezone":"Asia/Shanghai","qualification":"Q","version":1}`, &principal))
			if response.Code != testCase.want {
				t.Fatalf("status = %d, want %d (%s)", response.Code, testCase.want, response.Body.String())
			}
		})
	}
}

// 访问设置应用必须使用认证主体租户，并调用一次应用器。
func TestSettingsApplyAccessUsesPrincipalTenant(t *testing.T) {
	stub := &settingsServiceStub{}
	handler := settingsHandlerFor(t, stub)
	principal := settingsPrincipal()
	response := httptest.NewRecorder()
	handler.ApplyAccessSettings(response, settingsRequest(t, http.MethodPost, "/settings/access/apply", `{}`, &principal))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	if stub.applyCalls != 1 || len(stub.tenants) != 1 || stub.tenants[0] != "tenant-1" {
		t.Fatalf("apply calls = %d tenants = %v", stub.applyCalls, stub.tenants)
	}
}

// 请求体自带 tenant_id 必须被拒绝，不得进入服务层。
func TestSettingsUpdateRejectsBodySuppliedTenant(t *testing.T) {
	stub := &settingsServiceStub{}
	handler := settingsHandlerFor(t, stub)
	principal := settingsPrincipal()
	response := httptest.NewRecorder()
	handler.UpdateAccessSettings(response, settingsRequest(t, http.MethodPut, "/settings/access",
		`{"public_origin":"https://example.com","allow_insecure_http_redirect":false,"version":1,"tenant_id":"tenant-attacker"}`, &principal))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a body-supplied tenant (%s)", response.Code, response.Body.String())
	}
	if stub.updateCalls != 0 {
		t.Fatal("rejected request must not reach the service")
	}
}
