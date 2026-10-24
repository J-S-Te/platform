package application

import (
	"errors"
	"testing"
)

func updateClientExpectation() SubsystemUpdateServiceClientExpectation {
	return SubsystemUpdateServiceClientExpectation{
		CanonicalName: "settlement-dev-audit", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		ClientType: "service", TokenAuthMethod: "client_secret_basic",
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"settlement.contract.ingest"},
	}
}

func updateClientFixture() OAuthClientView {
	return OAuthClientView{
		ID: "01CLI000000000000000000001", ClientID: "settlement-dev-audit", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic",
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"settlement.contract.ingest"},
	}
}

func TestValidateSubsystemUpdateServiceClientAcceptsCanonical(t *testing.T) {
	if err := ValidateSubsystemUpdateServiceClient(updateClientFixture(), updateClientExpectation()); err != nil {
		t.Fatalf("canonical client must pass: %v", err)
	}
	// 与既有实现一致：不做小写归一化（环境值由上层保证规范形式）。
	if got := SubsystemUpdateServiceClientName("settlement", "dev", "audit"); got != "settlement-dev-audit" {
		t.Fatalf("canonical name = %s", got)
	}
}

func TestValidateSubsystemUpdateServiceClientRejectsDrift(t *testing.T) {
	mutations := map[string]func(*OAuthClientView){
		"name-drift":  func(c *OAuthClientView) { c.ClientID = "other-name" },
		"missing-id":  func(c *OAuthClientView) { c.ID = "" },
		"tenant":      func(c *OAuthClientView) { c.TenantID = "tenant-2" },
		"application": func(c *OAuthClientView) { c.ApplicationID = "01APP000000000000000000009" },
		"environment": func(c *OAuthClientView) { c.EnvironmentID = "01ENV000000000000000000009" },
		"status":      func(c *OAuthClientView) { c.Status = "DISABLED" },
		"client-type": func(c *OAuthClientView) { c.ClientType = "confidential" },
		"auth-method": func(c *OAuthClientView) { c.TokenAuthMethod = "client_secret_post" },
		"grant-types": func(c *OAuthClientView) { c.GrantTypes = []string{"authorization_code"} },
		"scopes":      func(c *OAuthClientView) { c.Scopes = []string{"other.scope"} },
	}
	for name, mutate := range mutations {
		client := updateClientFixture()
		mutate(&client)
		if err := ValidateSubsystemUpdateServiceClient(client, updateClientExpectation()); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
			t.Fatalf("%s: err = %v, want invalid", name, err)
		}
	}
	expectation := updateClientExpectation()
	expectation.ApplicationID = ""
	if err := ValidateSubsystemUpdateServiceClient(updateClientFixture(), expectation); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("missing expectation identifiers must be invalid, got %v", err)
	}
}

// 浏览器客户端额外的安全校验：PKCE 必须启用、回调地址必须精确包含（与既有实现一致）。
func TestValidateSubsystemUpdateServiceClientEnforcesPKCEAndRedirectURI(t *testing.T) {
	requirePKCE := true
	client := updateClientFixture()
	client.RequirePKCE = true
	client.RedirectURIs = []string{"https://settlement.example.com/settlement/auth/callback"}
	expectation := updateClientExpectation()
	expectation.RequirePKCE = &requirePKCE
	expectation.RedirectURIs = []string{"https://settlement.example.com/settlement/auth/callback"}
	if err := ValidateSubsystemUpdateServiceClient(client, expectation); err != nil {
		t.Fatalf("valid browser client must pass: %v", err)
	}

	withoutPKCE := client
	withoutPKCE.RequirePKCE = false
	if err := ValidateSubsystemUpdateServiceClient(withoutPKCE, expectation); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("missing PKCE err = %v", err)
	}

	wrongRedirect := client
	wrongRedirect.RedirectURIs = []string{"https://other.example.com/callback"}
	if err := ValidateSubsystemUpdateServiceClient(wrongRedirect, expectation); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("redirect mismatch err = %v", err)
	}

	// 不设置可选字段时不做这两项校验（对既有调用方保持兼容）。
	plain := updateClientExpectation()
	if err := ValidateSubsystemUpdateServiceClient(updateClientFixture(), plain); err != nil {
		t.Fatalf("optional checks must stay opt-in: %v", err)
	}
}
