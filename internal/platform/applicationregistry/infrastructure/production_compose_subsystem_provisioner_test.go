package infrastructure

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
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
		"PLATFORM_AUTHORIZATION_CONTEXT_URL=http://platform-api:8080/oauth2/authorization-context",
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
	generatedKey := parseEnvironmentValues(string(contents))["CONTRACT_TEST_KEY_BASE64"]
	decodedKey, err := base64.StdEncoding.DecodeString(generatedKey)
	if err != nil || len(decodedKey) != 32 {
		t.Fatalf("generated runtime key is invalid: length=%d error=%v", len(decodedKey), err)
	}
	target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatalf("repeat runtime write: %v", err)
	}
	repeatedContents, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if repeatedKey := parseEnvironmentValues(string(repeatedContents))["CONTRACT_TEST_KEY_BASE64"]; repeatedKey != generatedKey {
		t.Fatal("repeat runtime write rotated the generated key")
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

func TestResolveProductionAuthorizationContextBinding(t *testing.T) {
	t.Parallel()
	value, err := resolveProductionBinding(productionContractInput("https://platform.example.com"), "authorization_context_url")
	if err != nil {
		t.Fatalf("resolve authorization context binding: %v", err)
	}
	if value != productionAuthorizationContextURL {
		t.Fatalf("authorization context URL = %q", value)
	}
}

func TestProductionComposeSubsystemProvisionerPreflightAllowsInfrastructurePlaceholders(t *testing.T) {
	t.Parallel()
	provisioner, runner, _ := productionProvisionerFixture(t)

	target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	placeholderEnvironment := "MYSQL_PASSWORD=REPLACE_WITH_PLATFORM_PASSWORD\n" +
		"MYSQL_ROOT_PASSWORD=REPLACE_WITH_PLATFORM_ROOT_PASSWORD\n" +
		"IAM_MOBILE_ENCRYPTION_KEY=REPLACE_WITH_IAM_KEY\n" +
		"IAM_BOOTSTRAP_TOKEN=REPLACE_WITH_BOOTSTRAP_TOKEN\n" +
		"CONTRACT_MYSQL_PASSWORD=REPLACE_WITH_CONTRACT_PASSWORD\n" +
		"CONTRACT_MYSQL_ROOT_PASSWORD=REPLACE_WITH_CONTRACT_ROOT_PASSWORD\n"
	if err := os.WriteFile(target.config.RuntimeEnvPath, []byte(placeholderEnvironment), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", testProductionApplicationCode)); err != nil {
		t.Fatalf("preflight rejected infrastructure placeholders: %v", err)
	}
	preflightCalls := len(runner.calls)
	if err := provisioner.Provision(context.Background(), productionContractInput("https://platform.example.com")); err == nil {
		t.Fatal("provision accepted missing production infrastructure secrets")
	} else if !strings.Contains(err.Error(), "production subsystem database credentials are incomplete") {
		t.Fatalf("provision error = %v, want infrastructure secret validation error", err)
	}
	if len(runner.calls) != preflightCalls {
		t.Fatalf("provision reached Docker before infrastructure validation: %#v", runner.calls[preflightCalls:])
	}
}

func TestProductionComposeSubsystemProvisionerTeardownDoesNotRequireDatabaseCredentials(t *testing.T) {
	t.Parallel()
	provisioner, runner, _ := productionProvisionerFixture(t)
	target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	placeholderEnvironment := "CONTRACT_MYSQL_PASSWORD=REPLACE_WITH_CONTRACT_PASSWORD\n" +
		"CONTRACT_MYSQL_ROOT_PASSWORD=REPLACE_WITH_CONTRACT_ROOT_PASSWORD\n"
	if err := os.WriteFile(target.config.RuntimeEnvPath, []byte(placeholderEnvironment), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Teardown(context.Background(), "tenant-1", testProductionApplicationCode, testProductionEnvironment); err != nil {
		t.Fatalf("teardown rejected placeholder database credentials: %v", err)
	}
	if len(runner.calls) != 1 || !containsString(runner.calls[0].arguments, "stop") {
		t.Fatalf("teardown did not run the fixed stop step: %#v", runner.calls)
	}
}

func TestProductionComposeSubsystemProvisionerUpdateWritesManifestFixedValues(t *testing.T) {
	t.Parallel()
	provisioner, _, contractPath := productionProvisionerFixture(t)
	if err := provisioner.Update(context.Background(), productionContractInput("https://platform.example.com")); err != nil {
		t.Fatalf("update: %v", err)
	}
	contents, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED=true",
		"CONTRACT_TEST_KEY_BASE64=",
	} {
		if !strings.Contains(string(contents), expected) {
			t.Fatalf("update did not write manifest fixed value %q:\n%s", expected, contents)
		}
	}
	if strings.Contains(string(contents), "OIDC_CLIENT_SECRET=browser-secret") {
		t.Fatalf("update unexpectedly wrote a binding value:\n%s", contents)
	}
}

func TestProductionComposeSubsystemProvisionerAuthenticationUpdateWritesAndRetainsRollbackCredential(t *testing.T) {
	t.Parallel()
	provisioner, _, contractPath := productionProvisionerFixture(t)
	input := productionContractInput("http://keycloak.example.com/realms/basic-platform")
	input.AuthenticationRuntimeUpdate = true
	if err := provisioner.Update(context.Background(), input); err != nil {
		t.Fatalf("authentication update: %v", err)
	}
	contents, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"OIDC_ISSUER=http://keycloak.example.com/realms/basic-platform",
		"OIDC_CLIENT_ID=contract_management-prod-web",
		"OIDC_CLIENT_SECRET=browser-secret",
		"OIDC_CLIENT_ID_ROLLBACK=PENDING_ONBOARDING",
		"OIDC_CLIENT_SECRET_ROLLBACK=PENDING_ONBOARDING",
	} {
		if !strings.Contains(string(contents), expected) {
			t.Fatalf("authentication update did not write %q:\n%s", expected, contents)
		}
	}
}

