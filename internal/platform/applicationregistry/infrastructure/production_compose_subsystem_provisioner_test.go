package infrastructure

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func TestResolveProductionOIDCBackchannelUsesTrustedProviderOnly(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ provider, want string }{
		{"platform", "http://platform-api:8080"}, {"keycloak", "http://keycloak:8080"},
	} {
		// Deliberately misleading issuer paths must never choose the backchannel.
		for _, issuer := range []string{"https://browser.example/realms/fake", "https://public.example", "http://untrusted:8080"} {
			value, err := resolveProductionBinding(application.SubsystemProvisioningInput{AuthenticationProvider: test.provider, Issuer: issuer}, "oidc_backchannel_base_url")
			if err != nil || value != test.want {
				t.Fatalf("provider %s resolved %q: %v", test.provider, value, err)
			}
		}
	}
	for _, provider := range []string{"", "unknown", "http://keycloak:8080", "Platform", " keycloak"} {
		if _, err := resolveProductionBinding(application.SubsystemProvisioningInput{AuthenticationProvider: provider}, "oidc_backchannel_base_url"); err == nil {
			t.Fatalf("untrusted provider %q accepted", provider)
		}
	}
}

func TestProductionAuthenticationBackchannelFullSwitchRollbackAndOrdinaryUpdate(t *testing.T) {
	t.Parallel()
	provisioner, _, path := productionProvisionerFixture(t)
	target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	target.config.Profile.Manifest.Runtime.Files[0].Bindings["OIDC_BACKCHANNEL_BASE_URL"] = "oidc_backchannel_base_url"
	input := productionContractInput("https://platform.example.com")
	input.AuthenticationProvider = "platform"
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	assertRuntime := func(issuer, backchannel, client, secret string) {
		t.Helper()
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		values := parseEnvironmentValues(string(contents))
		for key, expected := range map[string]string{"OIDC_ISSUER": issuer, "OIDC_BACKCHANNEL_BASE_URL": backchannel, "OIDC_CLIENT_ID": client, "OIDC_CLIENT_SECRET": secret} {
			if values[key] != expected {
				t.Fatalf("authentication batch mismatch for %s", key)
			}
		}
	}
	assertRuntime(input.Issuer, "http://platform-api:8080", input.ClientID, input.ClientSecret)
	input.AuthenticationProvider, input.Issuer = "keycloak", "https://sso.example/realms/basic-platform"
	input.ClientID, input.ClientSecret, input.AuthenticationRuntimeUpdate = "keycloak-client", "keycloak-secret", true
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	assertRuntime(input.Issuer, "http://keycloak:8080", "keycloak-client", "keycloak-secret")
	input.AuthenticationRuntimeUpdate = false
	input.ClientID, input.ClientSecret = "ignored-client", "ignored-secret"
	input.Issuer = "https://sso-updated.example/realms/basic-platform"
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	assertRuntime(input.Issuer, "http://keycloak:8080", "keycloak-client", "keycloak-secret")
	input.AuthenticationProvider, input.Issuer = "platform", "https://platform.example.com"
	input.AuthenticationRuntimeUpdate, input.ClientID, input.ClientSecret = true, "", ""
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	original := productionContractInput("https://platform.example.com")
	assertRuntime(input.Issuer, "http://platform-api:8080", original.ClientID, original.ClientSecret)
}

func TestProductionManagedAuthenticationRejectsUnknownAndIncompleteBatchWithoutWrite(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, provider, issuer, client, secret string
		auth                                   bool
	}{
		{"unknown fixed", "unknown", "https://platform.example.com", "", "", false},
		{"missing fixed", "", "https://platform.example.com", "", "", false},
		{"missing issuer", "platform", "", "", "", false},
		{"partial switch", "keycloak", "https://sso.example/realms/basic-platform", "new-client", "", true},
		{"missing switch credential", "keycloak", "https://sso.example/realms/basic-platform", "", "", true},
		{"unavailable rollback", "platform", "https://platform.example.com", "", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			provisioner, _, path := productionProvisionerFixture(t)
			target, err := provisioner.target(testProductionApplicationCode, testProductionEnvironment)
			if err != nil {
				t.Fatal(err)
			}
			target.config.Profile.Manifest.Runtime.Files[0].Bindings["OIDC_BACKCHANNEL_BASE_URL"] = "oidc_backchannel_base_url"
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			input := productionContractInput("https://platform.example.com")
			input.AuthenticationProvider, input.Issuer, input.ClientID, input.ClientSecret, input.AuthenticationRuntimeUpdate = test.provider, test.issuer, test.client, test.secret, test.auth
			if err := target.writeRuntimeFixedValues(input); err == nil {
				t.Fatal("invalid fixed authentication accepted")
			}
			if test.provider == "unknown" || test.provider == "" || test.issuer == "" {
				if err := target.writeRuntimeConfiguration(input); err == nil {
					t.Fatal("invalid full authentication accepted")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("invalid batch changed runtime")
			}
		})
	}
}

