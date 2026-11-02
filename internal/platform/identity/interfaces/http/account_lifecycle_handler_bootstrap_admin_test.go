package identityhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
)

// accountLifecycleServiceStub 只桩接账号生命周期用例；嵌入接口占位使未预期调用立即 panic，
// 避免用例在未察觉的情况下走过未桩接的路径。
type accountLifecycleServiceStub struct {
	accountLifecycleApplicationService

	createErr     error
	initializeErr error
	resetErr      error

	createCalls     int
	initializeCalls int
	resetCalls      int
}

func (stub *accountLifecycleServiceStub) CreateLocalAccount(context.Context, application.LocalAccountCreateInput) (domain.Account, error) {
	stub.createCalls++
	return domain.Account{ID: "acc-new", Status: domain.StatusActive}, stub.createErr
}

func (stub *accountLifecycleServiceStub) InitializePassword(context.Context, application.PasswordInitializeInput) (domain.Account, error) {
	stub.initializeCalls++
	return domain.Account{ID: "acc-existing", Status: domain.StatusActive}, stub.initializeErr
}

func (stub *accountLifecycleServiceStub) ResetPassword(context.Context, application.PasswordResetInput) (application.PasswordResetResult, error) {
	stub.resetCalls++
	return application.PasswordResetResult{AccountID: "acc-existing", TemporaryPassword: "Temp-pass-123!"}, stub.resetErr
}

func (stub *accountLifecycleServiceStub) ChangeOwnPassword(context.Context, application.PasswordChangeInput) error {
	return nil
}

// failingScopeAuthorizer 模拟管理范围解析失败（数据库不可用等），调用方必须失败关闭。
type failingScopeAuthorizer struct{ err error }

func (authorizer failingScopeAuthorizer) Resolve(context.Context, managementscope.Subject, string) (managementscope.Scope, error) {
	return managementscope.Scope{}, authorizer.err
}

func (failingScopeAuthorizer) ResourceContext(context.Context, string, string, string) (managementscope.ResourceContext, error) {
	return managementscope.ResourceContext{}, nil
}

func newAccountLifecycleTestHandler(t *testing.T, service *accountLifecycleServiceStub, authorizer managementscope.Authorizer) *AccountLifecycleHandler {
	t.Helper()
	handler, err := NewAccountLifecycleHandler(service, &Handler{}, slog.New(slog.NewTextHandler(io.Discard, nil)), authorizer)
	if err != nil {
		t.Fatalf("NewAccountLifecycleHandler() error = %v", err)
	}
	return handler
}

func accountLifecycleCreateRequest(t *testing.T, userID string) *http.Request {
	t.Helper()
	return userScopeRequest(t, http.MethodPost, "/api/v1/accounts", `{"user_id":"`+userID+`","account_name":"local-account","initial_password":"Initial-pass-123!"}`, "", "")
}

// 账号生命周期端点在写入任何凭据之前必须完成管理范围核验。
func TestAccountLifecycleWriteErrorMapsBootstrapAdministratorProtection(t *testing.T) {
	handler := newAccountLifecycleTestHandler(t, &accountLifecycleServiceStub{}, allowAllScopeAuthorizer{})
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "protected",
			err:        fmt.Errorf("create local account: %w", bootstrapadmin.ErrProtected),
			wantStatus: http.StatusConflict,
			wantCode:   "IAM_BOOTSTRAP_ADMIN_PROTECTED",
		},
		{
			name:       "configuration anomaly",
			err:        fmt.Errorf("resolve bootstrap administrator: %w", bootstrapadmin.ErrConfiguration),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID",
		},
		{
			name:       "ordinary conflict stays a plain conflict",
			err:        fmt.Errorf("ordinary: %w", application.ErrConflict),
			wantStatus: http.StatusConflict,
			wantCode:   "IAM_CONFLICT",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil)
			recorder := httptest.NewRecorder()
			handler.writeError(recorder, request, testCase.err)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if envelope := decodeErrorEnvelope(t, recorder); envelope.Code != testCase.wantCode {
				t.Fatalf("code = %q, want %q", envelope.Code, testCase.wantCode)
			}
		})
	}
}

// 为部署初始化超管用户追加本地账号必须被拒绝，并映射为 409。
func TestCreateLocalAccountRejectsBootstrapAdministratorUser(t *testing.T) {
	service := &accountLifecycleServiceStub{createErr: fmt.Errorf("create local account: %w", bootstrapadmin.ErrProtected)}
	handler := newAccountLifecycleTestHandler(t, service, allowAllScopeAuthorizer{})
	recorder := httptest.NewRecorder()

	handler.CreateLocalAccount(recorder, accountLifecycleCreateRequest(t, "user-bootstrap"))

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if envelope := decodeErrorEnvelope(t, recorder); envelope.Code != "IAM_BOOTSTRAP_ADMIN_PROTECTED" {
		t.Fatalf("code = %q, want IAM_BOOTSTRAP_ADMIN_PROTECTED", envelope.Code)
	}
	if service.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", service.createCalls)
	}
}

