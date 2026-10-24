package application

import (
	"errors"
	"testing"
)

func updateAssemblyFixture() SubsystemUpdateProvisioningAssembly {
	return SubsystemUpdateProvisioningAssembly{
		TenantID: " tenant-1 ", ApplicationID: " 01APP000000000000000000001 ", ApplicationCode: " settlement ",
		Environment: " dev ", ManifestChecksum: " manifest-aaa ", Issuer: " https://sso.example.com/realms/basic ",
		AuthenticationProvider: " platform ",
		PublicBaseURL:          "https://settlement.example.com/ ", UpstreamURL: " http://settlement-api:8084/ ",
		PathPrefix: " /settlement/ ",
		ClientID:   "settlement-dev-web", ClientSecret: "secret-value",
		CatalogPublisherClientID: "settlement-publisher", CatalogPublisherClientSecret: "publisher-secret",
		RelatedServiceClients: []RelatedSubsystemServiceClient{{ApplicationCode: "contract_management"}},
	}
}

// 网关字段齐全：派生公网 URL 与回调地址（与既有实现一致），其余字段去空白、凭据原样。
func TestBuildSubsystemUpdateProvisioningInputDerivesGatewayURLs(t *testing.T) {
	input, err := BuildSubsystemUpdateProvisioningInput(updateAssemblyFixture())
	if err != nil {
		t.Fatal(err)
	}
	if input.PublicURL != "https://settlement.example.com/settlement/" {
		t.Fatalf("public url = %q", input.PublicURL)
	}
	if input.RedirectURI != "https://settlement.example.com/settlement/auth/callback" {
		t.Fatalf("redirect uri = %q", input.RedirectURI)
	}
	if input.PathPrefix != "/settlement" || input.UpstreamURL != "http://settlement-api:8084" {
		t.Fatalf("gateway fields = %+v", input)
	}
	if input.TenantID != "tenant-1" || input.ApplicationCode != "settlement" || input.ManifestChecksum != "manifest-aaa" {
		t.Fatalf("identifiers = %+v", input)
	}
	if input.ClientSecret != "secret-value" || input.CatalogPublisherClientSecret != "publisher-secret" {
		t.Fatalf("credentials must be passed through unchanged")
	}
	if len(input.RelatedServiceClients) != 1 {
		t.Fatalf("related clients = %+v", input.RelatedServiceClients)
	}
}

// 网关字段全空：仅重建，不下发浏览器 URL（既有兼容行为）。
func TestBuildSubsystemUpdateProvisioningInputAllowsRebuildOnly(t *testing.T) {
	assembly := updateAssemblyFixture()
	assembly.PublicBaseURL, assembly.UpstreamURL, assembly.PathPrefix = "", "", ""
	input, err := BuildSubsystemUpdateProvisioningInput(assembly)
	if err != nil {
		t.Fatal(err)
	}
	if input.PublicURL != "" || input.RedirectURI != "" || input.PathPrefix != "" || input.UpstreamURL != "" {
		t.Fatalf("rebuild-only input must not carry gateway fields: %+v", input)
	}
}

// 部分提供网关字段：拒绝（与既有实现一致）。
func TestBuildSubsystemUpdateProvisioningInputRejectsPartialGatewayFields(t *testing.T) {
	cases := []func(*SubsystemUpdateProvisioningAssembly){
		func(a *SubsystemUpdateProvisioningAssembly) { a.UpstreamURL = "" },
		func(a *SubsystemUpdateProvisioningAssembly) { a.PathPrefix = "" },
		func(a *SubsystemUpdateProvisioningAssembly) { a.PublicBaseURL = "" },
	}
	for index, mutate := range cases {
		assembly := updateAssemblyFixture()
		mutate(&assembly)
		if _, err := BuildSubsystemUpdateProvisioningInput(assembly); !errors.Is(err, ErrValidation) {
			t.Fatalf("case %d: err = %v, want ErrValidation", index, err)
		}
	}
}