type dataAnalysisCatalogFixtureRunner struct {
	recordingSubsystemRunner
	output       []byte
	catalogError error
}

func (runner *dataAnalysisCatalogFixtureRunner) RunOutput(ctx context.Context, directory string, environment []string, name string, arguments ...string) ([]byte, error) {
	if containsString(arguments, "--services") {
		return []byte("data-analysis-mysql\ndata-analysis-metabase-init\ndata-analysis-migrate\ndata-analysis-metabase\ndata-analysis-api\ndata-analysis-aggregation-worker\ndata-analysis-alert-worker\n"), nil
	}
	if err := runner.Run(ctx, directory, environment, name, arguments...); err != nil {
		return nil, err
	}
	if containsString(arguments, "/app/authz-catalog") {
		return runner.output, runner.catalogError
	}
	return nil, nil
}

func TestProductionDataAnalysisCatalogHashBeforeRuntime(t *testing.T) {
	hash := "sha256:" + strings.Repeat("b", 64)
	for _, test := range []struct {
		name      string
		output    string
		err       error
		wantError bool
	}{
		{"valid", "application=data_analysis\nclaims_role_config_hash=" + hash + "\nmax_effective_roles=10\n", nil, false},
		{"missing", "application=data_analysis\n", nil, true},
		{"invalid", "claims_role_config_hash=sha256:not-a-hash\n", nil, true},
		{"duplicate", "claims_role_config_hash=" + hash + "\nclaims_role_config_hash=" + hash + "\n", nil, true},
		{"command failed", "secret-from-command", errors.New("secret-from-command"), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "runtime"), 0o700); err != nil {
				t.Fatal(err)
			}
			runtimePath := filepath.Join(root, "runtime", "data-analysis.env")
			original := "OIDC_ROLE_CONFIG_HASH=sha256:old\nUNMANAGED=preserved\n"
			if err := os.WriteFile(runtimePath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".env"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			module := platformModuleRoot(t)
			profiles, _, err := loadProductionSubsystemProfiles(filepath.Join(module, "deploy", "production"), filepath.Join(module, "deploy", "production", "subsystems.d"))
			if err != nil {
				t.Fatal(err)
			}
			var profile productionSubsystemProfile
			for _, candidate := range profiles {
				if candidate.Manifest.Application.Code == "data_analysis" {
					profile = candidate
				}
			}
			runner := &dataAnalysisCatalogFixtureRunner{output: []byte(test.output), catalogError: test.err}
			target := &productionComposeTarget{config: productionComposeTargetConfig{
				DeployRoot: root, RuntimeEnvPath: filepath.Join(root, ".env"), ReleaseEnvPath: filepath.Join(root, ".release.env"),
				ComposeFile: filepath.Join(root, "docker-compose.yml"), ComposeProject: "reviewed-test", DockerBinary: "docker", Profile: profile,
			}, runner: runner}
			err = target.deployLocked(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("deploy error = %v, wantError %v", err, test.wantError)
			}
			if err != nil && strings.Contains(err.Error(), "secret-from-command") {
				t.Fatal("command output secret leaked")
			}
			contents, err := os.ReadFile(runtimePath)
			if err != nil {
				t.Fatal(err)
			}
			migration, catalog, runtime := -1, -1, -1
			for index, call := range runner.calls {
				if containsString(call.arguments, "/app/authz-catalog") {
					catalog = index
					for _, value := range []string{"run", "--rm", "--no-deps", "--entrypoint", "data-analysis-migrate", "print", "data_analysis"} {
						if !containsString(call.arguments, value) {
							t.Fatalf("missing fixed catalog argument %s", value)
						}
					}
				} else if containsString(call.arguments, "data-analysis-migrate") && containsString(call.arguments, "--name") {
					migration = index
				} else if containsString(call.arguments, "--force-recreate") {
					runtime = index
				}
			}
			if migration < 0 || catalog <= migration {
				t.Fatalf("catalog must follow successful migration: %d/%d", migration, catalog)
			}
			if test.wantError {
				if runtime != -1 || string(contents) != original {
					t.Fatal("failed catalog inspection started business or changed runtime")
				}
			} else {
				if runtime <= catalog || !strings.Contains(string(contents), "OIDC_ROLE_CONFIG_HASH="+hash) || !strings.Contains(string(contents), "UNMANAGED=preserved") {
					t.Fatal("authoritative hash not written before runtime start")
				}
				if info, err := os.Stat(runtimePath); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatal("runtime permissions changed")
				}
			}
		})
	}
}