// 初始化与重置部署初始化超管账号的密码必须被拒绝，并映射为 409。
func TestAdministratorPasswordRejectsBootstrapAdministratorAccount(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		body    string
		invoke  func(*AccountLifecycleHandler, http.ResponseWriter, *http.Request)
		callsOf func(*accountLifecycleServiceStub) int
	}{
		{
			name:   "initialize",
			path:   "/api/v1/accounts/acc-bootstrap/password/initialize",
			body:   `{"password":"Initial-pass-123!","version":1}`,
			invoke: (*AccountLifecycleHandler).InitializePassword,
			callsOf: func(stub *accountLifecycleServiceStub) int {
				return stub.initializeCalls
			},
		},
		{
			name:   "reset",
			path:   "/api/v1/accounts/acc-bootstrap/password/reset",
			body:   `{"version":1}`,
			invoke: (*AccountLifecycleHandler).ResetPassword,
			callsOf: func(stub *accountLifecycleServiceStub) int {
				return stub.resetCalls
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &accountLifecycleServiceStub{
				initializeErr: fmt.Errorf("initialize password: %w", bootstrapadmin.ErrProtected),
				resetErr:      fmt.Errorf("reset password: %w", bootstrapadmin.ErrProtected),
			}
			handler := newAccountLifecycleTestHandler(t, service, allowAllScopeAuthorizer{})
			recorder := httptest.NewRecorder()

			testCase.invoke(handler, recorder, userScopeRequest(t, http.MethodPost, testCase.path, testCase.body, "account_id", "acc-bootstrap"))

			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
			}
			if envelope := decodeErrorEnvelope(t, recorder); envelope.Code != "IAM_BOOTSTRAP_ADMIN_PROTECTED" {
				t.Fatalf("code = %q, want IAM_BOOTSTRAP_ADMIN_PROTECTED", envelope.Code)
			}
			if calls := testCase.callsOf(service); calls != 1 {
				t.Fatalf("service calls = %d, want 1", calls)
			}
		})
	}
}

// 受限管理范围在当前 interim fail-closed 阶段整体拒绝，且不触达应用服务。
func TestAccountLifecycleRejectsRestrictedManagementScope(t *testing.T) {
	restricted := scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:account:create":              organizationScope("org-a"),
		"platform:account:password-initialize": organizationScope("org-a"),
		"platform:account:password-reset":      organizationScope("org-a"),
	}}
	cases := []struct {
		name   string
		path   string
		body   string
		invoke func(*AccountLifecycleHandler, http.ResponseWriter, *http.Request)
	}{
		{"create", "/api/v1/accounts", `{"user_id":"user-1","account_name":"local-account","initial_password":"Initial-pass-123!"}`, (*AccountLifecycleHandler).CreateLocalAccount},
		{"initialize", "/api/v1/accounts/acc-1/password/initialize", `{"password":"Initial-pass-123!","version":1}`, (*AccountLifecycleHandler).InitializePassword},
		{"reset", "/api/v1/accounts/acc-1/password/reset", `{"version":1}`, (*AccountLifecycleHandler).ResetPassword},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := &accountLifecycleServiceStub{}
			handler := newAccountLifecycleTestHandler(t, service, restricted)
			recorder := httptest.NewRecorder()

			testCase.invoke(handler, recorder, userScopeRequest(t, http.MethodPost, testCase.path, testCase.body, "account_id", "acc-1"))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
			}
			if service.createCalls+service.initializeCalls+service.resetCalls != 0 {
				t.Fatalf("restricted scope must not reach the service: %#v", service)
			}
		})
	}
}

// 没有任何绑定隐含权限（空范围）时同样拒绝。
func TestAccountLifecycleRejectsEmptyManagementScope(t *testing.T) {
	service := &accountLifecycleServiceStub{}
	handler := newAccountLifecycleTestHandler(t, service, scopedAuthorizer{})
	recorder := httptest.NewRecorder()

	handler.CreateLocalAccount(recorder, accountLifecycleCreateRequest(t, "user-1"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if service.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", service.createCalls)
	}
}

// 管理范围解析失败时必须失败关闭为 500，绝不放行。
func TestAccountLifecycleFailsClosedWhenScopeResolutionFails(t *testing.T) {
	service := &accountLifecycleServiceStub{}
	handler := newAccountLifecycleTestHandler(t, service, failingScopeAuthorizer{err: errors.New("management scope database unavailable")})
	recorder := httptest.NewRecorder()

	handler.ResetPassword(recorder, userScopeRequest(t, http.MethodPost, "/api/v1/accounts/acc-1/password/reset", `{"version":1}`, "account_id", "acc-1"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if service.resetCalls != 0 {
		t.Fatalf("reset calls = %d, want 0", service.resetCalls)
	}
}

// 不受限范围保持既有行为：创建 201、重置 200 并只返回一次性临时密码。
func TestAccountLifecycleAllowsUnrestrictedManagementScope(t *testing.T) {
	service := &accountLifecycleServiceStub{}
	handler := newAccountLifecycleTestHandler(t, service, allowAllScopeAuthorizer{})

	createRecorder := httptest.NewRecorder()
	handler.CreateLocalAccount(createRecorder, accountLifecycleCreateRequest(t, "user-1"))
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body=%s", createRecorder.Code, http.StatusCreated, createRecorder.Body.String())
	}

	resetRecorder := httptest.NewRecorder()
	handler.ResetPassword(resetRecorder, userScopeRequest(t, http.MethodPost, "/api/v1/accounts/acc-1/password/reset", `{"version":1}`, "account_id", "acc-1"))
	if resetRecorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d; body=%s", resetRecorder.Code, http.StatusOK, resetRecorder.Body.String())
	}
}
