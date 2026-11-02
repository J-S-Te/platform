package identityhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/managementscope"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// authorizationOverviewStoreStub 提供确定性的只读总览数据；调用计数用于断言早退路径
// 没有触达后续查询。
type authorizationOverviewStoreStub struct {
	user         map[string]any
	userErr      error
	orgUnitIDs   []string
	orgUnitErr   error
	sections     authorizationOverviewSections
	sectionsErr  error
	identity     bootstrapadmin.Identity
	initialized  bool
	bootstrapErr error

	orgUnitCalls   int
	userCalls      int
	sectionsCalls  int
	bootstrapCalls int
}

func (stub *authorizationOverviewStoreStub) User(context.Context, string, string) (map[string]any, error) {
	stub.userCalls++
	return stub.user, stub.userErr
}

func (stub *authorizationOverviewStoreStub) ActiveMembershipOrgUnitIDs(context.Context, string, string) ([]string, error) {
	stub.orgUnitCalls++
	return stub.orgUnitIDs, stub.orgUnitErr
}

func (stub *authorizationOverviewStoreStub) Sections(context.Context, string, string) (authorizationOverviewSections, error) {
	stub.sectionsCalls++
	return stub.sections, stub.sectionsErr
}

func (stub *authorizationOverviewStoreStub) BootstrapAdministrator(context.Context, string) (bootstrapadmin.Identity, bool, error) {
	stub.bootstrapCalls++
	return stub.identity, stub.initialized, stub.bootstrapErr
}

func newAuthorizationOverviewTestHandler(store authorizationOverviewStore, authorizer managementscope.Authorizer) *AuthorizationOverviewHandler {
	return newAuthorizationOverviewHandlerWithStore(store, authorizer, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func authorizationOverviewRequest(t *testing.T, userID string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/people/"+userID+"/authorization-overview", nil)
	request.SetPathValue("user_id", userID)
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant:  authctx.ReferenceName{ID: "tenant-1"},
		User:    authctx.ReferenceName{ID: "operator-1"},
		Account: authctx.ReferenceName{ID: "account-operator"},
	}))
}

func decodeOverviewPayload(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode overview payload: %v; body=%s", err, recorder.Body.String())
	}
	return envelope.Data
}

