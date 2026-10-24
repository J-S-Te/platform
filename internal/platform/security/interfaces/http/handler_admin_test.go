package securityhttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	securityapplication "github.com/J-S-Te/Basic-Platform/internal/platform/security/application"
	securitydomain "github.com/J-S-Te/Basic-Platform/internal/platform/security/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// securityServiceStub 记录调用参数并返回预设结果。
type securityServiceStub struct {
	policy       securitydomain.LoginPolicy
	policyErr    error
	updateErr    error
	unlockErr    error
	unlockInput  securityapplication.UnlockInput
	policyTenant string
	policyCalls  int
}

func (stub *securityServiceStub) GetLoginPolicy(_ context.Context, tenantID string) (securitydomain.LoginPolicy, error) {
	stub.policyTenant = tenantID
	stub.policyCalls++
	if stub.policyErr != nil {
		return securitydomain.LoginPolicy{}, stub.policyErr
	}
	return stub.policy, nil
}

func (stub *securityServiceStub) UpdateLoginPolicy(context.Context, securityapplication.LoginPolicyUpdateInput) (securitydomain.LoginPolicy, error) {
	if stub.updateErr != nil {
		return securitydomain.LoginPolicy{}, stub.updateErr
	}
	return stub.policy, nil
}

func (stub *securityServiceStub) ListLockedAccounts(context.Context, string, securityapplication.PageRequest) (securityapplication.PageResult[securitydomain.LockedAccount], error) {
	return securityapplication.PageResult[securitydomain.LockedAccount]{}, nil
}

func (stub *securityServiceStub) UnlockAccount(_ context.Context, input securityapplication.UnlockInput) (securitydomain.LockedAccount, error) {
	stub.unlockInput = input
	if stub.unlockErr != nil {
		return securitydomain.LockedAccount{}, stub.unlockErr
	}
	return securitydomain.LockedAccount{AccountID: input.AccountID}, nil
}

func securityHandlerFor(t *testing.T, stub *securityServiceStub) *Handler {
	t.Helper()
	handler, err := NewHandler(stub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func securityRequest(t *testing.T, method, target, body string, principal *authctx.Principal) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if principal != nil {
		request = request.WithContext(authctx.WithPrincipal(request.Context(), *principal))
	}
	return request
}

func securityPrincipal() authctx.Principal {
	return authctx.Principal{Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "admin-1"}}
}

// 安全设置端点必须要求已认证主体，未认证不得触达服务。
func TestSecurityEndpointsRequirePrincipal(t *testing.T) {
	stub := &securityServiceStub{}
	handler := securityHandlerFor(t, stub)
	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"getPolicy":    handler.GetLoginPolicy,
		"updatePolicy": handler.UpdateLoginPolicy,
		"listLocked":   handler.ListLockedAccounts,
		"unlock":       handler.UnlockAccount,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			call(response, securityRequest(t, http.MethodGet, "/security/login-policy", `{}`, nil))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
	if stub.policyCalls != 0 || stub.unlockInput.AccountID != "" {
		t.Fatal("unauthenticated request must not reach the service")
	}
}

// 登录策略按认证主体的租户读取。
func TestSecurityLoginPolicyBindsTenantFromPrincipal(t *testing.T) {
	stub := &securityServiceStub{policy: securitydomain.LoginPolicy{TenantID: "tenant-1"}}
	handler := securityHandlerFor(t, stub)
	principal := securityPrincipal()
	response := httptest.NewRecorder()
	handler.GetLoginPolicy(response, securityRequest(t, http.MethodGet, "/security/login-policy", "", &principal))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	if stub.policyTenant != "tenant-1" {
		t.Fatalf("service saw tenant %q", stub.policyTenant)
	}
}

// 解锁必须使用路径中的账号标识与认证主体的租户，而不是请求体。
func TestSecurityUnlockUsesPathAccountAndPrincipalTenant(t *testing.T) {
	stub := &securityServiceStub{}
	handler := securityHandlerFor(t, stub)
	principal := securityPrincipal()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /security/locked-accounts/{account_id}/unlock", handler.UnlockAccount)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, securityRequest(t, http.MethodPost, "/security/locked-accounts/acc-9/unlock", `{"tenant_id":"tenant-attacker"}`, &principal))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	if stub.unlockInput.AccountID != "acc-9" || stub.unlockInput.TenantID != "tenant-1" || stub.unlockInput.OperatorID != "admin-1" {
		t.Fatalf("unlock input = %+v", stub.unlockInput)
	}
}

// 领域错误与内部错误必须映射为确定性状态码。
func TestSecurityLoginPolicyMapsErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"validation": {securityapplication.ErrValidation, http.StatusUnprocessableEntity},
		"not found":  {securityapplication.ErrNotFound, http.StatusNotFound},
		"internal":   {errors.New("policy store down"), http.StatusInternalServerError},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &securityServiceStub{updateErr: testCase.err}
			handler := securityHandlerFor(t, stub)
			principal := securityPrincipal()
			response := httptest.NewRecorder()
			handler.UpdateLoginPolicy(response, securityRequest(t, http.MethodPut, "/security/login-policy",
				`{"max_failed_attempts":5,"lockout_duration_seconds":900,"failure_reset_window_seconds":1800,"idle_timeout_seconds":600,"version":1}`, &principal))
			if response.Code != testCase.want {
				t.Fatalf("status = %d, want %d (%s)", response.Code, testCase.want, response.Body.String())
			}
		})
	}
}
