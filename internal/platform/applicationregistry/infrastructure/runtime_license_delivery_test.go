package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func runtimeLicenseFixture(t *testing.T) (*productionComposeTarget, application.SubsystemProvisioningInput) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	profile := productionSubsystemProfile{}
	profile.Manifest.Application.Code = "contract_management"
	profile.Manifest.Application.Environment = "prod"
	profile.Manifest.Compose.RuntimeServices = []string{"contract-api", "contract-worker"}
	approvals := []application.RuntimeLicenseApproval{{ServiceID: "contract-api", Protocol: 1, CoverageDigest: "sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64)}, {ServiceID: "contract-worker", Protocol: 1, CoverageDigest: "sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("d", 64)}}
	release := runtimeLicenseRelease{Version: 1, Application: "contract_management", Environment: "prod", Components: approvals}
	raw, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "runtime-license-contract_management-prod.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	target := &productionComposeTarget{}
	target.config.DeployRoot = root
	target.config.Profile = profile
	input := application.SubsystemProvisioningInput{ApplicationCode: "contract_management", Environment: "prod", RuntimeLicenseSettings: application.RuntimeLicenseSettings{InstanceID: "installation-1", Environment: "production", PlatformBaseURL: "http://api:8080", PlatformPublicKeyPath: "/keys/license-platform-public.pem", StateDirectory: "/var/lib/license", AllowHTTP: true}}
	for i, item := range approvals {
		input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{Approval: item, ClientID: item.ServiceID, ClientSecret: "isolated-test-secret-" + string(rune('a'+i))})
	}
	digest, err := target.runtimeLicenseReleaseDigest(release)
	if err != nil {
		t.Fatal(err)
	}
	for i := range input.RuntimeLicenseCredentials {
		input.RuntimeLicenseCredentials[i].ReleaseDigest = digest
	}
	return target, input
}
func TestRuntimeLicenseDeliveryIsolatesSecrets(t *testing.T) {
	target, input := runtimeLicenseFixture(t)
	if err := target.writeRuntimeLicenseCredentials(input); err != nil {
		t.Fatal(err)
	}
	for _, c := range input.RuntimeLicenseCredentials {
		path := filepath.Join(target.config.DeployRoot, "runtime", "license-"+c.Approval.ServiceID+".env")
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("credential file permissions")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		values := parseEnvironmentValues(string(raw))
		if values["COMMERCIAL_LICENSE_CLIENT_SECRET"] != c.ClientSecret || values["COMMERCIAL_LICENSE_ENVIRONMENT"] != "production" {
			t.Fatal("lost component binding")
		}
		if values["COMMERCIAL_LICENSE_STATE_PATH"] != runtimeLicenseStatePath(input.RuntimeLicenseSettings, c) {
			t.Fatal("durable state lost identity binding")
		}
		for _, other := range input.RuntimeLicenseCredentials {
			if c.ClientID != other.ClientID && strings.Contains(string(raw), other.ClientSecret) {
				t.Fatal("worker credential leaked into API env")
			}
		}
	}
	if err := target.writeRuntimeLicenseCredentials(input); err != nil {
		t.Fatal("retry failed")
	}
}

func TestRuntimeLicenseReplacementCannotInheritPendingCache(t *testing.T) {
	_, input := runtimeLicenseFixture(t)
	old := input.RuntimeLicenseCredentials[0]
	previous := runtimeLicenseStatePath(input.RuntimeLicenseSettings, old)
	rotated := old
	rotated.ClientSecret = "new-isolated-secret"
	if runtimeLicenseStatePath(input.RuntimeLicenseSettings, rotated) != previous {
		t.Fatal("secret retry lost durable anchor")
	}
	for _, field := range []string{"client", "image", "coverage", "instance", "environment"} {
		settings, next := input.RuntimeLicenseSettings, old
		switch field {
		case "client":
			next.ClientID += "-new-generation"
		case "image":
			next.Approval.ImageDigest = "sha256:" + strings.Repeat("f", 64)
		case "coverage":
			next.Approval.CoverageDigest = "sha256:" + strings.Repeat("e", 64)
		case "instance":
			settings.InstanceID += "-new"
		case "environment":
			settings.Environment = "test"
		}
		if runtimeLicenseStatePath(settings, next) == previous {
			t.Fatal("replacement inherited old state", field)
		}
	}
}

