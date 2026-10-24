package application

import (
	"errors"
	"testing"
)

func assemblyFixture() SubsystemOnboardingProvisioningAssembly {
	return SubsystemOnboardingProvisioningAssembly{
		TenantID: " tenant-1 ", ApplicationID: " 01APP000000000000000000001 ", EnvironmentID: "01ENV000000000000000000001",
		ApplicationCode: " settlement ", Environment: " dev ",
		Issuer: " https://sso.example.com/realms/basic ", AuthenticationProvider: " platform ",
		ManifestChecksum: " manifest-aaa ",
		ClientID:         "settlement-dev-web", ClientSecret: " secret-value ",
		CatalogPublisherClientID: "settlement-publisher", CatalogPublisherClientSecret: "publisher-secret",
		ServiceCredentials: []SubsystemServiceCredential{{Purpose: "audit_ingest"}},
		RedirectURI:        " https://settlement.example.com/callback ", PublicURL: " https://settlement.example.com ",
		PathPrefix: " /settlement ", UpstreamURL: " http://settlement-api:8084 ",
	}
}

// 装配：非敏感字段去空白；**凭据原样传递**（密钥不做 trim，避免破坏合法值）。
func TestBuildSubsystemOnboardingProvisioningInputMapsAndTrims(t *testing.T) {
	input, err := BuildSubsystemOnboardingProvisioningInput(assemblyFixture())
	if err != nil {
		t.Fatal(err)
	}
	if input.TenantID != "tenant-1" || input.ApplicationCode != "settlement" || input.Environment != "dev" {
		t.Fatalf("identifiers = %+v", input)
	}
	if input.ManifestChecksum != "manifest-aaa" || input.PathPrefix != "/settlement" || input.UpstreamURL != "http://settlement-api:8084" {
		t.Fatalf("target fields = %+v", input)
	}
	if input.Issuer != "https://sso.example.com/realms/basic" || input.AuthenticationProvider != "platform" {
		t.Fatalf("auth fields = %+v", input)
	}
	if input.ClientSecret != " secret-value " || input.CatalogPublisherClientSecret != "publisher-secret" {
		t.Fatalf("credentials must be passed through unchanged")
	}
	if len(input.ServiceCredentials) != 1 || input.ServiceCredentials[0].Purpose != "audit_ingest" {
		t.Fatalf("service credentials = %+v", input.ServiceCredentials)
	}
	if input.RedirectURI != "https://settlement.example.com/callback" || input.PublicURL != "https://settlement.example.com" {
		t.Fatalf("urls = %+v", input)
	}
}

// 缺失必需字段即失败（不猜测、不用空值继续部署）。
func TestBuildSubsystemOnboardingProvisioningInputRequiresIdentifiersAndCredentials(t *testing.T) {
	cases := map[string]func(*SubsystemOnboardingProvisioningAssembly){
		"tenant_id":        func(a *SubsystemOnboardingProvisioningAssembly) { a.TenantID = "  " },
		"application_code": func(a *SubsystemOnboardingProvisioningAssembly) { a.ApplicationCode = "" },
		"environment":      func(a *SubsystemOnboardingProvisioningAssembly) { a.Environment = "" },
	}
	for name, mutate := range cases {
		assembly := assemblyFixture()
		mutate(&assembly)
		if _, err := BuildSubsystemOnboardingProvisioningInput(assembly); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

// 凭据与资源 ID 为空必须原样传递（由既有服务/Agent 校验或补齐），装配不因此失败——保持既有行为。
func TestBuildSubsystemOnboardingProvisioningInputPassesEmptyCredentials(t *testing.T) {
	assembly := assemblyFixture()
	assembly.ClientID = ""
	assembly.ClientSecret = ""
	assembly.ApplicationID = ""
	assembly.EnvironmentID = ""
	input, err := BuildSubsystemOnboardingProvisioningInput(assembly)
	if err != nil {
		t.Fatalf("empty credentials must be passed through: %v", err)
	}
	if input.ClientID != "" || input.ClientSecret != "" {
		t.Fatalf("credentials = %q/%q", input.ClientID, input.ClientSecret)
	}
}

// 可选字段为空是允许的（例如无发布者凭据的场景），不得因此失败。
func TestBuildSubsystemOnboardingProvisioningInputAllowsEmptyOptionalFields(t *testing.T) {
	assembly := assemblyFixture()
	assembly.CatalogPublisherClientID = ""
	assembly.CatalogPublisherClientSecret = ""
	assembly.ServiceCredentials = nil
	assembly.RedirectURI = ""
	if _, err := BuildSubsystemOnboardingProvisioningInput(assembly); err != nil {
		t.Fatalf("optional fields must be allowed to be empty: %v", err)
	}
}
