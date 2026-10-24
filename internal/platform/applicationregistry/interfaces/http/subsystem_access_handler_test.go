package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

type subsystemAccessServiceStub struct {
	preview     application.SubsystemAccessPreview
	previewErr  error
	submit      application.SubsystemAccessSubmitResult
	submitErr   error
	view        application.SubsystemAccessOperationView
	viewErr     error
	found       bool
	submitCalls int
	resumeCalls int
	submitSeen  application.SubsystemAccessSubmitRequest
	previewSeen []string
}

func (stub *subsystemAccessServiceStub) Preview(_ context.Context, _, _, _ string, permissions []string) (application.SubsystemAccessPreview, error) {
	stub.previewSeen = permissions
	return stub.preview, stub.previewErr
}

func (stub *subsystemAccessServiceStub) Submit(_ context.Context, request application.SubsystemAccessSubmitRequest) (application.SubsystemAccessSubmitResult, error) {
	stub.submitCalls++
	stub.submitSeen = request
	return stub.submit, stub.submitErr
}

func (stub *subsystemAccessServiceStub) Resume(context.Context, string, string) (application.SubsystemAccessOperationView, error) {
	stub.resumeCalls++
	return stub.view, stub.viewErr
}

func (stub *subsystemAccessServiceStub) Describe(context.Context, string, string) (application.SubsystemAccessOperationView, error) {
	return stub.view, stub.viewErr
}

func (stub *subsystemAccessServiceStub) FindByTarget(context.Context, string, string, string) (application.SubsystemAccessOperationView, bool, error) {
	return stub.view, stub.found, stub.viewErr
}

func subsystemAccessHandlerFor(t *testing.T, stub *subsystemAccessServiceStub) *SubsystemAccessHandler {
	t.Helper()
	handler, err := NewSubsystemAccessHandler(stub, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func subsystemAccessRequest(t *testing.T, method, target, body string, principal *authctx.Principal) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if principal != nil {
		request = request.WithContext(authctx.WithPrincipal(request.Context(), *principal))
	}
	return request
}

func accessPrincipal(permissions ...string) authctx.Principal {
	return authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "operator-1"},
		PermissionCodes: append([]string{application.PermissionApplicationRead}, permissions...),
	}
}

func hasPermission(principal authctx.Principal, permission string) bool {
	for _, held := range principal.PermissionCodes {
		if held == permission {
			return true
		}
	}
	return false
}

// 未认证：一律 401，且不触达服务层。
func TestSubsystemAccessHandlerRequiresPrincipal(t *testing.T) {
	stub := &subsystemAccessServiceStub{}
	handler := subsystemAccessHandlerFor(t, stub)
	for name, call := range map[string]http.HandlerFunc{
		"preview": handler.Preview, "submit": handler.Submit, "get": handler.Get, "find": handler.FindByTarget, "resume": handler.Resume,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			call(response, subsystemAccessRequest(t, http.MethodGet, "/api/v1/subsystem-access/preview", "{}", nil))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
	if stub.submitCalls != 0 || stub.resumeCalls != 0 {
		t.Fatal("unauthenticated requests must not reach the service")
	}
}

// 预览：返回服务端判定的动作、缺项、所需权限与预览版本。
func TestSubsystemAccessHandlerPreviewReturnsServerDecision(t *testing.T) {
	stub := &subsystemAccessServiceStub{preview: application.SubsystemAccessPreview{
		Decision:  application.SubsystemAccessDecision{Action: application.SubsystemAccessActionAdopt, Reason: "已登记但没有受控部署记录"},
		NextStage: application.SubsystemAccessStageAuth, PreviewVersion: "pv-3",
		Missing:       []application.SubsystemAccessMissing{{Code: "RUNTIME_APPROVAL_MISSING", Message: "缺少运行批准", NextAction: "生成批准候选"}},
		RequiredPerms: application.SubsystemAccessRequiredPermissions(application.SubsystemAccessActionAdopt),
		MissingPerms:  []string{application.PermissionOAuthClientDisable},
	}}
	handler := subsystemAccessHandlerFor(t, stub)
	principal := accessPrincipal(application.PermissionApplicationUpdate)
	response := httptest.NewRecorder()
	handler.Preview(response, subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/preview",
		`{"application_code":"settlement","environment":"dev"}`, &principal))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{"ADOPT", "pv-3", "RUNTIME_APPROVAL_MISSING", "required_permissions", "missing_permissions"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("preview body missing %s: %s", expected, body)
		}
	}
	// 主体权限被透传给服务层（用于计算"缺哪些权限"）。
	if len(stub.previewSeen) == 0 || !hasPermission(authctx.Principal{PermissionCodes: stub.previewSeen}, application.PermissionApplicationUpdate) {
		t.Fatalf("operator permissions not forwarded: %v", stub.previewSeen)
	}
}

