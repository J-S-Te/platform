package application

import (
	"context"
	"testing"
)

// 联测：用真实的两份凭据服务 + 桩 OAuth 端口，跑通「需求表 → 服务凭据 + 浏览器凭据」。
// 这是在不接数据库/Agent 的前提下，对更新路径凭据准备最接近真实的验证。
type combinedOAuthPortStub struct {
	clients      []OAuthClientView
	repairs      int
	secrets      int
	listedTenant string
}

func (stub *combinedOAuthPortStub) ListOAuthClients(_ context.Context, tenantID string) ([]OAuthClientView, error) {
	stub.listedTenant = tenantID
	return stub.clients, nil
}

func (stub *combinedOAuthPortStub) ReplaceOAuthClientScopes(_ context.Context, input OAuthClientScopesUpdateInput) (OAuthClientView, error) {
	stub.repairs++
	for index := range stub.clients {
		if stub.clients[index].ID == input.OAuthClientID {
			stub.clients[index].Scopes = input.Scopes
			stub.clients[index].Version++
			return stub.clients[index], nil
		}
	}
	return OAuthClientView{}, ErrSubsystemUpdateServiceClientInvalid
}

func (stub *combinedOAuthPortStub) CreateOAuthClient(_ context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	stub.secrets++
	return OAuthClientCreateResult{Client: OAuthClientView{ID: input.ClientID, ClientID: input.ClientID}, PlaintextSecret: "secret-for-" + input.ClientID}, nil
}

func (stub *combinedOAuthPortStub) CreateOAuthClientSecret(_ context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	stub.secrets++
	return OAuthClientSecretResult{PlaintextSecret: "secret-for-" + input.OAuthClientID}, nil
}

func TestSubsystemUpdateCredentialsCombinedPreparation(t *testing.T) {
	requirements := SubsystemUpdateServiceCredentialRequirements("settlement")
	if len(requirements) == 0 {
		t.Fatal("settlement must declare service credential requirements")
	}
	clients := []OAuthClientView{{
		ID: "01CLI000000000000000000002", ClientID: "settlement-dev-web", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		Status: "ACTIVE", ClientType: "confidential", TokenAuthMethod: "client_secret_basic",
		GrantTypes: []string{"authorization_code", "refresh_token"}, Version: 1,
		RequirePKCE: true, RedirectURIs: []string{"https://settlement.example.com/settlement/auth/callback"},
	}}
	for _, requirement := range requirements {
		clients = append(clients, OAuthClientView{
			ID: "01CLI" + requirement.Suffix, ClientID: SubsystemUpdateServiceClientName("settlement", "dev", requirement.Suffix),
			TenantID: "tenant-1", ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
			Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic",
			GrantTypes: []string{"client_credentials"}, Scopes: SubsystemUpdateServiceCredentialScopes(requirement), Version: 1,
		})
	}
	port := &combinedOAuthPortStub{clients: clients}

	serviceCredentials, err := NewSubsystemUpdateServiceCredentialsService(port)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := serviceCredentials.Ensure(context.Background(), SubsystemUpdateServiceCredentialRequest{
		TenantID: "tenant-1", OperatorID: "operator-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		ApplicationCode: "settlement", Environment: "dev", Operation: "UPDATE", Requirements: requirements,
	})
	if err != nil {
		t.Fatalf("service credentials: %v", err)
	}
	if len(credentials) != len(requirements) {
		t.Fatalf("credentials = %d, want %d", len(credentials), len(requirements))
	}
	if port.repairs != 0 {
		t.Fatalf("no scope repair expected when scopes already match: %d", port.repairs)
	}
	// 普通更新只轮换需求表里标记 rotate 的用途（其余直接复用既有凭据）。
	expectedRotations := 0
	for _, requirement := range requirements {
		if requirement.Rotate {
			expectedRotations++
		}
	}
	if port.secrets != expectedRotations {
		t.Fatalf("plain UPDATE rotations = %d, want %d（按需求表 rotate 标记）", port.secrets, expectedRotations)
	}
	for _, credential := range credentials {
		if !requiresRotation(requirements, credential.Purpose) && credential.PlaintextSecret != "" {
			t.Fatalf("unmarked purpose must reuse the existing client without a new secret: %+v", credential)
		}
	}

	browserCredentials, err := NewSubsystemUpdateBrowserCredentialsService(port)
	if err != nil {
		t.Fatal(err)
	}
	browser, err := browserCredentials.Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "https://settlement.example.com/settlement/auth/callback")
	if err != nil {
		t.Fatalf("browser credentials: %v", err)
	}
	if browser.ClientID != "settlement-dev-web" || browser.ClientSecret == "" {
		t.Fatalf("browser credential = %+v", browser)
	}
	if port.secrets != expectedRotations+1 {
		t.Fatalf("browser rebuild must add exactly one secret: %d", port.secrets)
	}

	// ADOPT：服务凭据按规则全部轮换（每个用途一次签发）+ 浏览器一次。
	port.secrets = 0
	if _, err := serviceCredentials.Ensure(context.Background(), SubsystemUpdateServiceCredentialRequest{
		TenantID: "tenant-1", OperatorID: "operator-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		ApplicationCode: "settlement", Environment: "dev", Operation: "ADOPT", Requirements: requirements,
	}); err != nil {
		t.Fatalf("adopt service credentials: %v", err)
	}
	if port.secrets == 0 {
		t.Fatal("ADOPT must rotate service credentials")
	}
}

// requiresRotation 判断该用途在需求表中是否标记 rotate。
func requiresRotation(requirements []SubsystemUpdateServiceCredentialRequirement, purpose string) bool {
	for _, requirement := range requirements {
		if requirement.Purpose == purpose {
			return requirement.Rotate
		}
	}
	return false
}

// 文件网关用途的范围必须是完整三件套（防止回归为单范围而破坏上传/绑定/下载）。
func TestSubsystemUpdateServiceCredentialScopesGatewayComplete(t *testing.T) {
	for _, requirement := range SubsystemUpdateServiceCredentialRequirements("contract_management") {
		if requirement.Purpose != ServiceCredentialFileGatewayWrite {
			continue
		}
		scopes := SubsystemUpdateServiceCredentialScopes(requirement)
		for _, expected := range []string{"platform:file:upload", "platform:file:bind", "platform:file:download"} {
			found := false
			for _, scope := range scopes {
				if scope == expected {
					found = true
				}
			}
			if !found {
				t.Fatalf("file gateway scope %s missing in %v", expected, scopes)
			}
		}
	}
}