func TestProductionComposeSubsystemProvisionerTestServerAllowsPlaceholderDatabaseCredentials(t *testing.T) {
	t.Parallel()
	provisioner, runner, _ := productionProvisionerFixtureWithPlaceholderAllowance(t, true)
	target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	placeholderEnvironment := "MYSQL_PASSWORD=REPLACE_WITH_PLATFORM_PASSWORD\n" +
		"MYSQL_ROOT_PASSWORD=REPLACE_WITH_PLATFORM_ROOT_PASSWORD\n" +
		"IAM_MOBILE_ENCRYPTION_KEY=REPLACE_WITH_IAM_KEY\n" +
		"IAM_BOOTSTRAP_TOKEN=REPLACE_WITH_BOOTSTRAP_TOKEN\n" +
		"CONTRACT_MYSQL_PASSWORD=REPLACE_WITH_CONTRACT_PASSWORD\n" +
		"CONTRACT_MYSQL_ROOT_PASSWORD=REPLACE_WITH_CONTRACT_ROOT_PASSWORD\n"
	if err := os.WriteFile(target.config.RuntimeEnvPath, []byte(placeholderEnvironment), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", testProductionApplicationCode)); err != nil {
		t.Fatalf("preflight rejected placeholders with allowance enabled: %v", err)
	}
	if err := provisioner.Provision(context.Background(), productionContractInput("https://platform.example.com")); err != nil {
		t.Fatalf("test server provision rejected placeholder database credentials: %v", err)
	}
	if len(runner.calls) == 0 {
		t.Fatal("test server provision did not reach fixed deployment steps")
	}
}