// 提交：缺少动作所需任一权限即 403，且不得调用服务层提交。
func TestSubsystemAccessHandlerSubmitEnforcesActionPermissions(t *testing.T) {
	stub := &subsystemAccessServiceStub{preview: application.SubsystemAccessPreview{
		Decision: application.SubsystemAccessDecision{Action: application.SubsystemAccessActionOnboard}, PreviewVersion: "pv-1"}}
	handler := subsystemAccessHandlerFor(t, stub)
	// 只持有 application:create —— 首次接入还需要 environment/login-target/oauth-client/role-binding。
	principal := accessPrincipal(application.PermissionApplicationCreate)
	response := httptest.NewRecorder()
	handler.Submit(response, subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/operations",
		`{"application_code":"contract","environment":"dev","preview_version":"pv-1","idempotency_key":"idem-1"}`, &principal))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", response.Code, response.Body.String())
	}
	if stub.submitCalls != 0 {
		t.Fatal("submit must not run when the action permissions are missing")
	}

	// 持有全部所需权限 → 202 且租户/操作者来自主体。
	full := accessPrincipal(application.PermissionApplicationCreate, application.PermissionApplicationEnvironmentCreate,
		application.PermissionLoginTargetCreate, application.PermissionOAuthClientCreate, application.PermissionRoleBindingUpdate)
	stub.submit = application.SubsystemAccessSubmitResult{Created: true, Operation: application.SubsystemAccessOperation{
		ID: "01SAOP000000000000000000001", Action: application.SubsystemAccessActionOnboard,
		Stage: application.SubsystemAccessStageCheck, Status: application.SubsystemAccessStatusPending}}
	response = httptest.NewRecorder()
	handler.Submit(response, subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/operations",
		`{"application_code":"contract","environment":"dev","preview_version":"pv-1","idempotency_key":"idem-1","public_origin":"https://contract.example.com"}`, &full))
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), "01SAOP000000000000000000001") {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if stub.submitSeen.TenantID != "tenant-1" || stub.submitSeen.OperatorID != "operator-1" || stub.submitSeen.PublicOrigin != "https://contract.example.com" {
		t.Fatalf("submit request = %+v", stub.submitSeen)
	}
}

// 已一致 → 200 且标记 display_only；冲突 / 不存在 / 未知错误映射为确定状态码。
func TestSubsystemAccessHandlerMapsResultsAndErrors(t *testing.T) {
	handler := subsystemAccessHandlerFor(t, &subsystemAccessServiceStub{
		preview: application.SubsystemAccessPreview{Decision: application.SubsystemAccessDecision{Action: application.SubsystemAccessActionNone, DisplayOnly: true}},
		submit:  application.SubsystemAccessSubmitResult{DisplayOnly: true, Decision: application.SubsystemAccessDecision{Action: application.SubsystemAccessActionNone, Reason: "已接入且配置一致"}},
	})
	principal := accessPrincipal()
	response := httptest.NewRecorder()
	handler.Submit(response, subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/operations",
		`{"application_code":"contract","environment":"dev","preview_version":"pv-1","idempotency_key":"idem-1"}`, &principal))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "display_only") {
		t.Fatalf("display-only status = %d body = %s", response.Code, response.Body.String())
	}

	cases := map[string]struct {
		err  error
		want int
	}{
		"conflict":   {application.ErrSubsystemAccessOperationConflict, http.StatusConflict},
		"not found":  {application.ErrSubsystemAccessOperationNotFound, http.StatusNotFound},
		"validation": {application.ErrSubsystemAccessInvalidRequest, http.StatusUnprocessableEntity},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			handler := subsystemAccessHandlerFor(t, &subsystemAccessServiceStub{viewErr: testCase.err})
			response := httptest.NewRecorder()
			handler.Get(response, subsystemAccessRequest(t, http.MethodGet, "/api/v1/subsystem-access/operations/op-1", "", &principal))
			if response.Code != testCase.want {
				t.Fatalf("status = %d, want %d", response.Code, testCase.want)
			}
		})
	}

	// 未知字段必须被拒（不静默忽略）。
	response = httptest.NewRecorder()
	handler.Preview(response, subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/preview",
		`{"application_code":"contract","environment":"dev","tenant_id":"tenant-attacker"}`, &principal))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field status = %d, want 422", response.Code)
	}
}

