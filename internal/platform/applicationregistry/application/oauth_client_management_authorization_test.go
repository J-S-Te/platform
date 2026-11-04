package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// 安全审查 H-1：OAuth 客户端创建/改 scope 必须复核应用归属，否则持
// platform:oauth-client:create 的安全管理员可以为控制面应用登记客户端，再用
// authorization.catalog.sync 改写平台内置角色并取得任意 platform:* 权限。

func oauthClientPolicyContext(tenantID, userID string, roleCodes, permissionCodes []string) context.Context {
	roles := make([]authctx.ReferenceName, 0, len(roleCodes))
	for _, code := range roleCodes {
		roles = append(roles, authctx.ReferenceName{Code: code})
	}
	return authctx.WithPrincipal(context.Background(), authctx.Principal{
		Tenant:          authctx.ReferenceName{ID: tenantID},
		User:            authctx.ReferenceName{ID: userID},
		Roles:           roles,
		PermissionCodes: permissionCodes,
	})
}

func tenantApplication(ownerUserID string) OAuthClientParentApplication {
	return OAuthClientParentApplication{ID: "app-contract", Code: "contract_management", OwnerUserID: ownerUserID}
}

func platformApplication() OAuthClientParentApplication {
	return OAuthClientParentApplication{ID: "app-platform", Code: builtInApplicationCode, OwnerUserID: "operator-1"}
}

func TestAuthorizeOAuthClientMutationEnforcesApplicationOwnership(t *testing.T) {
	cases := []struct {
		name       string
		ctx        context.Context
		operatorID string
		parent     OAuthClientParentApplication
		scopes     []string
		want       error
	}{
		{
			name:       "platform application rejects security administrator holding every registry permission",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-security-admin"}, []string{PermissionApplicationUpdate, PermissionOAuthClientCreate}),
			parent:     platformApplication(),
			operatorID: "operator-1",
			want:       ErrManagementForbidden,
		},
		{
			name:       "platform application accepts platform super administrator",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-super-admin"}, nil),
			parent:     platformApplication(),
			operatorID: "operator-1",
			want:       nil,
		},
		{
			name:       "platform application rejects reserved catalog sync scope even for super administrator",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-super-admin"}, nil),
			parent:     platformApplication(),
			operatorID: "operator-1",
			scopes:     []string{"openid", OAuthClientReservedCatalogSyncScope},
			want:       ErrManagementValidation,
		},
		{
			name:       "ordinary application accepts its owner without extra permissions",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", nil, nil),
			parent:     tenantApplication("operator-1"),
			operatorID: "operator-1",
			scopes:     []string{"openid"},
			want:       nil,
		},
		{
			name:       "ordinary application accepts registry administrator with application update",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-security-admin"}, []string{PermissionApplicationUpdate}),
			parent:     tenantApplication("someone-else"),
			operatorID: "operator-1",
			want:       nil,
		},
		{
			name:       "ordinary application rejects unrelated operator without application update",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", nil, []string{PermissionOAuthClientCreate}),
			parent:     tenantApplication("someone-else"),
			operatorID: "operator-1",
			want:       ErrManagementForbidden,
		},
		{
			name:       "ordinary application keeps catalog publisher scope available to subsystems",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", nil, []string{PermissionApplicationUpdate}),
			parent:     tenantApplication("someone-else"),
			operatorID: "operator-1",
			scopes:     []string{OAuthClientReservedCatalogSyncScope},
			want:       nil,
		},
		{
			name:       "process internal caller without principal keeps existing system paths",
			ctx:        context.Background(),
			parent:     tenantApplication("someone-else"),
			operatorID: "system-keycloak",
			want:       nil,
		},
		{
			name:       "principal from another tenant is treated as a process internal caller",
			ctx:        oauthClientPolicyContext("tenant-2", "operator-1", nil, nil),
			parent:     tenantApplication("someone-else"),
			operatorID: "operator-1",
			want:       nil,
		},
		{
			// Keycloak Broker 以 system-keycloak 身份为平台应用登记 broker 客户端；
			// 这条管理员发起的程序化路径不能被控制台归属规则误伤。
			name:       "system broker operator keeps platform application client registration",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-security-admin"}, []string{PermissionApplicationUpdate}),
			parent:     platformApplication(),
			operatorID: "system-keycloak",
			want:       nil,
		},
		{
			// 控制台请求固定 OperatorID = principal.User.ID；主体与操作者不一致的
			// 请求只能来自进程内调用。
			name:       "mismatched authenticated principal is a process internal caller",
			ctx:        oauthClientPolicyContext("tenant-1", "operator-2", nil, nil),
			parent:     tenantApplication("someone-else"),
			operatorID: "operator-1",
			want:       nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := AuthorizeOAuthClientMutation(testCase.ctx, "tenant-1", testCase.operatorID, testCase.parent, testCase.scopes)
			switch {
			case testCase.want == nil && err != nil:
				t.Fatalf("expected authorization, got %v", err)
			case testCase.want != nil && !errors.Is(err, testCase.want):
				t.Fatalf("expected %v, got %v", testCase.want, err)
			}
		})
	}
}