func TestProductionDataAnalysisCatalogHashDoesNotAffectOtherTargets(t *testing.T) {
	for _, applicationCode := range []string{"contract_management", "project_management", "customer_and_opportunity", "customer_portal", "settlement"} {
		target := &productionComposeTarget{runner: &dataAnalysisCatalogFixtureRunner{}}
		target.config.Profile.Manifest.Application.Code = applicationCode
		target.config.Profile.Manifest.Application.Environment = "prod"
		if err := target.refreshDataAnalysisCatalogHash(context.Background()); err != nil {
			t.Fatalf("%s: %v", applicationCode, err)
		}
		if len(target.runner.(*dataAnalysisCatalogFixtureRunner).calls) != 0 {
			t.Fatal("unrelated image inspected")
		}
	}
}

func TestParseDataAnalysisCatalogHashRejectsMalformedOutput(t *testing.T) {
	t.Parallel()
	for _, output := range []string{
		"", "claims_role_config_hash=sha256:" + strings.Repeat("A", 64),
		"claims_role_config_hash=sha256:" + strings.Repeat("a", 63),
		"claims_role_config_hash=sha256:" + strings.Repeat("g", 64),
		strings.Repeat("x", 64*1024+1),
	} {
		if _, err := parseDataAnalysisCatalogHash([]byte(output)); err == nil {
			t.Fatal("malformed catalog output accepted")
		}
	}
}

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

func TestProductionPublicTransportDefaultsToHTTPAndDerivesAllBindings(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"PUBLIC_PLATFORM_HOST=platform.example.com",
		"PUBLIC_SSO_HOST=sso.example.com",
		"PUBLIC_SSO_HTTP_PORT=18090",
		"KEYCLOAK_REALM=company",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := productionPublicTransportEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, expected := range []string{
		"PUBLIC_HTTPS_ENABLED=false",
		"PUBLIC_PLATFORM_ORIGIN=http://platform.example.com:8081",
		"PUBLIC_SSO_ORIGIN=http://sso.example.com:18090",
		"PUBLIC_KEYCLOAK_ISSUER=http://sso.example.com:18090/realms/company",
		"PUBLIC_TRANSPORT_COOKIE_SECURE=false",
		"PUBLIC_TRANSPORT_ALLOW_INSECURE_HTTP=true",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("derived environment %q does not contain %q", joined, expected)
		}
	}
}

func TestProductionPublicTransportUsesLegacyKeycloakPortForIPMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"PUBLIC_ACCESS_MODE=ip",
		"PUBLIC_PLATFORM_HOST=192.0.2.20",
		"PUBLIC_SSO_HOST=192.0.2.20",
		"PUBLIC_HTTP_PORT=80",
		"KEYCLOAK_HTTP_PORT=18090",
		"KEYCLOAK_REALM=basic-platform",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := productionPublicTransportEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, expected := range []string{
		"PUBLIC_PLATFORM_ORIGIN=http://192.0.2.20",
		"PUBLIC_SSO_ORIGIN=http://192.0.2.20:18090",
		"PUBLIC_KEYCLOAK_ISSUER=http://192.0.2.20:18090/realms/basic-platform",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("derived legacy IP environment %q does not contain %q", joined, expected)
		}
	}
}

func TestProductionPublicTransportDerivesHTTPSWithCustomPort(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"PUBLIC_HTTPS_ENABLED=true",
		"PUBLIC_PLATFORM_HOST=platform.example.com",
		"PUBLIC_SSO_HOST=sso.example.com",
		"PUBLIC_HTTPS_PORT=8443",
	}, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := productionPublicTransportEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\n")
	for _, expected := range []string{
		"PUBLIC_PLATFORM_ORIGIN=https://platform.example.com:8443",
		"PUBLIC_KEYCLOAK_ISSUER=https://sso.example.com:8443/realms/basic-platform",
		"PUBLIC_TRANSPORT_COOKIE_SECURE=true",
		"PUBLIC_TRANSPORT_KEYCLOAK_REQUIRE_HTTPS=true",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("derived environment %q does not contain %q", joined, expected)
		}
	}
}

