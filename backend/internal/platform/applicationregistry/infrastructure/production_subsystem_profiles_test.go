package infrastructure

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProductionSubsystemProfilesLoadReviewedRepositoryTargets(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	profilesPath := filepath.Join(root, "deploy", "production", "subsystems.d")
	capabilities, err := LoadProductionSubsystemCapabilities(filepath.Join(root, "deploy", "production"), profilesPath)
	if err != nil {
		t.Fatalf("load repository profiles: %v", err)
	}
	if !capabilities.Enabled || capabilities.Mode != "production" || capabilities.DefaultApplicationCode != "contract_management" {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if !reflect.DeepEqual(capabilities.SupportedApplicationCodes, []string{"contract_management", "customer_and_opportunity", "customer_portal", "project_management"}) {
		t.Fatalf("supported applications = %#v", capabilities.SupportedApplicationCodes)
	}
	if len(capabilities.Targets) != 5 {
		t.Fatalf("targets = %#v", capabilities.Targets)
	}

	provisioner, err := newProductionComposeSubsystemProvisioner(ProductionComposeSubsystemProvisionerConfig{
		Enabled: true, DeployRoot: filepath.Join(root, "deploy", "production"), ProfilesDirectory: profilesPath,
		AllowedTenantID: "tenant-1", DockerBinary: "docker", Timeout: time.Minute,
	}, &recordingSubsystemRunner{})
	if err != nil {
		t.Fatalf("construct profile registry: %v", err)
	}
	for _, target := range []string{"customer_portal", "project_management"} {
		if _, err := provisioner.target(target, "prod"); err != nil {
			t.Fatalf("reviewed target %s/prod was not routable: %v", target, err)
		}
	}
	if _, err := provisioner.target("unreviewed_system", "prod"); err == nil {
		t.Fatal("unreviewed target was routable")
	}
}

func TestProductionSubsystemProfilesRejectUnknownFieldsAndUnsafeBindings(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(string) string{
		"unknown YAML field": func(manifest string) string {
			return strings.Replace(manifest, "version: 1", "version: 1\nunexpected_command: whoami", 1)
		},
		"unsupported binding": func(manifest string) string {
			return strings.Replace(manifest, "OIDC_CLIENT_ID: client_id", "OIDC_CLIENT_ID: shell.command", 1)
		},
		"unsupported service purpose": func(manifest string) string {
			return strings.Replace(manifest, "OIDC_CLIENT_ID: client_id", "OIDC_CLIENT_ID: service.unreviewed_scope.client_id", 1)
		},
		"template escapes deployment root": func(manifest string) string {
			return strings.Replace(manifest, "template_path: sample.env.example", "template_path: ../sample.env.example", 1)
		},
		"generated key has unsupported shape": func(manifest string) string {
			return strings.Replace(manifest, "SAMPLE_KEY_BASE64", "SAMPLE_SECRET", 1)
		},
		"generated key overlaps binding": func(manifest string) string {
			return strings.Replace(manifest, "OIDC_CLIENT_ID: client_id", "OIDC_CLIENT_ID: client_id\n        SAMPLE_KEY_BASE64: client_secret", 1)
		},
		"generated key overlaps required key": func(manifest string) string {
			return strings.Replace(manifest, "required_existing_keys: []", "required_existing_keys: [SAMPLE_KEY_BASE64]", 1)
		},
		"reserved platform service": func(manifest string) string {
			return strings.Replace(manifest, "runtime_services: [sample-api]", "runtime_services: [platform-api]", 1)
		},
		"reserved platform image": func(manifest string) string {
			return strings.Replace(manifest, "release_image_keys: [SAMPLE_IMAGE]", "release_image_keys: [PLATFORM_IMAGE]", 1)
		},
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, profilesPath := productionProfilesFixture(t)
			writeProductionProfileForTest(t, profilesPath, "sample.yaml", mutate(minimalProductionProfileYAML))
			if _, err := LoadProductionSubsystemCapabilities(root, profilesPath); err == nil {
				t.Fatalf("unsafe profile %q was accepted", name)
			}
		})
	}
}

func TestProductionSubsystemProfilesRejectDuplicateTargetsAndWritableManifest(t *testing.T) {
	t.Parallel()
	t.Run("duplicate target", func(t *testing.T) {
		root, profilesPath := productionProfilesFixture(t)
		writeProductionProfileForTest(t, profilesPath, "a.yaml", minimalProductionProfileYAML)
		writeProductionProfileForTest(t, profilesPath, "b.yaml", minimalProductionProfileYAML)
		if _, err := LoadProductionSubsystemCapabilities(root, profilesPath); err == nil {
			t.Fatal("duplicate target was accepted")
		}
	})
	t.Run("group writable manifest", func(t *testing.T) {
		root, profilesPath := productionProfilesFixture(t)
		path := writeProductionProfileForTest(t, profilesPath, "sample.yaml", minimalProductionProfileYAML)
		if err := os.Chmod(path, 0o660); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProductionSubsystemCapabilities(root, profilesPath); err == nil {
			t.Fatal("group-writable manifest was accepted")
		}
	})
}

func TestProductionSubsystemProfilesAllowReviewedTemplateWithoutManagedBindings(t *testing.T) {
	t.Parallel()
	root, profilesPath := productionProfilesFixture(t)
	manifest := strings.Replace(minimalProductionProfileYAML, `      required_existing_keys: []
      generated_keys: [SAMPLE_KEY_BASE64]
      values: {OIDC_SCOPES: "openid profile"}
      bindings:
        OIDC_CLIENT_ID: client_id`, `      required_existing_keys: []
      generated_keys: []
      values: {}
      bindings: {}`, 1)
	writeProductionProfileForTest(t, profilesPath, "sample.yaml", manifest)
	if _, err := LoadProductionSubsystemCapabilities(root, profilesPath); err != nil {
		t.Fatalf("reviewed template-only runtime file was rejected: %v", err)
	}
}

func productionProfilesFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	profilesPath := filepath.Join(root, "subsystems.d")
	if err := os.Mkdir(profilesPath, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, profilesPath
}

func writeProductionProfileForTest(t *testing.T, directory, name, contents string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalProductionProfileYAML = `version: 1
default: true
application:
  code: sample
  name: 示例系统
  description: 测试清单
  environment: prod
  path_prefix: /sample
  upstream_url: http://sample-api:8080
  client_type: confidential
runtime:
  required_infrastructure_keys: [SAMPLE_MYSQL_PASSWORD]
  files:
    - path: runtime/sample.env
      template_path: sample.env.example
      compose_environment_key: SAMPLE_RUNTIME_ENV_FILE
      required_existing_keys: []
      generated_keys: [SAMPLE_KEY_BASE64]
      values: {OIDC_SCOPES: "openid profile"}
      bindings:
        OIDC_CLIENT_ID: client_id
compose:
  profiles: [sample]
  dependency_services: [sample-mysql]
  database: {service: sample-mysql, name: sample}
  migrate_service: sample-migrate
  runtime_services: [sample-api]
  teardown_services: [sample-api]
  release_image_keys: [SAMPLE_IMAGE]
`
