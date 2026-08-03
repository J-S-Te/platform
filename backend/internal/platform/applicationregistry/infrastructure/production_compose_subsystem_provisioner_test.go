package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
)

const (
	testProductionApplicationCode = "contract_management"
	testProductionEnvironment     = "prod"
	testProductionPathPrefix      = "/contract_management"
	testProductionUpstreamURL     = "http://contract-api:8081"
)

func TestProductionComposeSubsystemProvisionerRejectsTargetsMissingFromReviewedProfiles(t *testing.T) {
	t.Parallel()
	provisioner, _, _ := productionProvisionerFixture(t)
	if err := provisioner.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", "customer_portal")); err == nil {
		t.Fatal("production preflight accepted a non-contract application")
	}
	input := productionContractInput("https://platform.example.com")
	input.Environment = "dev"
	if err := provisioner.Provision(context.Background(), input); err == nil {
		t.Fatal("production provision accepted a non-prod environment")
	}
}

func TestProductionComposeSubsystemProvisionerWritesManagedSecretsAndRunsOnlyFixedServices(t *testing.T) {
	t.Parallel()
	provisioner, runner, runtimePath := productionProvisionerFixture(t)
	input := productionContractInput("https://platform.example.com")
	if err := provisioner.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", testProductionApplicationCode)); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if err := provisioner.Provision(context.Background(), input); err != nil {
		t.Fatalf("provision: %v", err)
	}
	contents, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"OIDC_CLIENT_ID=contract_management-prod-web",
		"OIDC_CLIENT_SECRET=browser-secret",
		"OIDC_TENANT_ID=tenant-1",
		"OIDC_SESSION_COOKIE_SECURE=true",
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED=true",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET=publisher-secret",
		"PLATFORM_AUDIT_CLIENT_ID=contract_management-prod-audit-publisher",
		"PLATFORM_AUDIT_CLIENT_SECRET=audit-secret",
	} {
		if !strings.Contains(string(contents), expected) {
			t.Fatalf("runtime environment missing %q:\n%s", expected, contents)
		}
	}
	info, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime environment permissions = %v", info.Mode().Perm())
	}

	// Preflight performs Docker and Compose validation. Provision then performs exactly the
	// dependency, migration and contract API operations; no browser value becomes an argument.
	if len(runner.calls) != 6 {
		t.Fatalf("runner calls = %d, want 6: %#v", len(runner.calls), runner.calls)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call.arguments, " ")
		auditCredential, _ := input.ServiceCredential(application.ServiceCredentialAuditIngest)
		for _, secret := range []string{input.ClientSecret, input.CatalogPublisherClientSecret, auditCredential.PlaintextSecret} {
			if strings.Contains(joined, secret) {
				t.Fatalf("secret appeared in command arguments: %s", joined)
			}
		}
	}
	if !containsString(runner.calls[2].arguments, "contract-mysql") || !containsString(runner.calls[2].arguments, "temporal") {
		t.Fatalf("dependency call is not fixed: %v", runner.calls[2].arguments)
	}
	if !strings.Contains(strings.Join(runner.calls[3].arguments, " "), "mysqldump") {
		t.Fatalf("backup call is not fixed: %v", runner.calls[3].arguments)
	}
	if !containsString(runner.calls[4].arguments, "contract-migrate") {
		t.Fatalf("migration call is not fixed: %v", runner.calls[4].arguments)
	}
	if !containsString(runner.calls[5].arguments, "contract-api") {
		t.Fatalf("contract API call is not fixed: %v", runner.calls[5].arguments)
	}
}

func TestProductionComposeSubsystemProvisionerRejectsInconsistentPublicIntegration(t *testing.T) {
	t.Parallel()
	provisioner, runner, _ := productionProvisionerFixture(t)
	input := productionContractInput("https://platform.example.com")
	input.RedirectURI = "https://attacker.example/callback"
	if err := provisioner.Provision(context.Background(), input); err == nil {
		t.Fatal("provision accepted a redirect URI outside the configured issuer")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid request reached command runner: %#v", runner.calls)
	}
}

func TestProductionComposeSubsystemProvisionerBindsFirstTenantAndRejectsAnotherTenant(t *testing.T) {
	t.Parallel()
	provisioner, _, _ := productionProvisionerFixture(t)
	if err := provisioner.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", testProductionApplicationCode)); err != nil {
		t.Fatalf("first tenant preflight: %v", err)
	}
	other := productionPreflightInput("https://platform.example.com", testProductionApplicationCode)
	other.TenantID = "tenant-2"
	if err := provisioner.Preflight(context.Background(), other); err == nil {
		t.Fatal("second tenant was allowed to claim the shared production contract runtime")
	}
}

