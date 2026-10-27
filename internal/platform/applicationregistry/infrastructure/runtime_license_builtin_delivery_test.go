package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/license-core/catalog"
)

func TestBuiltinStatePathIgnoresImageAndCoverage(t *testing.T) {
	settings := application.RuntimeLicenseSettings{InstanceID: "instance", Environment: "production", StateDirectory: "/var/lib/commercial-license"}
	base := application.RuntimeLicenseCredential{
		Approval:             application.RuntimeLicenseApproval{ServiceID: "contract-api", Protocol: catalog.RegistrationProtocolV2},
		RegistrationProtocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version,
		CredentialGeneration: "catalog-1", ClientID: "client",
	}
	first := runtimeLicenseStatePath(settings, base)
	changed := base
	changed.Approval.ImageDigest = "sha256:old"
	changed.Approval.CoverageDigest = "sha256:old"
	if runtimeLicenseStatePath(settings, changed) != first {
		t.Fatal("V2 state path changed with diagnostic digests")
	}
	changed.CredentialGeneration = "catalog-2"
	if runtimeLicenseStatePath(settings, changed) == first {
		t.Fatal("V2 state path ignored credential generation")
	}
	legacy := base
	legacy.RegistrationProtocol = catalog.RegistrationProtocolV1
	legacy.CatalogVersion = 0
	legacy.Approval.Protocol = 1
	legacy.Approval.ImageDigest = "sha256:image"
	legacy.Approval.CoverageDigest = "sha256:coverage"
	if runtimeLicenseStatePath(settings, legacy) == first {
		t.Fatal("V1 state path collided with V2")
	}
}

func TestBuiltinDeliveryNeedsNoApprovalFileAndOmitsDigests(t *testing.T) {
	target, input := runtimeLicenseFixture(t)
	if err := os.Remove(filepath.Join(target.config.DeployRoot, "runtime-license-contract_management-prod.json")); err != nil {
		t.Fatal(err)
	}
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(context.Background(), "contract_management", "prod")
	if err != nil {
		t.Fatal(err)
	}
	input.RuntimeLicenseCredentials = nil
	for index, component := range set.Components {
		input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{
			Approval:             application.RuntimeLicenseApproval{ServiceID: component.ServiceID, Protocol: catalog.RegistrationProtocolV2},
			RegistrationProtocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version,
			CredentialGeneration: component.CredentialGeneration, ClientID: "builtin-" + component.ServiceID,
			ClientSecret: "builtin-secret-" + strings.Repeat(string(rune('a'+index)), 16),
		})
	}
	if err := target.writeRuntimeLicenseCredentials(input); err != nil {
		t.Fatal(err)
	}
	for _, credential := range input.RuntimeLicenseCredentials {
		path := filepath.Join(target.config.DeployRoot, "runtime", "license-"+credential.Approval.ServiceID+".env")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		values := parseEnvironmentValues(string(raw))
		if values["COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL"] != "2" || values["COMMERCIAL_LICENSE_CATALOG_VERSION"] != "1" {
			t.Fatalf("V2 registration not written: %v", values)
		}
		if values["COMMERCIAL_LICENSE_COVERAGE_DIGEST"] != "" || values["COMMERCIAL_LICENSE_IMAGE_DIGEST"] != "" {
			t.Fatalf("V2 delivery wrote diagnostic digests as required values: %v", values)
		}
		if values["COMMERCIAL_LICENSE_STATE_PATH"] != runtimeLicenseStatePath(input.RuntimeLicenseSettings, credential) {
			t.Fatal("V2 state path mismatch")
		}
	}
}