func TestProductionComposeSubsystemProvisionerInitializesMissingRuntimeFromReviewedTemplate(t *testing.T) {
	t.Parallel()
	provisioner, _, runtimePath := productionProvisionerFixture(t)
	target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	auxiliaryTemplatePath := filepath.Join(target.config.DeployRoot, "auxiliary.env.example")
	auxiliaryRuntimePath := filepath.Join(target.config.DeployRoot, "runtime", "auxiliary.env")
	if err := os.WriteFile(auxiliaryTemplatePath, []byte("AUXILIARY_SETTING=preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target.config.RuntimeBootstrapFiles = append(target.config.RuntimeBootstrapFiles, productionSubsystemRuntimeFileManifest{
		Path: "runtime/auxiliary.env", TemplatePath: "auxiliary.env.example", ComposeEnvironmentKey: "AUXILIARY_RUNTIME_ENV_FILE",
	})
	if err := os.Remove(runtimePath); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", testProductionApplicationCode)); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	contents, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "UNMANAGED_TEMPLATE_VALUE=preserved") ||
		!strings.Contains(string(contents), "CONTRACT_TEST_KEY_BASE64=REPLACE_WITH_32_BYTE_BASE64_KEY") {
		t.Fatalf("runtime file was not initialized from template:\n%s", contents)
	}
	info, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("initialized runtime mode = %o", info.Mode().Perm())
	}
	auxiliaryContents, err := os.ReadFile(auxiliaryRuntimePath)
	if err != nil {
		t.Fatalf("read supporting runtime initialized from another reviewed profile: %v", err)
	}
	if string(auxiliaryContents) != "AUXILIARY_SETTING=preserved\n" {
		t.Fatalf("supporting runtime contents = %q", auxiliaryContents)
	}
}

func TestProductionRuntimeBootstrapFilesRejectsCrossProfileTemplateMismatch(t *testing.T) {
	t.Parallel()
	profiles := []productionSubsystemProfile{
		{Manifest: productionSubsystemManifest{Runtime: productionSubsystemRuntimeManifest{Files: []productionSubsystemRuntimeFileManifest{{
			Path: "runtime/shared.env", TemplatePath: "one.env.example", ComposeEnvironmentKey: "SHARED_RUNTIME_ENV_FILE",
		}}}}},
		{Manifest: productionSubsystemManifest{Runtime: productionSubsystemRuntimeManifest{Files: []productionSubsystemRuntimeFileManifest{{
			Path: "runtime/shared.env", TemplatePath: "two.env.example", ComposeEnvironmentKey: "SHARED_RUNTIME_ENV_FILE",
		}}}}},
	}
	if _, err := productionRuntimeBootstrapFiles(profiles); err == nil {
		t.Fatal("cross-profile runtime template mismatch was accepted")
	}
}

func TestProductionComposeSubsystemProvisionerTightensRuntimeModeAndPreservesUnknownKeys(t *testing.T) {
	t.Parallel()
	provisioner, _, runtimePath := productionProvisionerFixture(t)
	if err := os.WriteFile(runtimePath, []byte("OIDC_CLIENT_ID=PENDING_ONBOARDING\nCUSTOM_FUTURE_SETTING=enabled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runtimePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", testProductionApplicationCode)); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	info, err := os.Stat(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime mode = %o, want 600", info.Mode().Perm())
	}
	contents, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "CUSTOM_FUTURE_SETTING=enabled") {
		t.Fatalf("unknown subsystem setting was removed:\n%s", contents)
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

// productionFailureOutputRunner 模拟生产 Agent：固定部署步骤失败时支持 RunOutput 抓取日志，
// 用于验证失败详情会携带目标容器日志摘要。
type productionFailureOutputRunner struct {
	calls    []recordingSubsystemRunnerCall
	failUp   bool
	upOutput string
	logs     string
}

func (runner *productionFailureOutputRunner) record(directory string, environment []string, name string, arguments ...string) {
	runner.calls = append(runner.calls, recordingSubsystemRunnerCall{
		directory: directory, environment: environment, binary: name, arguments: append([]string(nil), arguments...),
	})
}

func (runner *productionFailureOutputRunner) Run(_ context.Context, directory string, environment []string, name string, arguments ...string) error {
	runner.record(directory, environment, name, arguments...)
	joined := strings.Join(arguments, " ")
	if runner.failUp && strings.Contains(joined, "up") && strings.Contains(joined, "contract-api") {
		return errors.New("container exited before health check")
	}
	return nil
}

func (runner *productionFailureOutputRunner) RunOutput(ctx context.Context, directory string, environment []string, name string, arguments ...string) ([]byte, error) {
	runner.record(directory, environment, name, arguments...)
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "logs") {
		return []byte(runner.logs), nil
	}
	if runner.failUp && strings.Contains(joined, "up") && strings.Contains(joined, "contract-api") {
		return []byte(runner.upOutput), errors.New("container exited before health check")
	}
	return nil, nil
}