func productionProvisionerFixture(t *testing.T) (*ProductionComposeSubsystemProvisioner, *recordingSubsystemRunner, string) {
	t.Helper()
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(canonicalRoot, ".env")
	contractPath := filepath.Join(canonicalRoot, "runtime", "contract.env")
	releasePath := filepath.Join(canonicalRoot, ".release.env")
	composePath := filepath.Join(canonicalRoot, "compose.yaml")
	profilesPath := filepath.Join(canonicalRoot, "subsystems.d")
	if err := os.Mkdir(filepath.Join(canonicalRoot, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(profilesPath, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		runtimePath:  "MYSQL_PASSWORD=platform-password\nMYSQL_ROOT_PASSWORD=platform-root-password\nIAM_MOBILE_ENCRYPTION_KEY=valid-key\nIAM_BOOTSTRAP_TOKEN=valid-bootstrap-token\nCONTRACT_MYSQL_PASSWORD=contract-password\nCONTRACT_MYSQL_ROOT_PASSWORD=contract-root-password\nOIDC_CLIENT_ID=PENDING_ONBOARDING\nOIDC_CLIENT_SECRET=PENDING_ONBOARDING\n",
		contractPath: "OIDC_CLIENT_ID=PENDING_ONBOARDING\nOIDC_CLIENT_SECRET=PENDING_ONBOARDING\n",
		releasePath:  "PLATFORM_IMAGE=example/platform@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nCONTRACT_IMAGE=example/contract@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		composePath:  "services: {}\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := `version: 1
default: true
application:
  code: contract_management
  name: 合同管理系统
  description: 合同创建与审批
  environment: prod
  path_prefix: /contract_management
  upstream_url: http://contract-api:8081
  client_type: confidential
runtime:
  required_infrastructure_keys: [MYSQL_PASSWORD, MYSQL_ROOT_PASSWORD, IAM_MOBILE_ENCRYPTION_KEY, IAM_BOOTSTRAP_TOKEN, CONTRACT_MYSQL_PASSWORD, CONTRACT_MYSQL_ROOT_PASSWORD]
  files:
    - path: runtime/contract.env
      compose_environment_key: CONTRACT_RUNTIME_ENV_FILE
      required_existing_keys: []
      values:
        PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED: "true"
      bindings:
        OIDC_ISSUER: issuer
        OIDC_CLIENT_ID: client_id
        OIDC_CLIENT_SECRET: client_secret
        OIDC_TENANT_ID: tenant_id
        OIDC_SESSION_COOKIE_SECURE: cookie_secure
        PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET: catalog_publisher_client_secret
        PLATFORM_AUDIT_CLIENT_ID: service.audit_ingest.client_id
        PLATFORM_AUDIT_CLIENT_SECRET: service.audit_ingest.client_secret
compose:
  profiles: [release]
  dependency_services: [contract-mysql, temporal]
  database: {service: contract-mysql, name: contract_management}
  migrate_service: contract-migrate
  runtime_services: [contract-api]
  teardown_services: [contract-api]
  release_image_keys: [CONTRACT_IMAGE]
`
	if err := os.WriteFile(filepath.Join(profilesPath, "contract.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingSubsystemRunner{}
	provisioner, err := newProductionComposeSubsystemProvisioner(ProductionComposeSubsystemProvisionerConfig{
		Enabled: true, DeployRoot: canonicalRoot, ProfilesDirectory: profilesPath, RuntimeEnvPath: runtimePath,
		ReleaseEnvPath: releasePath, ComposeFile: composePath, ComposeProject: "basic-platform-production",
		AllowedTenantID: "tenant-1", DockerBinary: "docker", Timeout: time.Minute,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	return provisioner, runner, contractPath
}

func productionPreflightInput(origin, applicationCode string) application.SubsystemPreflightInput {
	return application.SubsystemPreflightInput{
		TenantID: "tenant-1", ApplicationCode: applicationCode, Environment: testProductionEnvironment,
		Issuer: origin, PublicBaseURL: origin, UpstreamURL: testProductionUpstreamURL,
		PathPrefix: testProductionPathPrefix, ClientType: "confidential",
	}
}

func productionContractInput(origin string) application.SubsystemProvisioningInput {
	return application.SubsystemProvisioningInput{
		TenantID: "tenant-1", ApplicationID: "app-1", ApplicationCode: testProductionApplicationCode,
		Environment: testProductionEnvironment, Issuer: origin,
		ClientID: "contract_management-prod-web", ClientSecret: "browser-secret",
		CatalogPublisherClientID:     "contract_management-prod-catalog-publisher",
		CatalogPublisherClientSecret: "publisher-secret",
		RedirectURI:                  origin + testProductionPathPrefix + "/auth/callback",
		PublicURL:                    origin + testProductionPathPrefix + "/",
		PathPrefix:                   testProductionPathPrefix, UpstreamURL: testProductionUpstreamURL,
		ServiceCredentials: []application.SubsystemServiceCredential{{
			Purpose:         application.ServiceCredentialAuditIngest,
			OAuthClient:     application.OAuthClientView{ClientID: "contract_management-prod-audit-publisher"},
			PlaintextSecret: "audit-secret",
		}},
	}
}