func TestRuntimeLicenseDeliveryBindsCompleteApprovalBeforeWrite(t *testing.T) {
	for _, field := range []string{"retire", "generation", "missing", "mixed"} {
		target, input := runtimeLicenseFixture(t)
		path := filepath.Join(target.config.DeployRoot, "runtime-license-contract_management-prod.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var release runtimeLicenseRelease
		if err = json.Unmarshal(raw, &release); err != nil {
			t.Fatal(err)
		}
		switch field {
		case "retire":
			release.RetireServiceIDs = []string{"contract-old-worker"}
		case "generation":
			release.ReleaseGeneration = "new-generation"
		case "missing":
			for i := range input.RuntimeLicenseCredentials {
				input.RuntimeLicenseCredentials[i].ReleaseDigest = ""
			}
		case "mixed":
			input.RuntimeLicenseCredentials[0].ReleaseDigest = ""
		}
		raw, err = json.Marshal(release)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if target.writeRuntimeLicenseCredentials(input) == nil {
			t.Fatal("accepted approval drift", field)
		}
		if target.retireRuntimeLicenseContainers(context.Background(), input) == nil {
			t.Fatal("retirement ignored approval drift", field)
		}
		if _, err = os.Stat(filepath.Join(target.config.DeployRoot, "runtime", "license-contract-api.env")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("wrote before approval validation", field)
		}
	}
}
func TestRuntimeLicenseDeliveryRejectsUnapprovedAndSymlink(t *testing.T) {
	target, input := runtimeLicenseFixture(t)
	input.RuntimeLicenseCredentials[0].Approval.CoverageDigest = "sha256:" + strings.Repeat("f", 64)
	if target.writeRuntimeLicenseCredentials(input) == nil {
		t.Fatal("accepted coverage drift")
	}
	target, input = runtimeLicenseFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target.config.DeployRoot, "runtime", "license-contract-api.env")); err != nil {
		t.Fatal(err)
	}
	if target.writeRuntimeLicenseCredentials(input) == nil {
		t.Fatal("followed credential symlink")
	}
	raw, _ := os.ReadFile(outside)
	if string(raw) != "unchanged" {
		t.Fatal("overwrote outside target")
	}
}
func TestRuntimeLicenseApprovalRequiresExactProfile(t *testing.T) {
	target, _ := runtimeLicenseFixture(t)
	path := filepath.Join(target.config.DeployRoot, "runtime-license-contract_management-prod.json")
	if _, err := loadRuntimeLicenseRelease(path, target.config.Profile); err != nil {
		t.Fatal(err)
	}
	target.config.Profile.Manifest.Compose.RuntimeServices = append(target.config.Profile.Manifest.Compose.RuntimeServices, "missing-worker")
	if _, err := loadRuntimeLicenseRelease(path, target.config.Profile); err == nil {
		t.Fatal("accepted incomplete component set")
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimeLicenseRelease(path, target.config.Profile); err == nil {
		t.Fatal("accepted writable release approval")
	}
}
func TestRuntimeLicenseSecretsIncludedInLogRedaction(t *testing.T) {
	input := application.SubsystemProvisioningInput{RuntimeLicenseCredentials: []application.RuntimeLicenseCredential{{ClientSecret: "isolated-test-secret"}}}
	if strings.Contains(sanitizeProvisioningLog("failure isolated-test-secret", productionProvisioningSecrets(input)), "isolated-test-secret") {
		t.Fatal("runtime credential escaped log redaction")
	}
}

func TestRuntimeLicenseRetirementMustBeExplicitAndOutsideCurrentProfile(t *testing.T) {
	target, _ := runtimeLicenseFixture(t)
	path := filepath.Join(target.config.DeployRoot, "runtime-license-contract_management-prod.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var release runtimeLicenseRelease
	if err = json.Unmarshal(original, &release); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ids   []string
		valid bool
	}{
		{[]string{"contract-old-worker"}, true},
		{[]string{"contract-api"}, false},
		{[]string{"../worker"}, false},
		{[]string{"contract-old-worker", "contract-old-worker"}, false},
	} {
		release.RetireServiceIDs = tc.ids
		raw, err := json.Marshal(release)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := loadRuntimeLicenseReleaseDocument(path, target.config.Profile)
		if (err == nil) != tc.valid {
			t.Fatal("retirement validation mismatch", tc.ids, err)
		}
		if tc.valid && len(got.RetireServiceIDs) != 1 {
			t.Fatal("retirement lost")
		}
	}
}
