package application

import (
	"errors"
	"testing"
)

// 浏览器客户端规范名与期望：编码/环境归一化；范围不约束（随授权目录变化）。
func TestSubsystemUpdateBrowserClientExpectation(t *testing.T) {
	if got := SubsystemUpdateBrowserClientName(" Settlement ", " DEV "); got != "settlement-dev-web" {
		t.Fatalf("browser client name = %s", got)
	}
	expectation := SubsystemUpdateBrowserClientExpectation("tenant-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "https://settlement.example.com/auth/callback")
	if expectation.CanonicalName != "settlement-dev-web" || expectation.ClientType != "confidential" {
		t.Fatalf("expectation = %+v", expectation)
	}
	if len(expectation.Scopes) != 0 {
		t.Fatalf("browser client scopes must not be constrained here: %+v", expectation.Scopes)
	}
	client := OAuthClientView{
		ID: "01CLI000000000000000000002", ClientID: "settlement-dev-web", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		Status: "ACTIVE", ClientType: "confidential", TokenAuthMethod: "client_secret_basic",
		GrantTypes: []string{"refresh_token", "authorization_code"}, Scopes: []string{"openid", "profile"},
		RequirePKCE: true, RedirectURIs: []string{"https://settlement.example.com/auth/callback"},
	}
	if err := ValidateSubsystemUpdateServiceClient(client, expectation); err != nil {
		t.Fatalf("valid browser client must pass: %v", err)
	}
	// 授权类型不一致（缺少 refresh_token）必须被拒。
	client.GrantTypes = []string{"authorization_code"}
	if err := ValidateSubsystemUpdateServiceClient(client, expectation); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("grant type drift err = %v", err)
	}
}