// productionMigrateFailureOutputRunner 模拟迁移失败，并让 RunOutput 返回迁移容器输出。
type productionMigrateFailureOutputRunner struct {
	calls []recordingSubsystemRunnerCall
	logs  string
}

func (runner *productionMigrateFailureOutputRunner) record(directory string, environment []string, name string, arguments ...string) {
	runner.calls = append(runner.calls, recordingSubsystemRunnerCall{
		directory: directory, environment: environment, binary: name, arguments: append([]string(nil), arguments...),
	})
}

func (runner *productionMigrateFailureOutputRunner) Run(_ context.Context, directory string, environment []string, name string, arguments ...string) error {
	runner.record(directory, environment, name, arguments...)
	if strings.Contains(strings.Join(arguments, " "), "contract-migrate") {
		return errors.New("migrate exited non-zero")
	}
	return nil
}

func (runner *productionMigrateFailureOutputRunner) RunOutput(ctx context.Context, directory string, environment []string, name string, arguments ...string) ([]byte, error) {
	runner.record(directory, environment, name, arguments...)
	if strings.Contains(strings.Join(arguments, " "), "contract-migrate") {
		return []byte(runner.logs), errors.New("migrate exited non-zero")
	}
	return nil, nil
}

func TestProductionComposeSubsystemProvisionerSurfacesMigrateLogsOnFailure(t *testing.T) {
	t.Parallel()
	runner := &productionMigrateFailureOutputRunner{logs: "configuration failed: OIDC_CLIENT_SECRET is required"}
	provisioner, _ := productionProvisionerFixtureWithRunner(t, false, runner)
	err := provisioner.Provision(context.Background(), productionContractInput("https://platform.example.com"))
	if err == nil {
		t.Fatal("provision unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "OIDC_CLIENT_SECRET is required") {
		t.Fatalf("migrate error does not carry container output: %v", err)
	}
}

func TestProductionComposeSubsystemProvisionerSurfacesRuntimeServiceLogsOnFailure(t *testing.T) {
	t.Parallel()
	runner := &productionFailureOutputRunner{
		failUp: true,
		logs:   "CRM startup failed: authorization catalog token returned HTTP 401\ncontainer exited",
	}
	provisioner, _ := productionProvisionerFixtureWithRunner(t, false, runner)
	err := provisioner.Provision(context.Background(), productionContractInput("https://platform.example.com"))
	if err == nil {
		t.Fatal("provision unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "authorization catalog token returned HTTP 401") {
		t.Fatalf("provision error does not carry container logs: %v", err)
	}
}

func TestProductionComposeSubsystemProvisionerPrefersRuntimeComposeOutputOnFailure(t *testing.T) {
	t.Parallel()
	runner := &productionFailureOutputRunner{
		failUp:   true,
		upOutput: "customer-notification-delivery-worker: exec: \"./notification-delivery-worker\": stat ./notification-delivery-worker: no such file or directory\nCLIENT_SECRET=browser-secret",
		logs:     "customer-presale-worker | Started Worker Namespace default",
	}
	provisioner, _ := productionProvisionerFixtureWithRunner(t, false, runner)
	err := provisioner.Provision(context.Background(), productionContractInput("https://platform.example.com"))
	if err == nil {
		t.Fatal("provision unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "customer-notification-delivery-worker") || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("provision error does not carry compose failure output: %v", err)
	}
	if strings.Contains(err.Error(), "Started Worker") {
		t.Fatalf("provision error unexpectedly fell back to unrelated service logs: %v", err)
	}
	if strings.Contains(err.Error(), "browser-secret") {
		t.Fatalf("provision error leaked a secret: %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call.arguments, " "), "logs") {
			t.Fatalf("provision unexpectedly collected fallback logs after compose output: %#v", call.arguments)
		}
	}
}

func productionProvisionerFixture(t *testing.T) (*ProductionComposeSubsystemProvisioner, *recordingSubsystemRunner, string) {
	t.Helper()
	return productionProvisionerFixtureWithPlaceholderAllowance(t, false)
}

func productionProvisionerFixtureWithPlaceholderAllowance(t *testing.T, allowPlaceholderDatabaseCredentials bool) (*ProductionComposeSubsystemProvisioner, *recordingSubsystemRunner, string) {
	t.Helper()
	runner := &recordingSubsystemRunner{}
	provisioner, contractPath := productionProvisionerFixtureWithRunner(t, allowPlaceholderDatabaseCredentials, runner)
	return provisioner, runner, contractPath
}

func productionProvisionerFixtureWithRunner(t *testing.T, allowPlaceholderDatabaseCredentials bool, runner subsystemCommandRunner) (*ProductionComposeSubsystemProvisioner, string) {
	t.Helper()
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(canonicalRoot, ".env")
	contractPath := filepath.Join(canonicalRoot, "runtime", "contract.env")
	contractTemplatePath := filepath.Join(canonicalRoot, "contract.env.example")
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
		runtimePath:          "MYSQL_PASSWORD=platform-password\nMYSQL_ROOT_PASSWORD=platform-root-password\nIAM_MOBILE_ENCRYPTION_KEY=valid-key\nIAM_BOOTSTRAP_TOKEN=valid-bootstrap-token\nCONTRACT_MYSQL_PASSWORD=contract-password\nCONTRACT_MYSQL_ROOT_PASSWORD=contract-root-password\nOIDC_CLIENT_ID=PENDING_ONBOARDING\nOIDC_CLIENT_SECRET=PENDING_ONBOARDING\n",
		contractPath:         "OIDC_CLIENT_ID=PENDING_ONBOARDING\nOIDC_CLIENT_SECRET=PENDING_ONBOARDING\n",
		contractTemplatePath: "OIDC_CLIENT_ID=PENDING_ONBOARDING\nOIDC_CLIENT_SECRET=PENDING_ONBOARDING\nCONTRACT_TEST_KEY_BASE64=REPLACE_WITH_32_BYTE_BASE64_KEY\nUNMANAGED_TEMPLATE_VALUE=preserved\n",
		releasePath:          "PLATFORM_IMAGE=example/platform@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nCONTRACT_IMAGE=example/contract@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n",
		composePath:          "services: {}\n",
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
      template_path: contract.env.example
      compose_environment_key: CONTRACT_RUNTIME_ENV_FILE
      required_existing_keys: []
      generated_keys: [CONTRACT_TEST_KEY_BASE64]
      values:
        PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED: "true"
      bindings:
        OIDC_ISSUER: issuer
        OIDC_CLIENT_ID: client_id
        OIDC_CLIENT_SECRET: client_secret
        OIDC_TENANT_ID: tenant_id
        OIDC_SESSION_COOKIE_SECURE: cookie_secure
        PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET: catalog_publisher_client_secret
        PLATFORM_AUTHORIZATION_CONTEXT_URL: authorization_context_url
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
	provisioner, err := newProductionComposeSubsystemProvisioner(ProductionComposeSubsystemProvisionerConfig{
		Enabled: true, DeployRoot: canonicalRoot, ProfilesDirectory: profilesPath, RuntimeEnvPath: runtimePath,
		ReleaseEnvPath: releasePath, ComposeFile: composePath, ComposeProject: "basic-platform-production",
		AllowedTenantID: "tenant-1", DockerBinary: "docker", Timeout: time.Minute,
		AllowPlaceholderDatabaseCredentials: allowPlaceholderDatabaseCredentials,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	return provisioner, contractPath
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