func TestProductionPublicTransportRejectsInvalidSwitchHostAndPort(t *testing.T) {
	t.Parallel()
	tests := []string{
		"PUBLIC_HTTPS_ENABLED=yes\nPUBLIC_PLATFORM_HOST=platform.example.com\nPUBLIC_SSO_HOST=sso.example.com\n",
		"PUBLIC_PLATFORM_HOST=https://platform.example.com\nPUBLIC_SSO_HOST=sso.example.com\n",
		"PUBLIC_PLATFORM_HOST=platform.example.com\nPUBLIC_SSO_HOST=sso.example.com/path\n",
		"PUBLIC_PLATFORM_HOST=platform.example.com\nPUBLIC_SSO_HOST=sso.example.com\nPUBLIC_HTTP_PORT=70000\n",
		"PUBLIC_PLATFORM_HOST=platform.example.com\nPUBLIC_SSO_HOST=sso.example.com\nPUBLIC_SSO_HTTP_PORT=70000\n",
	}
	for index, contents := range tests {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("invalid-%d.env", index))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := productionPublicTransportEnvironment(path); err == nil {
			t.Fatalf("invalid public transport configuration %d was accepted", index)
		}
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
	if len(runner.calls) != 8 {
		t.Fatalf("runner calls = %d, want 8: %#v", len(runner.calls), runner.calls)
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
	backupCommand := strings.Join(runner.calls[3].arguments, " ")
	if !strings.Contains(backupCommand, "mysqldump") {
		t.Fatalf("backup call is not fixed: %v", runner.calls[3].arguments)
	}
	// SEC-F5a：root 密码不得以 -p"$MYSQL_ROOT_PASSWORD" 形式展开进 argv，必须走 MYSQL_PWD。
	if strings.Contains(backupCommand, "-p\"$MYSQL_ROOT_PASSWORD\"") || strings.Contains(backupCommand, "-p$MYSQL_ROOT_PASSWORD") {
		t.Fatalf("mysqldump password must not expand into argv: %s", backupCommand)
	}
	if !strings.Contains(backupCommand, "MYSQL_PWD=\"$MYSQL_ROOT_PASSWORD\"") {
		t.Fatalf("mysqldump must receive credentials via MYSQL_PWD: %s", backupCommand)
	}
	if !containsString(runner.calls[4].arguments, "contract-migrate") {
		t.Fatalf("migration call is not fixed: %v", runner.calls[4].arguments)
	}
	if containsString(runner.calls[4].arguments, "--rm") || !containsString(runner.calls[4].arguments, "--name") {
		t.Fatalf("migration container must be named and retained on failure: %v", runner.calls[4].arguments)
	}
	if !containsString(runner.calls[5].arguments, "container") || !containsString(runner.calls[5].arguments, "rm") {
		t.Fatalf("successful migration container was not cleaned precisely: %v", runner.calls[5].arguments)
	}
	if !containsString(runner.calls[6].arguments, "contract-api") {
		t.Fatalf("contract API call is not fixed: %v", runner.calls[6].arguments)
	}
	if !containsString(runner.calls[7].arguments, "contract-catalog-sync") {
		t.Fatalf("catalog sync call is not fixed: %v", runner.calls[7].arguments)
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

func TestResolveProductionInsecureHTTPOriginBinding(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		origin string
		want   string
	}{
		{origin: "http://203.0.113.10:8081", want: "true"},
		{origin: "https://platform.example.com", want: "false"},
	} {
		value, err := resolveProductionBinding(productionContractInput(test.origin), "allow_insecure_http_origin")
		if err != nil {
			t.Fatalf("resolve insecure HTTP origin binding for %s: %v", test.origin, err)
		}
		if value != test.want {
			t.Fatalf("insecure HTTP origin binding for %s = %q, want %q", test.origin, value, test.want)
		}
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
	input := productionContractInput("https://platform.example.com")
	if err := provisioner.Provision(context.Background(), input); err != nil {
		t.Fatalf("prepare runtime configuration: %v", err)
	}
	if err := provisioner.Update(context.Background(), input); err != nil {
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
	if !strings.Contains(string(contents), "OIDC_CLIENT_SECRET=browser-secret") {
		t.Fatalf("update unexpectedly lost the existing browser credential:\n%s", contents)
	}
}

func TestProductionComposeSubsystemProvisionerUpdateRejectsIncompleteRuntimeBeforeDocker(t *testing.T) {
	t.Parallel()
	provisioner, runner, _ := productionProvisionerFixture(t)
	err := provisioner.Update(context.Background(), productionContractInput("https://platform.example.com"))
	if err == nil || !strings.Contains(err.Error(), "runtime configuration is incomplete") {
		t.Fatalf("update error = %v, want incomplete runtime configuration", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("update reached Docker with incomplete runtime configuration: %#v", runner.calls)
	}
}

func TestProductionInitialAdoptionCreatesRuntimeOnlyWithCompleteCredentials(t *testing.T) {
	for _, complete := range []bool{false, true} {
		provisioner, runner, path := productionProvisionerFixture(t)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		input := productionContractInput("https://platform.example.com")
		input.InitialRuntimeProvisioning = true
		if !complete {
			input.ClientSecret = ""
		}
		err := provisioner.Update(context.Background(), input)
		if (err == nil) != complete {
			t.Fatalf("initial adoption credential validation: %v", err)
		}
		if !complete {
			if len(runner.calls) != 0 {
				t.Fatal("incomplete initial request reached Docker")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("incomplete initial request created runtime")
			}
			continue
		}
		if err := provisioner.Update(context.Background(), input); err != nil {
			t.Fatal("initial retry is not idempotent", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(raw), "OIDC_CLIENT_SECRET=browser-secret") {
			t.Fatal("initial browser credential not delivered")
		}
	}
}

func TestProductionComposeSubsystemProvisionerAuthenticationUpdateWritesAndRetainsRollbackCredential(t *testing.T) {
	t.Parallel()
	provisioner, _, contractPath := productionProvisionerFixture(t)
	input := productionContractInput("https://platform.example.com")
	if err := provisioner.Provision(context.Background(), input); err != nil {
		t.Fatalf("prepare runtime configuration: %v", err)
	}
	input.Issuer = "http://keycloak.example.com/realms/basic-platform"
	input.ClientID = "contract_management-prod-keycloak-web"
	input.ClientSecret = "keycloak-browser-secret"
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
		"OIDC_CLIENT_ID=contract_management-prod-keycloak-web",
		"OIDC_CLIENT_SECRET=keycloak-browser-secret",
		"OIDC_CLIENT_ID_ROLLBACK=contract_management-prod-web",
		"OIDC_CLIENT_SECRET_ROLLBACK=browser-secret",
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
	if _, err := os.Stat(auxiliaryRuntimePath); !os.IsNotExist(err) {
		t.Fatalf("unrelated subsystem runtime must not be initialized: %v", err)
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

// productionInitializationFailureOutputRunner 模拟初始化 one-shot 失败，验证失败容器
// 保留且后续迁移、运行服务不会继续执行。
type productionInitializationFailureOutputRunner struct {
	calls []recordingSubsystemRunnerCall
	logs  string
}

func (runner *productionInitializationFailureOutputRunner) record(directory string, environment []string, name string, arguments ...string) {
	runner.calls = append(runner.calls, recordingSubsystemRunnerCall{
		directory: directory, environment: environment, binary: name, arguments: append([]string(nil), arguments...),
	})
}

func (runner *productionInitializationFailureOutputRunner) Run(_ context.Context, directory string, environment []string, name string, arguments ...string) error {
	runner.record(directory, environment, name, arguments...)
	if containsString(arguments, "contract-database-init") {
		return errors.New("initialization exited non-zero")
	}
	return nil
}

func (runner *productionInitializationFailureOutputRunner) RunOutput(_ context.Context, directory string, environment []string, name string, arguments ...string) ([]byte, error) {
	runner.record(directory, environment, name, arguments...)
	if containsString(arguments, "contract-database-init") {
		return []byte(runner.logs), errors.New("initialization exited non-zero")
	}
	return nil, nil
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
	if !strings.Contains(err.Error(), "failed container retained as uip-contract-management-migrate-") {
		t.Fatalf("migrate error does not identify retained diagnostic container: %v", err)
	}
	for _, call := range runner.calls {
		if containsString(call.arguments, "--rm") && containsString(call.arguments, "contract-migrate") {
			t.Fatalf("failed migration container would be removed automatically: %v", call.arguments)
		}
		if len(call.arguments) >= 2 && call.arguments[0] == "container" && call.arguments[1] == "rm" {
			t.Fatalf("failed migration container was removed: %v", call.arguments)
		}
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
  catalog_sync_service: contract-catalog-sync
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
	}, &productionScopeFixtureRunner{wrapped: runner, services: "contract-api\ncontract-mysql\ntemporal\ncontract-migrate\ncontract-catalog-sync\n"})
	if err != nil {
		t.Fatal(err)
	}
	return provisioner, contractPath
}

// Existing command-order tests record deployment effects separately from the
// read-only scope query. Scope rejection is exercised explicitly below.
type productionScopeFixtureRunner struct {
	wrapped    subsystemCommandRunner
	services   string
	scopeCalls int
	scopeError error
}

func (r *productionScopeFixtureRunner) Run(ctx context.Context, directory string, environment []string, name string, args ...string) error {
	return r.wrapped.Run(ctx, directory, environment, name, args...)
}

func (r *productionScopeFixtureRunner) RunOutput(ctx context.Context, directory string, environment []string, name string, args ...string) ([]byte, error) {
	if containsString(args, "--services") {
		r.scopeCalls++
		if containsString(args, "--no-env-resolution") || containsString(args, "--no-interpolate") || containsString(args, "--profile") {
			return nil, errors.New("scope query must use portable interpolation and avoid profiles")
		}
		return []byte(r.services), r.scopeError
	}
	if output, ok := r.wrapped.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	}); ok {
		return output.RunOutput(ctx, directory, environment, name, args...)
	}
	return nil, r.wrapped.Run(ctx, directory, environment, name, args...)
}

func TestProductionComposeDisabledSubsystemHasNoDeploymentSideEffects(t *testing.T) {
	for _, operation := range []string{"preflight", "provision", "update"} {
		t.Run(operation, func(t *testing.T) {
			p, runner, runtimePath := productionProvisionerFixture(t)
			target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
			scope := target.runner.(*productionScopeFixtureRunner)
			scope.services = "platform-api\ncontract-mysql\ntemporal\n"
			if err := os.Remove(runtimePath); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "preflight":
				err = p.Preflight(context.Background(), productionPreflightInput("https://platform.example.com", testProductionApplicationCode))
			case "provision":
				err = p.Provision(context.Background(), productionContractInput("https://platform.example.com"))
			case "update":
				err = p.Update(context.Background(), productionContractInput("https://platform.example.com"))
			}
			if err == nil || !strings.Contains(err.Error(), "此系统未启用") {
				t.Fatalf("got %v", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("disabled subsystem ran commands: %#v", runner.calls)
			}
			if _, err := os.Stat(runtimePath); !os.IsNotExist(err) {
				t.Fatalf("disabled subsystem wrote runtime: %v", err)
			}
		})
	}
}

func TestProductionComposeScopeResolutionFailsClosed(t *testing.T) {
	p, runner, _ := productionProvisionerFixture(t)
	target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
	target.runner.(*productionScopeFixtureRunner).scopeError = errors.New("invalid YAML")
	if err := p.Provision(context.Background(), productionContractInput("https://platform.example.com")); err == nil {
		t.Fatal("invalid scope accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatal("invalid scope started deployment")
	}
}

func TestProductionComposeConditionalWorkerRequiresEnabledOnboardedDependency(t *testing.T) {
	for _, state := range []string{"disabled", "pending", "ready"} {
		t.Run(state, func(t *testing.T) {
			p, _, _ := productionProvisionerFixture(t)
			target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
			target.config.Profile.Manifest.Runtime.Files = append(target.config.Profile.Manifest.Runtime.Files, productionSubsystemRuntimeFileManifest{
				Path: "runtime/customer.env", WhenService: "customer-api", ComposeEnvironmentKey: "CUSTOMER_RUNTIME_ENV_FILE", RequiredExistingKeys: []string{"OIDC_CLIENT_SECRET"},
			})
			target.config.Profile.Manifest.Compose.RuntimeServices = append(target.config.Profile.Manifest.Compose.RuntimeServices, "portal-invite-compensation-worker")
			target.config.Profile.Manifest.Compose.ConditionalRuntimeServices = map[string]string{"portal-invite-compensation-worker": "runtime/customer.env"}
			scope := target.runner.(*productionScopeFixtureRunner)
			scope.services += "portal-invite-compensation-worker\n"
			if state != "disabled" {
				scope.services += "customer-api\n"
				value := "PENDING_ONBOARDING"
				if state == "ready" {
					value = "test-credential"
				}
				if err := os.WriteFile(filepath.Join(target.config.DeployRoot, "runtime/customer.env"), []byte("OIDC_CLIENT_SECRET="+value+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := target.validateEnabledServices(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := containsString(target.selectedRuntimeServices(), "portal-invite-compensation-worker")
			if got != (state == "ready") {
				t.Fatalf("worker included=%v for dependency %s", got, state)
			}
			if !containsString(target.selectedRuntimeServices(), "contract-api") {
				t.Fatal("primary runtime service excluded")
			}
		})
	}
}

func TestProductionComposeCommandDoesNotEnableLegacyProfiles(t *testing.T) {
	p, _, _ := productionProvisionerFixture(t)
	target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
	args, _ := target.composeCommand("config", "--services")
	if containsString(args, "--profile") {
		t.Fatalf("profiles must not override YAML membership: %v", args)
	}
}

func TestProductionComposeInitializationPrecedesMigrationAndBlocksOnFailure(t *testing.T) {
	t.Run("success is cleaned precisely", func(t *testing.T) {
		p, runner, _ := productionProvisionerFixture(t)
		target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
		target.config.Profile.Manifest.Compose.InitializationServices = []string{"contract-database-init"}
		target.runner.(*productionScopeFixtureRunner).services += "contract-database-init\n"
		err := p.Provision(context.Background(), productionContractInput("https://platform.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		initIndex, cleanupIndex, migrationIndex, runtimeIndex := -1, -1, -1, -1
		initializationContainer := ""
		for index, call := range runner.calls {
			if containsString(call.arguments, "contract-database-init") {
				initIndex = index
				if containsString(call.arguments, "--rm") || !containsString(call.arguments, "--name") {
					t.Fatalf("initialization container must be named and retained on failure: %v", call.arguments)
				}
				for argumentIndex, argument := range call.arguments {
					if argument == "--name" && argumentIndex+1 < len(call.arguments) {
						initializationContainer = call.arguments[argumentIndex+1]
					}
				}
			}
			if len(call.arguments) >= 3 && call.arguments[0] == "container" && call.arguments[1] == "rm" && call.arguments[2] == initializationContainer {
				cleanupIndex = index
			}
			if containsString(call.arguments, "contract-migrate") {
				migrationIndex = index
			}
			if containsString(call.arguments, "contract-api") {
				runtimeIndex = index
			}
		}
		if initIndex < 0 {
			t.Fatal("initialization never executed")
		}
		if initializationContainer == "" || !strings.HasPrefix(initializationContainer, "uip-contract-management-init-contract-database-init-") {
			t.Fatalf("unexpected initialization container name %q", initializationContainer)
		}
		if !(initIndex < cleanupIndex && cleanupIndex < migrationIndex && migrationIndex < runtimeIndex) {
			t.Fatalf("wrong order: init=%d cleanup=%d migrate=%d runtime=%d", initIndex, cleanupIndex, migrationIndex, runtimeIndex)
		}
	})

	t.Run("failure is retained and blocks later steps", func(t *testing.T) {
		runner := &productionInitializationFailureOutputRunner{logs: "database initialization failed"}
		p, _ := productionProvisionerFixtureWithRunner(t, false, runner)
		target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
		target.config.Profile.Manifest.Compose.InitializationServices = []string{"contract-database-init"}
		target.runner.(*productionScopeFixtureRunner).services += "contract-database-init\n"

		err := p.Provision(context.Background(), productionContractInput("https://platform.example.com"))
		if err == nil || !strings.Contains(err.Error(), "failed container retained as uip-contract-management-init-contract-database-init-") {
			t.Fatalf("initialization failure did not identify retained container: %v", err)
		}
		for _, call := range runner.calls {
			if containsString(call.arguments, "contract-migrate") || containsString(call.arguments, "contract-api") {
				t.Fatalf("failed initialization allowed later deployment step: %v", call.arguments)
			}
			if len(call.arguments) >= 2 && call.arguments[0] == "container" && call.arguments[1] == "rm" {
				t.Fatalf("failed initialization container was removed: %v", call.arguments)
			}
		}
	})
}

func TestProductionComposeMissingInitializerRejectsScope(t *testing.T) {
	p, runner, _ := productionProvisionerFixture(t)
	target, _ := p.target(testProductionApplicationCode, testProductionEnvironment)
	target.config.Profile.Manifest.Compose.InitializationServices = []string{"contract-database-init"}
	if err := p.Provision(context.Background(), productionContractInput("https://platform.example.com")); err == nil || !strings.Contains(err.Error(), "contract-database-init") {
		t.Fatalf("missing initializer accepted: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("missing initializer permitted deployment effects")
	}
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

func TestValidateProductionReleaseImageNamesMissingDigestAndFixCommands(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, ".release.env")

	writeRelease := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write release env: %v", err)
		}
	}

	for name, content := range map[string]string{
		"unset digest":     "CONTRACT_IMAGE=registry.example.com/contract-management/contract:pending\n",
		"mutable tag":      "CONTRACT_IMAGE=registry.example.com/contract-management/contract:20260922\n",
		"malformed digest": "CONTRACT_IMAGE=registry.example.com/contract-management/contract@sha256:nothex\n",
	} {
		content := content
		t.Run(name, func(t *testing.T) {
			writeRelease(content)
			err := validateProductionReleaseImage(path, "CONTRACT_IMAGE")
			if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
				t.Fatalf("error = %v", err)
			}
			message := err.Error()
			for _, expected := range []string{"CONTRACT_IMAGE", "deploy.sh import", "deploy.sh prepare"} {
				if !strings.Contains(message, expected) {
					t.Fatalf("immutable digest error missing %q: %s", expected, message)
				}
			}
		})
	}

	writeRelease("CONTRACT_IMAGE=registry.example.com/contract-management/contract@sha256:" + strings.Repeat("a", 64) + "\n")
	if err := validateProductionReleaseImage(path, "CONTRACT_IMAGE"); err != nil {
		t.Fatalf("valid immutable digest rejected: %v", err)
	}
}

func TestProductionComposeAvailableCapabilitiesFollowPreparedReleaseImages(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	releasePath := filepath.Join(directory, ".release.env")
	profiles := []productionSubsystemProfile{
		{
			Checksum: "sha256:customer",
			Manifest: productionSubsystemManifest{
				Application: productionSubsystemApplicationManifest{
					Code: "customer_and_opportunity", Name: "客户与商机管理系统", Environment: "prod",
				},
				Compose: productionSubsystemComposeManifest{ReleaseImageKeys: []string{"CUSTOMER_CRM_IMAGE"}},
			},
		},
		{
			Checksum: "sha256:contract",
			Manifest: productionSubsystemManifest{
				Application: productionSubsystemApplicationManifest{
					Code: "contract_management", Name: "合同管理系统", Environment: "prod",
				},
				Compose: productionSubsystemComposeManifest{ReleaseImageKeys: []string{"CONTRACT_IMAGE", "CONTRACT_MIGRATE_IMAGE"}},
			},
		},
	}
	provisioner := &ProductionComposeSubsystemProvisioner{
		enabled: true, profiles: profiles, releaseEnvPath: releasePath,
	}
	digest := "registry.local/uip/customer@sha256:" + strings.Repeat("a", 64)
	if err := os.WriteFile(releasePath, []byte(
		"CUSTOMER_CRM_IMAGE="+digest+"\n"+
			"CONTRACT_IMAGE=registry.local/uip/contract:pending\n"+
			"CONTRACT_MIGRATE_IMAGE=registry.local/uip/contract-migrate@sha256:"+strings.Repeat("b", 64)+"\n",
	), 0o600); err != nil {
		t.Fatalf("write release env: %v", err)
	}

	capabilities, err := provisioner.AvailableCapabilities(context.Background())
	if err != nil {
		t.Fatalf("available capabilities: %v", err)
	}
	if !capabilities.Enabled || !reflect.DeepEqual(capabilities.SupportedApplicationCodes, []string{"customer_and_opportunity"}) {
		t.Fatalf("available application codes = %#v", capabilities.SupportedApplicationCodes)
	}
	if len(capabilities.Targets) != 1 || capabilities.Targets[0].ApplicationCode != "customer_and_opportunity" {
		t.Fatalf("available targets = %#v", capabilities.Targets)
	}

	contractDigest := "registry.local/uip/contract@sha256:" + strings.Repeat("c", 64)
	if err := os.WriteFile(releasePath, []byte(
		"CUSTOMER_CRM_IMAGE="+digest+"\n"+
			"CONTRACT_IMAGE="+contractDigest+"\n"+
			"CONTRACT_MIGRATE_IMAGE=registry.local/uip/contract-migrate@sha256:"+strings.Repeat("b", 64)+"\n",
	), 0o600); err != nil {
		t.Fatalf("update release env: %v", err)
	}
	capabilities, err = provisioner.AvailableCapabilities(context.Background())
	if err != nil {
		t.Fatalf("updated available capabilities: %v", err)
	}
	if !reflect.DeepEqual(capabilities.SupportedApplicationCodes, []string{"customer_and_opportunity", "contract_management"}) {
		t.Fatalf("updated available application codes = %#v", capabilities.SupportedApplicationCodes)
	}
}