// 目标无操作 → 404；恢复权限同样按该操作的动作校验。
func TestSubsystemAccessHandlerFindAndResume(t *testing.T) {
	stub := &subsystemAccessServiceStub{found: false}
	handler := subsystemAccessHandlerFor(t, stub)
	principal := accessPrincipal()
	response := httptest.NewRecorder()
	handler.FindByTarget(response, subsystemAccessRequest(t, http.MethodGet, "/api/v1/subsystem-access/operations?application_code=contract&environment=dev", "", &principal))
	if response.Code != http.StatusNotFound {
		t.Fatalf("find status = %d, want 404", response.Code)
	}

	// 恢复：操作动作是 UPDATE，主体缺权限 → 403 且不调用 Resume。
	stub = &subsystemAccessServiceStub{view: application.SubsystemAccessOperationView{Operation: application.SubsystemAccessOperation{
		ID: "01SAOP000000000000000000001", Action: application.SubsystemAccessActionUpdate, Status: application.SubsystemAccessStatusWaiting}}}
	handler = subsystemAccessHandlerFor(t, stub)
	response = httptest.NewRecorder()
	request := subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/operations/01SAOP000000000000000000001/resume", "", &principal)
	request.SetPathValue("operation_id", "01SAOP000000000000000000001")
	handler.Resume(response, request)
	if response.Code != http.StatusForbidden || stub.resumeCalls != 0 {
		t.Fatalf("resume status = %d calls = %d", response.Code, stub.resumeCalls)
	}

	full := accessPrincipal(application.PermissionApplicationUpdate, application.PermissionApplicationEnvironmentUpdate,
		application.PermissionLoginTargetUpdate, application.PermissionOAuthClientDisable)
	response = httptest.NewRecorder()
	request = subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/operations/01SAOP000000000000000000001/resume", "", &full)
	request.SetPathValue("operation_id", "01SAOP000000000000000000001")
	handler.Resume(response, request)
	if response.Code != http.StatusOK || stub.resumeCalls != 1 {
		t.Fatalf("resume status = %d calls = %d body=%s", response.Code, stub.resumeCalls, response.Body.String())
	}
}

// 预览响应必须显式暴露执行路径；接管/更新/重试现在都走统一入口，不再给 CLI 提示。
func TestSubsystemAccessHandlerPreviewExposesExecutionPath(t *testing.T) {
	stub := &subsystemAccessServiceStub{preview: application.SubsystemAccessPreview{
		Decision:       application.SubsystemAccessDecision{Action: application.SubsystemAccessActionUpdate, Reason: "已接入且无漂移"},
		NextStage:      application.SubsystemAccessStageDeploy,
		PreviewVersion: "pv-9",
		ExecutionPath:  application.SubsystemAccessExecutionUnified,
	}}
	handler := subsystemAccessHandlerFor(t, stub)
	principal := accessPrincipal(application.PermissionApplicationUpdate)
	response := httptest.NewRecorder()
	handler.Preview(response, subsystemAccessRequest(t, http.MethodPost, "/api/v1/subsystem-access/preview",
		`{"application_code":"settlement","environment":"dev"}`, &principal))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "execution_path") || !strings.Contains(body, "unified") {
		t.Fatalf("preview body missing execution path: %s", body)
	}
	// 四类动作都走统一入口后，预览不应再把用户引回 CLI。
	if strings.Contains(body, "subsystem.sh update") {
		t.Fatalf("preview still advertises legacy CLI: %s", body)
	}
}