func TestOAuthClientReservedCatalogSyncScopeIsStable(t *testing.T) {
	// 控制器面的目录同步 scope 由 OIDC 应用 Bearer 携带；常量漂移会让保留 scope
	// 校验静默失效。
	if OAuthClientReservedCatalogSyncScope != "authorization.catalog.sync" {
		t.Fatalf("reserved catalog sync scope drift: got %q", OAuthClientReservedCatalogSyncScope)
	}
}

type oauthClientAuthorizationClock struct{ now time.Time }

func (clock oauthClientAuthorizationClock) Now() time.Time { return clock.now }

type oauthClientAuthorizationIDs struct{}

func (oauthClientAuthorizationIDs) New(time.Time) (string, error) { return "generated-client-id", nil }

type oauthClientAuthorizationRepository struct {
	OAuthClientManagementRepository
	parent OAuthClientParentApplication
	client OAuthClientView
	// mutationErr 让 stub 在通过授权后模拟仓储写入结果；created/replaced 记录是否
	// 真的进入了写入路径。
	mutationErr  error
	created      bool
	replacedWith []string
}

func (repository *oauthClientAuthorizationRepository) GetOAuthClientParentApplication(context.Context, string, string) (OAuthClientParentApplication, error) {
	return repository.parent, nil
}

func (repository *oauthClientAuthorizationRepository) CreateOAuthClient(_ context.Context, _ OAuthClientCreateInput, oauthClientID string, _ *SecretWrite, _ time.Time) (OAuthClientView, error) {
	repository.created = true
	return OAuthClientView{ID: oauthClientID}, repository.mutationErr
}

func (repository *oauthClientAuthorizationRepository) GetOAuthClient(context.Context, string, string) (OAuthClientView, error) {
	return repository.client, nil
}

func (repository *oauthClientAuthorizationRepository) ReplaceOAuthClientScopes(_ context.Context, input OAuthClientScopesUpdateInput, _ time.Time) (OAuthClientView, error) {
	repository.replacedWith = append([]string(nil), input.Scopes...)
	return repository.client, repository.mutationErr
}