func TestMixedLegacyAndBuiltinDeliveryKeepsEachProtocol(t *testing.T) {
	target, input := runtimeLicenseFixture(t)
	var legacy application.RuntimeLicenseCredential
	for _, credential := range input.RuntimeLicenseCredentials {
		if credential.Approval.ServiceID == "contract-api" {
			legacy = credential
		}
	}
	if legacy.ClientID == "" {
		t.Fatal("missing legacy fixture")
	}
	set, err := (BuiltinRuntimeComponentCatalog{}).RuntimeComponentSet(context.Background(), "contract_management", "prod")
	if err != nil {
		t.Fatal(err)
	}
	var worker application.RuntimeComponentSpec
	for _, component := range set.Components {
		if component.ServiceID == "contract-worker" {
			worker = component
		}
	}
	builtin := application.RuntimeLicenseCredential{
		Approval:             application.RuntimeLicenseApproval{ServiceID: worker.ServiceID, Protocol: catalog.RegistrationProtocolV2},
		RegistrationProtocol: catalog.RegistrationProtocolV2, CatalogVersion: catalog.Version,
		CredentialGeneration: worker.CredentialGeneration, ClientID: "mixed-worker", ClientSecret: "mixed-worker-secret",
	}
	input.RuntimeLicenseCredentials = []application.RuntimeLicenseCredential{legacy, builtin}
	if err := target.writeRuntimeLicenseCredentials(input); err != nil {
		t.Fatal(err)
	}
	read := func(service string) map[string]string {
		raw, err := os.ReadFile(filepath.Join(target.config.DeployRoot, "runtime", "license-"+service+".env"))
		if err != nil {
			t.Fatal(err)
		}
		return parseEnvironmentValues(string(raw))
	}
	legacyValues := read("contract-api")
	if legacyValues["COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL"] != "" || legacyValues["COMMERCIAL_LICENSE_COVERAGE_DIGEST"] == "" || legacyValues["COMMERCIAL_LICENSE_IMAGE_DIGEST"] == "" {
		t.Fatalf("legacy env changed: %v", legacyValues)
	}
	builtinValues := read("contract-worker")
	if builtinValues["COMMERCIAL_LICENSE_REGISTRATION_PROTOCOL"] != "2" || builtinValues["COMMERCIAL_LICENSE_CATALOG_VERSION"] != "1" || builtinValues["COMMERCIAL_LICENSE_COVERAGE_DIGEST"] != "" || builtinValues["COMMERCIAL_LICENSE_IMAGE_DIGEST"] != "" {
		t.Fatalf("builtin env changed: %v", builtinValues)
	}
}

func TestBuiltinRetirementDoesNotReadLegacyApprovalWithoutRemovedMembers(t *testing.T) {
	target, input := runtimeLicenseFixture(t)
	if err := os.Remove(filepath.Join(target.config.DeployRoot, "runtime-license-contract_management-prod.json")); err != nil {
		t.Fatal(err)
	}
	for i := range input.RuntimeLicenseCredentials {
		input.RuntimeLicenseCredentials[i].RegistrationProtocol = 2
		input.RuntimeLicenseCredentials[i].RetirementNotRequired = true
	}
	if err := target.retireRuntimeLicenseContainers(context.Background(), input); err != nil {
		t.Fatalf("unchanged V2 topology requires legacy approval: %v", err)
	}
	input.RuntimeLicenseCredentials[0].RetirementNotRequired = false
	if err := target.retireRuntimeLicenseContainers(context.Background(), input); err == nil {
		t.Fatal("unverified retirement bypassed approval")
	}
}

// TestProductionBuiltinProvisionAndUpdateWithoutLegacyApproval 用途：验证 V2 完整部署不读取旧批准且实际加载私有许可。
// 参数：t 为隔离部署测试上下文。
// 返回值：无。
func TestProductionBuiltinProvisionAndUpdateWithoutLegacyApproval(t *testing.T) {
	for _, update := range []bool{false, true} {
		p, _, _ := productionProvisionerFixture(t)
		target, err := p.target(testProductionApplicationCode, testProductionEnvironment)
		if err != nil {
			t.Fatal(err)
		}
		input := productionContractInput("https://platform.example.com")
		_, binding := runtimeLicenseFixture(t)
		input.RuntimeLicenseSettings = binding.RuntimeLicenseSettings
		for _, id := range target.config.Profile.Manifest.Compose.RuntimeServices {
			input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{Approval: application.RuntimeLicenseApproval{ServiceID: id, Protocol: 2}, RegistrationProtocol: 2, CatalogVersion: 1, CredentialGeneration: "catalog-1", ClientID: "builtin-" + id, ClientSecret: "isolated-secret", RetirementNotRequired: true})
		}
		target.runner = &licensedComposeFixtureRunner{subsystemCommandRunner: target.runner, input: input}
		// Fixture starts without a V1 runtime approval file. Both full Agent paths must
		// reach deployment while preserving all normal manifest/configuration checks.
		if update {
			if err := p.Provision(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			err = p.Update(context.Background(), input)
		} else {
			err = p.Provision(context.Background(), input)
		}
		if err != nil {
			t.Fatalf("update=%v: %v", update, err)
		}
		for _, credential := range input.RuntimeLicenseCredentials {
			if _, err := os.Stat(filepath.Join(target.config.DeployRoot, "runtime", "license-"+credential.Approval.ServiceID+".env")); err != nil {
				t.Fatal(err)
			}
		}
	}
}