// 总览必须只回显显式列投影：手机号密文/哈希、乐观锁版本与创建更新人不得出现。
func TestAuthorizationOverviewProjectionsExcludeSensitiveColumns(t *testing.T) {
	hasColumn := func(columns, name string) bool {
		pattern := `(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name) + `([^A-Za-z0-9_]|$)`
		return regexp.MustCompile(pattern).MatchString(columns)
	}
	cases := []struct {
		name      string
		columns   string
		required  []string
		forbidden []string
	}{
		{
			name:    "user",
			columns: overviewUserColumns,
			required: []string{
				"id", "display_name", "employee_no", "email", "status", "employment_status",
				"primary_org_id", "manager_user_id", "valid_until", "created_at", "updated_at",
			},
			forbidden: []string{"mobile_ciphertext", "mobile_hash", "version", "created_by", "updated_by", "legal_name"},
		},
		{
			name:    "account",
			columns: overviewAccountColumns,
			required: []string{
				"id", "user_id", "username", "account_type", "auth_source", "external_subject_id",
				"valid_until", "locked_until", "last_login_at", "status", "created_at", "updated_at",
				"password_initialized",
			},
			forbidden: []string{"mobile_ciphertext", "mobile_hash", "version", "created_by", "updated_by", "password_hash", "expiry_processed_at"},
		},
		{
			name:    "role binding",
			columns: overviewRoleBindingColumns,
			required: []string{
				"id", "role_id", "subject_type", "subject_id", "scope_type", "scope_id",
				"grant_origin", "valid_until", "status", "created_at",
			},
			forbidden: []string{"version", "created_by", "updated_by"},
		},
		{
			name:    "membership",
			columns: overviewMembershipColumns,
			required: []string{
				"id", "user_id", "org_unit_id", "position_id", "membership_type", "is_primary",
				"inherit_authorization", "valid_from", "valid_until", "status", "created_at", "updated_at",
			},
			forbidden: []string{"version", "created_by", "updated_by"},
		},
		{
			name:    "pending change",
			columns: overviewPendingChangeColumns,
			required: []string{
				"id", "user_id", "change_type", "status", "source_membership_id", "target_org_unit_id",
				"target_position_id", "effective_at", "submitted_by", "created_at", "updated_at",
			},
			forbidden: []string{"version"},
		},
		{
			name:    "handover",
			columns: overviewHandoverColumns,
			required: []string{
				"id", "request_id", "system_code", "resource_type", "resource_id",
				"current_owner_id", "target_owner_id", "status", "created_at", "updated_at",
			},
			forbidden: []string{"version"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, column := range testCase.required {
				if !hasColumn(testCase.columns, column) {
					t.Fatalf("%s projection is missing %q; columns=%s", testCase.name, column, testCase.columns)
				}
			}
			for _, column := range testCase.forbidden {
				if strings.Contains(testCase.columns, column) {
					t.Fatalf("%s projection leaks %q; columns=%s", testCase.name, column, testCase.columns)
				}
			}
		})
	}
}

// 空范围表示没有绑定隐含权限，必须 403 且不读取任何数据。
func TestAuthorizationOverviewRejectsEmptyManagementScope(t *testing.T) {
	store := &authorizationOverviewStoreStub{user: map[string]any{"id": "user-1"}}
	handler := newAuthorizationOverviewTestHandler(store, scopedAuthorizer{})
	recorder := httptest.NewRecorder()

	handler.Get(recorder, authorizationOverviewRequest(t, "user-1"))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if store.userCalls+store.orgUnitCalls+store.bootstrapCalls != 0 {
		t.Fatalf("forbidden request must not query data: %#v", store)
	}
}

// 受限范围下范围外用户与不存在用户都返回 403，不能用 404 泄露存在性。
func TestAuthorizationOverviewRejectsTargetOutsideManagementScope(t *testing.T) {
	cases := []struct {
		name  string
		store *authorizationOverviewStoreStub
	}{
		{name: "outside scope", store: &authorizationOverviewStoreStub{orgUnitIDs: []string{"org-b"}, user: map[string]any{"id": "user-outside"}}},
		{name: "no active membership", store: &authorizationOverviewStoreStub{orgUnitIDs: nil, user: map[string]any{"id": "user-outside"}}},
		{name: "unknown user", store: &authorizationOverviewStoreStub{userErr: application.ErrNotFound}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := newAuthorizationOverviewTestHandler(testCase.store, scopedAuthorizer{scopes: map[string]managementscope.Scope{
				"platform:user:read": organizationScope("org-a"),
			}})
			recorder := httptest.NewRecorder()

			handler.Get(recorder, authorizationOverviewRequest(t, "user-outside"))

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
			}
			if testCase.store.userCalls != 0 || testCase.store.sectionsCalls != 0 {
				t.Fatalf("out-of-scope request must not read user data: %#v", testCase.store)
			}
		})
	}
}

// 受限范围内用户在允许组织内时正常返回，并带上显式投影的分节。
func TestAuthorizationOverviewAllowsTargetInsideManagementScope(t *testing.T) {
	store := &authorizationOverviewStoreStub{
		orgUnitIDs: []string{"org-a"},
		user:       map[string]any{"id": "user-1", "display_name": "范围内用户"},
		sections: authorizationOverviewSections{
			Accounts: []map[string]any{{"id": "acc-1", "username": "local", "password_initialized": true}},
		},
	}
	handler := newAuthorizationOverviewTestHandler(store, scopedAuthorizer{scopes: map[string]managementscope.Scope{
		"platform:user:read": organizationScope("org-a"),
	}})
	recorder := httptest.NewRecorder()

	handler.Get(recorder, authorizationOverviewRequest(t, "user-1"))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	payload := decodeOverviewPayload(t, recorder)
	if payload["user"] == nil || payload["accounts"] == nil {
		t.Fatalf("overview payload = %#v", payload)
	}
	for _, key := range []string{"memberships", "role_bindings", "pending_changes", "handover", "keycloak_sync"} {
		rows, ok := payload[key].([]any)
		if !ok || len(rows) != 0 {
			t.Fatalf("%s = %#v, want empty array", key, payload[key])
		}
	}
}

// 部署初始化超管按 ListUsers 同口径隐藏：即使范围内也返回 404，且不读取其数据。
func TestAuthorizationOverviewHidesBootstrapAdministrator(t *testing.T) {
	store := &authorizationOverviewStoreStub{
		identity:    bootstrapadmin.Identity{TenantID: "tenant-1", UserID: "user-bootstrap", AccountID: "acc-bootstrap"},
		initialized: true,
		user:        map[string]any{"id": "user-bootstrap"},
	}
	handler := newAuthorizationOverviewTestHandler(store, allowAllScopeAuthorizer{})
	recorder := httptest.NewRecorder()

	handler.Get(recorder, authorizationOverviewRequest(t, "user-bootstrap"))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if store.userCalls != 0 || store.sectionsCalls != 0 {
		t.Fatalf("protected user must not be read: %#v", store)
	}
}

// 初始化记录异常必须失败关闭为 500，而不是退回普通用户口径。
func TestAuthorizationOverviewFailsClosedOnBootstrapConfigurationError(t *testing.T) {
	store := &authorizationOverviewStoreStub{bootstrapErr: bootstrapadmin.ErrConfiguration}
	handler := newAuthorizationOverviewTestHandler(store, allowAllScopeAuthorizer{})
	recorder := httptest.NewRecorder()

	handler.Get(recorder, authorizationOverviewRequest(t, "user-1"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if envelope := decodeErrorEnvelope(t, recorder); envelope.Code != "IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID" {
		t.Fatalf("code = %q, want IAM_BOOTSTRAP_ADMIN_CONFIGURATION_INVALID", envelope.Code)
	}
}

// 管理范围解析失败同样失败关闭为 500。
func TestAuthorizationOverviewFailsClosedWhenScopeResolutionFails(t *testing.T) {
	store := &authorizationOverviewStoreStub{}
	handler := newAuthorizationOverviewTestHandler(store, failingScopeAuthorizer{err: errors.New("management scope database unavailable")})
	recorder := httptest.NewRecorder()

	handler.Get(recorder, authorizationOverviewRequest(t, "user-1"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if store.userCalls != 0 {
		t.Fatalf("failed scope resolution must not read user data: %#v", store)
	}
}

// 目标用户不存在时（不受限范围）保持既有 404 语义。
func TestAuthorizationOverviewReturnsNotFoundForUnknownUser(t *testing.T) {
	store := &authorizationOverviewStoreStub{userErr: application.ErrNotFound}
	handler := newAuthorizationOverviewTestHandler(store, allowAllScopeAuthorizer{})
	recorder := httptest.NewRecorder()

	handler.Get(recorder, authorizationOverviewRequest(t, "user-missing"))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
	if store.sectionsCalls != 0 {
		t.Fatalf("missing user must not read sections: %#v", store)
	}
}