func newOAuthClientAuthorizationService(t *testing.T, repository *oauthClientAuthorizationRepository) *OAuthClientManagementService {
	t.Helper()
	service, err := NewOAuthClientManagementService(repository, oauthClientAuthorizationIDs{}, oauthClientAuthorizationClock{time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatalf("construct OAuth client management service: %v", err)
	}
	return service
}

func authorizedOAuthClientCreateInput(applicationID string) OAuthClientCreateInput {
	return OAuthClientCreateInput{
		TenantID: "tenant-1", ApplicationID: applicationID, EnvironmentID: "env-1", OperatorID: "operator-1",
		ClientID: "console-client", ClientName: "控制台客户端", ClientType: "public", TokenAuthMethod: "none",
		AccessTokenTTLSeconds: 900, RequirePKCE: true, GrantTypes: []string{"authorization_code"},
		Scopes: []string{"openid"}, RedirectURIs: []string{"https://console.example.com/callback"},
	}
}

func TestCreateOAuthClientServiceEnforcesApplicationOwnership(t *testing.T) {
	cases := []struct {
		name       string
		ctx        context.Context
		parent     OAuthClientParentApplication
		scopes     []string
		want       error
		wantCreate bool
	}{
		{
			name:   "platform application rejects non-super administrator before any write",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-security-admin"}, []string{PermissionApplicationUpdate, PermissionOAuthClientCreate}),
			parent: platformApplication(), want: ErrManagementForbidden,
		},
		{
			name:   "platform application accepts platform super administrator",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-super-admin"}, nil),
			parent: platformApplication(), wantCreate: true,
		},
		{
			name:   "ordinary application accepts its owner",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", nil, nil),
			parent: tenantApplication("operator-1"), wantCreate: true,
		},
		{
			name:   "ordinary application rejects unrelated operator",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", nil, []string{PermissionOAuthClientCreate}),
			parent: tenantApplication("someone-else"), want: ErrManagementForbidden,
		},
		{
			name:   "platform application rejects reserved catalog sync scope even for super administrator",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-super-admin"}, nil),
			parent: platformApplication(), scopes: []string{"openid", OAuthClientReservedCatalogSyncScope}, want: ErrManagementValidation,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := &oauthClientAuthorizationRepository{parent: testCase.parent}
			service := newOAuthClientAuthorizationService(t, repository)
			input := authorizedOAuthClientCreateInput(testCase.parent.ID)
			if testCase.scopes != nil {
				input.Scopes = testCase.scopes
			}
			_, err := service.CreateOAuthClient(testCase.ctx, input)
			switch {
			case testCase.want == nil && err != nil:
				t.Fatalf("expected creation, got %v", err)
			case testCase.want != nil && !errors.Is(err, testCase.want):
				t.Fatalf("expected %v, got %v", testCase.want, err)
			}
			if repository.created != testCase.wantCreate {
				t.Fatalf("repository write attempted = %v, want %v", repository.created, testCase.wantCreate)
			}
		})
	}
}

func TestReplaceOAuthClientScopesServiceRechecksParentApplication(t *testing.T) {
	cases := []struct {
		name         string
		ctx          context.Context
		parent       OAuthClientParentApplication
		scopes       []string
		want         error
		wantReplaced bool
	}{
		{
			name:   "platform application rejects non-super administrator",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-security-admin"}, []string{PermissionApplicationUpdate}),
			parent: platformApplication(), scopes: []string{"openid"}, want: ErrManagementForbidden,
		},
		{
			name:   "platform application rejects reserved catalog sync scope even for super administrator",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", []string{"platform-super-admin"}, nil),
			parent: platformApplication(), scopes: []string{"openid", OAuthClientReservedCatalogSyncScope}, want: ErrManagementValidation,
		},
		{
			name:   "ordinary application accepts its owner",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", nil, nil),
			parent: tenantApplication("operator-1"), scopes: []string{"openid"}, wantReplaced: true,
		},
		{
			name:   "ordinary application rejects unrelated operator",
			ctx:    oauthClientPolicyContext("tenant-1", "operator-1", nil, []string{PermissionOAuthClientCreate}),
			parent: tenantApplication("someone-else"), scopes: []string{"openid"}, want: ErrManagementForbidden,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository := &oauthClientAuthorizationRepository{parent: testCase.parent, client: OAuthClientView{ID: "client-1", ApplicationID: testCase.parent.ID}}
			service := newOAuthClientAuthorizationService(t, repository)
			_, err := service.ReplaceOAuthClientScopes(testCase.ctx, OAuthClientScopesUpdateInput{
				TenantID: "tenant-1", OAuthClientID: "client-1", OperatorID: "operator-1",
				Scopes: testCase.scopes, Version: 1,
			})
			switch {
			case testCase.want == nil && err != nil:
				t.Fatalf("expected scope replacement, got %v", err)
			case testCase.want != nil && !errors.Is(err, testCase.want):
				t.Fatalf("expected %v, got %v", testCase.want, err)
			}
			if (len(repository.replacedWith) > 0) != testCase.wantReplaced {
				t.Fatalf("repository scope write = %#v, want attempted=%v", repository.replacedWith, testCase.wantReplaced)
			}
		})
	}
}
