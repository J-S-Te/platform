package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func TestUpdateSubsystemEnvironmentPreservesUnmanagedValuesAndProtectsSecrets(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	source := filepath.Join(directory, ".env.example")
	destination := filepath.Join(directory, ".env.local")
	content := strings.Join([]string{
		"CUSTOM_SETTING=keep-me",
		"CONTRACT_MYSQL_PASSWORD=REPLACE_WITH_CONTRACT_PASSWORD",
		"OIDC_CLIENT_ID=REPLACE_WITH_ONBOARDING_CLIENT_ID",
		"OIDC_CLIENT_SECRET=REPLACE_WITH_ONBOARDING_CLIENT_SECRET",
		"",
	}, "\n")
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatalf("write environment template: %v", err)
	}

	if err := updateSubsystemEnvironment(source, destination, map[string]string{
		"OIDC_CLIENT_ID":                                "contract_management-dev-web",
		"OIDC_CLIENT_SECRET":                            "generated-oauth-secret",
		"OIDC_REDIRECT_URI":                             "http://localhost:8081/contract_management/auth/callback",
		"PLATFORM_APPLICATION_ID":                       "app-1",
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "false",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID":      "contract_management-dev-catalog-publisher",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET":  "catalog-publisher-secret",
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID": "app-1",
	}); err != nil {
		t.Fatalf("update environment: %v", err)
	}

	resultBytes, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read generated environment: %v", err)
	}
	result := string(resultBytes)
	for _, expected := range []string{
		"CUSTOM_SETTING=keep-me",
		"OIDC_CLIENT_ID=contract_management-dev-web",
		"OIDC_CLIENT_SECRET=generated-oauth-secret",
		"OIDC_REDIRECT_URI=http://localhost:8081/contract_management/auth/callback",
		"PLATFORM_APPLICATION_ID=app-1",
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED=false",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID=contract_management-dev-catalog-publisher",
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET=catalog-publisher-secret",
		"PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID=app-1",
	} {
		if !strings.Contains(result, expected) {
			t.Fatalf("generated environment missing %q:\n%s", expected, result)
		}
	}
	if strings.Contains(result, "REPLACE_WITH_CONTRACT_PASSWORD") {
		t.Fatal("database password placeholder was not replaced")
	}
	passwordPattern := regexp.MustCompile(`(?m)^CONTRACT_MYSQL_PASSWORD=([0-9a-f]{64})$`)
	if !passwordPattern.MatchString(result) {
		t.Fatalf("database password was not generated as a 256-bit hexadecimal value:\n%s", result)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat generated environment: %v", err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("environment permissions = %o, want 600", permission)
	}
}

func TestUpdateProductionSubsystemEnvironmentPreservesOwnerAndRejectsDuplicateManagedKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("OIDC_CLIENT_ID=old\nUNRELATED=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateProductionSubsystemEnvironment(path, map[string]string{"OIDC_CLIENT_ID": "new"}); err != nil {
		t.Fatalf("update production environment: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if beforeOK && afterOK && (beforeStat.Uid != afterStat.Uid || beforeStat.Gid != afterStat.Gid) {
		t.Fatalf("environment owner changed from %d:%d to %d:%d", beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), "OIDC_CLIENT_ID=new") || !strings.Contains(string(contents), "UNRELATED=value") {
		t.Fatalf("unexpected environment contents: %v %s", err, contents)
	}

	if err := os.WriteFile(path, []byte("OIDC_CLIENT_ID=one\nOIDC_CLIENT_ID=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updateProductionSubsystemEnvironment(path, map[string]string{"OIDC_CLIENT_ID": "new"}); err == nil {
		t.Fatal("duplicate managed environment key was accepted")
	}
}

func TestProjectDirectoryRejectsTraversalAndSymlinkEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "contract_management"), 0o755); err != nil {
		t.Fatalf("create valid project: %v", err)
	}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled:      true,
		ProjectsRoot: root,
		Timeout:      time.Minute,
	}, noOpSubsystemRunner{})
	if err != nil {
		t.Fatalf("construct provisioner: %v", err)
	}

	valid, err := provisioner.projectDirectory("contract_management")
	if err != nil {
		t.Fatalf("valid project directory rejected: %v", err)
	}
	expectedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temporary root: %v", err)
	}
	if valid != filepath.Join(expectedRoot, "contract_management") {
		t.Fatalf("valid directory = %q", valid)
	}
	if _, err := provisioner.projectDirectory("../outside"); !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("traversal error = %v, want provisioning sentinel", err)
	}

	link := filepath.Join(root, "escaped")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create escape symlink: %v", err)
	}
	if _, err := provisioner.projectDirectory("escaped"); !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("symlink escape error = %v, want provisioning sentinel", err)
	}
}

func TestLocalDockerSubsystemProvisionerUsesPrivateKeycloakBackchannelForRealmIssuer(t *testing.T) {
	t.Parallel()
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		KeycloakPublicURL:   "https://sso.example.com/",
		KeycloakInternalURL: "http://keycloak:8080/",
		KeycloakRealm:       "basic-platform",
	}, noOpSubsystemRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := provisioner.oidcBackchannelBaseURL("https://sso.example.com/realms/basic-platform"), "http://keycloak:8080"; got != want {
		t.Fatalf("Keycloak backchannel = %q, want %q", got, want)
	}
	if got := provisioner.oidcBackchannelBaseURL("https://identity.example.com"); got != "" {
		t.Fatalf("non-Keycloak issuer backchannel = %q, want empty", got)
	}
}

type noOpSubsystemRunner struct{}

func (noOpSubsystemRunner) Run(_ context.Context, _ string, _ []string, _ string, _ ...string) error {
	return nil
}

type recordingSubsystemRunnerCall struct {
	directory   string
	environment []string
	binary      string
	arguments   []string
}

type recordingSubsystemRunner struct {
	calls  []recordingSubsystemRunnerCall
	errors map[string]error
}

func (runner *recordingSubsystemRunner) Run(_ context.Context, directory string, environment []string, name string, arguments ...string) error {
	runner.calls = append(runner.calls, recordingSubsystemRunnerCall{
		directory: directory, environment: environment, binary: name, arguments: append([]string(nil), arguments...),
	})
	if runner.errors == nil {
		return nil
	}
	key := strings.Join(append([]string{name}, arguments...), " ")
	return runner.errors[key]
}

func (runner *recordingSubsystemRunner) firstCallMatching(t *testing.T, predicate func(call recordingSubsystemRunnerCall) bool) recordingSubsystemRunnerCall {
	t.Helper()
	for _, call := range runner.calls {
		if predicate(call) {
			return call
		}
	}
	t.Fatalf("no recorded call matched predicate; recorded calls: %#v", runner.calls)
	return recordingSubsystemRunnerCall{}
}

func TestLocalDockerSubsystemProvisionerUpdateRebuildsStandaloneSubsystemWithoutTouchingGatewayOrEnvFile(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	project := filepath.Join(root, "customer_management")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatalf("create project: %v", err)
	}
	composePath := filepath.Join(project, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	envPath := filepath.Join(project, ".env.local")
	if err := os.WriteFile(envPath, []byte("OIDC_CLIENT_ID=original\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled:                 true,
		ProjectsRoot:            root,
		GatewayScriptPath:       "/nonexistent/gateway.sh",
		GatewayIncludePath:      "/nonexistent/gateway.conf.d",
		PlatformComposeProject:  "platform",
		PlatformFrontendService: "frontend",
		PlatformDockerNetwork:   "platform-net",
		Timeout:                 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatalf("construct provisioner: %v", err)
	}

	if err := provisioner.Update(context.Background(), application.SubsystemProvisioningInput{
		ApplicationCode: "customer_management",
		Environment:     "prod",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	upCall := runner.firstCallMatching(t, func(call recordingSubsystemRunnerCall) bool {
		return call.binary == "docker" && len(call.arguments) > 0 && call.arguments[0] == "compose"
	})
	if upCall.directory != project {
		t.Fatalf("compose up directory = %q, want %q", upCall.directory, project)
	}
	if !containsString(upCall.arguments, "--env-file") {
		t.Fatalf("compose up missing --env-file: %v", upCall.arguments)
	}
	if !containsString(upCall.arguments, "up") {
		t.Fatalf("compose up missing up subcommand: %v", upCall.arguments)
	}
	if !containsString(upCall.arguments, "--build") {
		t.Fatalf("compose up missing --build: %v", upCall.arguments)
	}
	if !containsString(upCall.arguments, "-d") {
		t.Fatalf("compose up missing -d: %v", upCall.arguments)
	}

	for _, call := range runner.calls {
		if call.binary == "/bin/bash" {
			t.Fatalf("update must not call the gateway script: %v", call.arguments)
		}
		if len(call.arguments) > 0 && call.arguments[0] == "compose" && containsString(call.arguments, "down") {
			t.Fatalf("update must not call compose down: %v", call.arguments)
		}
	}

	contents, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(contents), "OIDC_CLIENT_ID=original") {
		t.Fatalf("update must not rewrite .env.local: %s", contents)
	}
}

func TestLocalDockerSubsystemProvisionerTeardownStopsContainersRemovesEnvAndGateway(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	project := filepath.Join(root, "customer_management")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatalf("create project: %v", err)
	}
	composePath := filepath.Join(project, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	envPath := filepath.Join(project, ".env.local")
	if err := os.WriteFile(envPath, []byte("OIDC_CLIENT_ID=original\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	gatewayDir := t.TempDir()
	gatewayScript := filepath.Join(gatewayDir, "portal-gateway.sh")
	if err := os.WriteFile(gatewayScript, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write gateway script: %v", err)
	}

	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled:                 true,
		ProjectsRoot:            root,
		GatewayScriptPath:       gatewayScript,
		GatewayIncludePath:      filepath.Join(gatewayDir, "includes.d"),
		PlatformComposeProject:  "platform",
		PlatformFrontendService: "frontend",
		Timeout:                 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatalf("construct provisioner: %v", err)
	}

	if err := provisioner.Teardown(context.Background(), "tenant-1", "customer_management", "prod"); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	downCall := runner.firstCallMatching(t, func(call recordingSubsystemRunnerCall) bool {
		return call.binary == "docker" && len(call.arguments) > 0 && call.arguments[0] == "compose" && containsString(call.arguments, "down")
	})
	if !containsString(downCall.arguments, "--remove-orphans") {
		t.Fatalf("compose down missing --remove-orphans: %v", downCall.arguments)
	}

	gatewayCall := runner.firstCallMatching(t, func(call recordingSubsystemRunnerCall) bool {
		return call.binary == "/bin/bash" && len(call.arguments) >= 2 && call.arguments[0] == gatewayScript && call.arguments[1] == "remove"
	})
	if !containsString(gatewayCall.environment, "PORTAL_GATEWAY_NGINX_INCLUDE="+filepath.Join(gatewayDir, "includes.d")) {
		t.Fatalf("gateway call did not pass PORTAL_GATEWAY_NGINX_INCLUDE: %v", gatewayCall.environment)
	}
	if gatewayCall.arguments[2] != "customer_management" {
		t.Fatalf("gateway call application code = %q, want customer_management", gatewayCall.arguments[2])
	}

	if _, err := os.Stat(envPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".env.local still exists after teardown: err = %v", err)
	}
}

func TestLocalDockerSubsystemProvisionerUpdateUsesUnifiedContractCompose(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	platformRoot := filepath.Join(root, "platform")
	project := filepath.Join(root, "contract_management")
	for _, directory := range []string{filepath.Join(platformRoot, "scripts"), filepath.Join(platformRoot, "docker"), project} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create directory %s: %v", directory, err)
		}
	}
	for path, contents := range map[string]string{
		filepath.Join(platformRoot, "compose.local.yaml"):            "services: {}\n",
		filepath.Join(platformRoot, "docker", ".env.local"):          "PLATFORM_SETTING=keep\n",
		filepath.Join(platformRoot, "docker", ".env.customer.local"): "CUSTOMER_SETTING=keep\n",
		filepath.Join(platformRoot, "scripts", "portal-gateway.sh"):  "#!/bin/sh\n",
		filepath.Join(project, "docker-compose.yml"):                 "services: {}\n",
		filepath.Join(project, ".env.local"):                         "OIDC_CLIENT_ID=contract_management-prod-web\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root,
		GatewayScriptPath:      filepath.Join(platformRoot, "scripts", "portal-gateway.sh"),
		PlatformComposeProject: "basic-platform-local", Timeout: 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatalf("construct provisioner: %v", err)
	}
	if err := provisioner.Update(context.Background(), application.SubsystemProvisioningInput{
		ApplicationCode: "contract_management", Environment: "prod",
	}); err != nil {
		t.Fatalf("update integrated contract subsystem: %v", err)
	}

	if len(runner.calls) != 3 {
		t.Fatalf("integrated contract update calls = %d, want 3: %#v", len(runner.calls), runner.calls)
	}
	for _, call := range runner.calls {
		if call.directory != platformRoot {
			t.Fatalf("compose directory = %q, want %q", call.directory, platformRoot)
		}
		for _, expected := range []string{
			"BASIC_PLATFORM_RUNTIME_ENV_FILE=" + filepath.Join(platformRoot, "docker", ".env.local"),
			"CONTRACT_RUNTIME_ENV_FILE=" + filepath.Join(project, ".env.local"),
			"CUSTOMER_RUNTIME_ENV_FILE=" + filepath.Join(platformRoot, "docker", ".env.customer.local"),
			"BASIC_PLATFORM_LAN_OVERRIDE_ENV_FILE=" + filepath.Join(platformRoot, "docker", ".env.lan.disabled"),
			"CONTRACT_LAN_OVERRIDE_ENV_FILE=" + filepath.Join(platformRoot, "docker", ".env.lan.disabled"),
			"CUSTOMER_LAN_OVERRIDE_ENV_FILE=" + filepath.Join(platformRoot, "docker", ".env.lan.disabled"),
			"PORTAL_LAN_OVERRIDE_ENV_FILE=" + filepath.Join(platformRoot, "docker", ".env.lan.disabled"),
			"PROJECT_LAN_OVERRIDE_ENV_FILE=" + filepath.Join(platformRoot, "docker", ".env.lan.disabled"),
			"BASIC_PLATFORM_HOST_PROJECT_ROOT=" + platformRoot,
			"SUBSYSTEM_HOST_PROJECTS_ROOT=" + root,
		} {
			if !containsString(call.environment, expected) {
				t.Fatalf("compose environment missing %q: %v", expected, call.environment)
			}
		}
		if !containsString(call.arguments, "--project-name") || !containsString(call.arguments, "basic-platform-local") {
			t.Fatalf("compose call missing unified project name: %v", call.arguments)
		}
		if containsString(call.arguments, filepath.Join(project, "docker-compose.yml")) {
			t.Fatalf("integrated update must not use standalone contract Compose: %v", call.arguments)
		}
	}
	if !containsString(runner.calls[0].arguments, "contract-mysql") || !containsString(runner.calls[0].arguments, "temporal") {
		t.Fatalf("first call must ensure integrated dependencies: %v", runner.calls[0].arguments)
	}
	if !containsString(runner.calls[0].arguments, "--no-deps") {
		t.Fatalf("dependency startup must not recreate platform services: %v", runner.calls[0].arguments)
	}
	if !containsString(runner.calls[1].arguments, "contract-migrate") {
		t.Fatalf("second call must run integrated migrations: %v", runner.calls[1].arguments)
	}
	if !containsString(runner.calls[2].arguments, "contract-api") || !containsString(runner.calls[2].arguments, "--build") {
		t.Fatalf("third call must rebuild integrated contract API: %v", runner.calls[2].arguments)
	}
}

func TestLocalDockerSubsystemProvisionerProvisionIntegratedContractDoesNotReloadGateway(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	platformRoot := filepath.Join(root, "platform")
	project := filepath.Join(root, "contract_management")
	for _, directory := range []string{filepath.Join(platformRoot, "scripts"), filepath.Join(platformRoot, "docker"), project} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create directory %s: %v", directory, err)
		}
	}
	for path, contents := range map[string]string{
		filepath.Join(platformRoot, "compose.local.yaml"):            "services: {}\n",
		filepath.Join(platformRoot, "docker", ".env.local"):          "PLATFORM_SETTING=keep\n",
		filepath.Join(platformRoot, "docker", ".env.customer.local"): "CUSTOMER_SETTING=keep\n",
		filepath.Join(platformRoot, "scripts", "portal-gateway.sh"):  "#!/bin/sh\n",
		filepath.Join(project, "docker-compose.yml"):                 "services: {}\n",
		filepath.Join(project, ".env.example"):                       "CONTRACT_MYSQL_PASSWORD=REPLACE_WITH_CONTRACT_PASSWORD\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root,
		GatewayScriptPath:      filepath.Join(platformRoot, "scripts", "portal-gateway.sh"),
		GatewayIncludePath:     filepath.Join(platformRoot, "docker", "portal-apps-locations.conf"),
		PlatformComposeProject: "basic-platform-local", Timeout: 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatalf("construct provisioner: %v", err)
	}
	if err := provisioner.Provision(context.Background(), application.SubsystemProvisioningInput{
		TenantID: "tenant-1", ApplicationID: "app-1", ApplicationCode: "contract_management", Environment: "prod",
		Issuer: "http://localhost:8081", ClientID: "contract_management-prod-web", ClientSecret: "browser-secret",
		RedirectURI: "http://localhost:8081/contract_management/auth/callback", PublicURL: "http://localhost:8081/contract_management/",
		PathPrefix: "/contract_management", UpstreamURL: "http://contract-api:8081",
		CatalogPublisherClientID: "contract_management-prod-catalog-publisher", CatalogPublisherClientSecret: "publisher-secret",
		ServiceCredentials: []application.SubsystemServiceCredential{
			{Purpose: application.ServiceCredentialAuditIngest, OAuthClient: application.OAuthClientView{ClientID: "contract_management-prod-audit-publisher"}, PlaintextSecret: "audit-secret"},
			{Purpose: application.ServiceCredentialContractOpportunitySignedWrite, OAuthClient: application.OAuthClientView{ClientID: "contract_management-prod-opportunity-intake"}, PlaintextSecret: "intake-secret"},
			{Purpose: application.ServiceCredentialContractSummaryRead, OAuthClient: application.OAuthClientView{ClientID: "contract_management-prod-contract-summary"}, PlaintextSecret: "summary-secret"},
			{Purpose: application.ServiceCredentialOwnerDirectoryRead, OAuthClient: application.OAuthClientView{ClientID: "contract_management-prod-owner-directory"}, PlaintextSecret: "directory-secret"},
		},
	}); err != nil {
		t.Fatalf("provision integrated contract subsystem: %v", err)
	}

	if len(runner.calls) != 3 {
		t.Fatalf("integrated contract provision calls = %d, want 3 unified Compose calls: %#v", len(runner.calls), runner.calls)
	}
	for _, call := range runner.calls {
		if call.binary == "/bin/bash" || containsString(call.arguments, "nginx") {
			t.Fatalf("integrated contract provisioning must not mutate or reload the current gateway: %#v", call)
		}
	}
}

func TestLocalDockerSubsystemProvisionerPreflightIntegratedCustomerDoesNotRequireStandaloneCompose(t *testing.T) {
	t.Parallel()
	root, platformRoot, project, gatewayScript := createIntegratedProvisionerFixture(t, integratedCustomerApplicationCode)
	if err := os.WriteFile(filepath.Join(project, ".env.example"), []byte("DEV_AUTH_ENABLED=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root, GatewayScriptPath: gatewayScript,
		GatewayIncludePath:     filepath.Join(platformRoot, "docker", "portal-apps-locations.conf"),
		PlatformComposeProject: "basic-platform-local", Timeout: 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Preflight(context.Background(), application.SubsystemPreflightInput{ApplicationCode: integratedCustomerApplicationCode}); err != nil {
		t.Fatalf("integrated customer preflight rejected without standalone Compose: %v", err)
	}
	if len(runner.calls) != 1 || !containsString(runner.calls[0].arguments, "version") {
		t.Fatalf("unexpected preflight calls: %#v", runner.calls)
	}
}

func TestLocalDockerSubsystemProvisionerProvisionIntegratedCustomerWritesSharedEnvPublishesCatalogAndSkipsGateway(t *testing.T) {
	t.Parallel()
	root, platformRoot, _, gatewayScript := createIntegratedProvisionerFixture(t, integratedCustomerApplicationCode)
	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root, GatewayScriptPath: gatewayScript,
		GatewayIncludePath:     filepath.Join(platformRoot, "docker", "portal-apps-locations.conf"),
		PlatformComposeProject: "basic-platform-local", Timeout: 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Provision(context.Background(), application.SubsystemProvisioningInput{
		TenantID: "tenant-1", ApplicationID: "app-1", ApplicationCode: integratedCustomerApplicationCode, Environment: "dev",
		Issuer: "http://localhost:8081", ClientID: "customer_and_opportunity-dev-web", ClientSecret: "browser-secret",
		RedirectURI: "http://localhost:8081/customer-opportunity/auth/callback", PublicURL: "http://localhost:8081/customer-opportunity/",
		PathPrefix: "/customer-opportunity", UpstreamURL: "http://customer-api:8090",
		CatalogPublisherClientID: "customer_and_opportunity-dev-catalog-publisher", CatalogPublisherClientSecret: "publisher-secret",
		ServiceCredentials: []application.SubsystemServiceCredential{{Purpose: application.ServiceCredentialAuditIngest, OAuthClient: application.OAuthClientView{ClientID: "customer_and_opportunity-dev-audit-publisher"}, PlaintextSecret: "audit-secret"}},
	}); err != nil {
		t.Fatalf("provision integrated customer subsystem: %v", err)
	}

	contents, readErr := os.ReadFile(filepath.Join(platformRoot, "docker", ".env.customer.local"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, expected := range []string{
		"DEV_AUTH_ENABLED=false", "APP_PATH_PREFIX=/customer-opportunity", "APP_PUBLIC_ORIGIN=http://localhost:8081",
		"PLATFORM_BASE_URL=http://platform-api:8080",
		"OIDC_CLIENT_ID=customer_and_opportunity-dev-web", "OIDC_SESSION_COOKIE_SECURE=false", "OIDC_ROLE_CONFIG_HASH=" + integratedCustomerRoleConfigHash,
		"PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID=customer_and_opportunity-dev-catalog-publisher",
		"PLATFORM_AUDIT_CLIENT_ID=customer_and_opportunity-dev-audit-publisher",
	} {
		if !strings.Contains(string(contents), expected) {
			t.Fatalf("shared customer environment missing %q:\n%s", expected, contents)
		}
	}
	if len(runner.calls) != 5 {
		t.Fatalf("integrated customer provision calls = %d, want 5: %#v", len(runner.calls), runner.calls)
	}
	publishCall := runner.firstCallMatching(t, func(call recordingSubsystemRunnerCall) bool {
		return containsString(call.arguments, "./authz-catalog") && containsString(call.arguments, "publish")
	})
	if !containsString(publishCall.arguments, "crm") {
		t.Fatalf("catalog publish call missing CRM manifest: %v", publishCall.arguments)
	}
	for _, call := range runner.calls {
		if call.binary == "/bin/bash" || containsString(call.arguments, "nginx") {
			t.Fatalf("integrated customer provisioning must not mutate the generated gateway: %#v", call)
		}
		if !containsString(call.environment, "CUSTOMER_RUNTIME_ENV_FILE="+filepath.Join(platformRoot, "docker", ".env.customer.local")) {
			t.Fatalf("compose environment missing customer runtime file: %v", call.environment)
		}
	}
}

func TestLocalDockerSubsystemProvisionerProvisionIntegratedPortalWritesIsolatedRuntimeAndCRMIntegration(t *testing.T) {
	t.Parallel()
	root, platformRoot, _, gatewayScript := createIntegratedProvisionerFixture(t, integratedPortalApplicationCode)
	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root, GatewayScriptPath: gatewayScript,
		GatewayIncludePath:     filepath.Join(platformRoot, "docker", "portal-apps-locations.conf"),
		PlatformComposeProject: "basic-platform-local", Timeout: 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	credentials := portalServiceCredentialsForTest()
	if err := provisioner.Provision(context.Background(), application.SubsystemProvisioningInput{
		TenantID: "tenant-1", ApplicationID: "portal-app-1", ApplicationCode: integratedPortalApplicationCode, Environment: "dev",
		Issuer: "http://localhost:8081", ClientID: "customer_portal-dev-web", ClientSecret: "portal-browser-secret",
		RedirectURI: "http://localhost:8081/customer-portal/auth/callback", PublicURL: "http://localhost:8081/customer-portal/",
		PathPrefix: "/customer-portal", UpstreamURL: "http://portal-api:8091",
		CatalogPublisherClientID: "customer_portal-dev-catalog-publisher", CatalogPublisherClientSecret: "portal-publisher-secret",
		ServiceCredentials: credentials,
	}); err != nil {
		t.Fatalf("provision integrated Portal subsystem: %v", err)
	}

	portalBytes, err := os.ReadFile(filepath.Join(platformRoot, "docker", ".env.portal.local"))
	if err != nil {
		t.Fatal(err)
	}
	portalEnvironment := string(portalBytes)
	for _, expected := range []string{
		"PORTAL_OIDC_CLIENT_ID=customer_portal-dev-web",
		"PORTAL_ROLE_CONFIG_HASH=" + integratedPortalRoleConfigHash,
		"PORTAL_CRM_INVITE_CLIENT_ID=customer_portal-dev-portal-invite-verify",
		"PORTAL_CRM_PROVISION_CLIENT_SUBJECT=customer_portal-dev-portal-mapping-provision",
		"PORTAL_CRM_DISABLE_CLIENT_SUBJECT=customer_portal-dev-portal-mapping-disable",
		"PORTAL_AUTHORIZATION_CATALOG_CLIENT_ID=customer_portal-dev-catalog-publisher",
		"PLATFORM_AUDIT_CLIENT_ID=customer_portal-dev-audit-publisher",
	} {
		if !strings.Contains(portalEnvironment, expected) {
			t.Fatalf("Portal environment missing %q:\n%s", expected, portalEnvironment)
		}
	}
	if strings.Contains(portalEnvironment, "REPLACE_WITH_") {
		t.Fatalf("Portal environment still contains a secret placeholder:\n%s", portalEnvironment)
	}
	passwordMatch := regexp.MustCompile(`(?m)^PORTAL_MYSQL_PASSWORD=([0-9a-f]{64})$`).FindStringSubmatch(portalEnvironment)
	if len(passwordMatch) != 2 || !strings.Contains(portalEnvironment, "PORTAL_MYSQL_DSN=\"portal:"+passwordMatch[1]+"@tcp(portal-mysql:3306)") {
		t.Fatalf("Portal DSN does not use the generated database password:\n%s", portalEnvironment)
	}

	customerBytes, err := os.ReadFile(filepath.Join(platformRoot, "docker", ".env.customer.local"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"PORTAL_INVITE_ENABLED=true",
		"PLATFORM_EXTERNAL_IDENTITY_ENABLED=true",
		"PLATFORM_PORTAL_APPLICATION_CODE=customer_portal",
		"PLATFORM_EXTERNAL_USER_CLIENT_ID=customer_portal-dev-external-user-provision",
		"PLATFORM_ROLE_ASSIGN_CLIENT_ID=customer_portal-dev-role-assign",
		"PLATFORM_ROLE_REVOKE_CLIENT_ID=customer_portal-dev-role-revoke",
	} {
		if !strings.Contains(string(customerBytes), expected) {
			t.Fatalf("CRM environment missing %q:\n%s", expected, customerBytes)
		}
	}
	if len(runner.calls) != 6 {
		t.Fatalf("integrated Portal provision calls = %d, want 6: %#v", len(runner.calls), runner.calls)
	}
	publishCall := runner.firstCallMatching(t, func(call recordingSubsystemRunnerCall) bool {
		return containsString(call.arguments, "./authz-catalog") && containsString(call.arguments, "publish")
	})
	if !containsString(publishCall.arguments, "portal") {
		t.Fatalf("catalog publish call missing Portal manifest: %v", publishCall.arguments)
	}
	for _, call := range runner.calls {
		if call.binary == "/bin/bash" || containsString(call.arguments, "nginx") {
			t.Fatalf("integrated Portal provisioning must not mutate the generated gateway: %#v", call)
		}
		if !containsString(call.environment, "PORTAL_RUNTIME_ENV_FILE="+filepath.Join(platformRoot, "docker", ".env.portal.local")) {
			t.Fatalf("compose environment missing Portal runtime file: %v", call.environment)
		}
	}
}

func TestLocalDockerSubsystemProvisionerPortalRequiresEveryPurposeBoundCredential(t *testing.T) {
	t.Parallel()
	root, platformRoot, _, gatewayScript := createIntegratedProvisionerFixture(t, integratedPortalApplicationCode)
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled: true, ProjectsRoot: root, GatewayScriptPath: gatewayScript,
		GatewayIncludePath: filepath.Join(platformRoot, "docker", "portal-apps-locations.conf"), Timeout: 30 * time.Second,
	}, &recordingSubsystemRunner{})
	if err != nil {
		t.Fatal(err)
	}
	credentials := portalServiceCredentialsForTest()
	credentials = credentials[:len(credentials)-1]
	err = provisioner.Provision(context.Background(), application.SubsystemProvisioningInput{
		TenantID: "tenant-1", ApplicationID: "portal-app-1", ApplicationCode: integratedPortalApplicationCode, Environment: "dev",
		Issuer: "http://localhost:8081", ClientID: "customer_portal-dev-web", ClientSecret: "browser-secret",
		RedirectURI: "http://localhost:8081/customer-portal/auth/callback", PublicURL: "http://localhost:8081/customer-portal/",
		PathPrefix: "/customer-portal", UpstreamURL: "http://portal-api:8091", ServiceCredentials: credentials,
	})
	if !errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		t.Fatalf("missing service credential error = %v", err)
	}
}

func portalServiceCredentialsForTest() []application.SubsystemServiceCredential {
	definitions := []struct{ purpose, suffix string }{
		{application.ServiceCredentialAuditIngest, "audit-publisher"},
		{application.ServiceCredentialExternalUserProvision, "external-user-provision"},
		{application.ServiceCredentialApplicationRoleAssign, "role-assign"},
		{application.ServiceCredentialApplicationRoleRevoke, "role-revoke"},
		{application.ServiceCredentialPortalMappingProvision, "portal-mapping-provision"},
		{application.ServiceCredentialPortalMappingDisable, "portal-mapping-disable"},
		{application.ServiceCredentialPortalInviteVerify, "portal-invite-verify"},
	}
	result := make([]application.SubsystemServiceCredential, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, application.SubsystemServiceCredential{
			Purpose:         definition.purpose,
			OAuthClient:     application.OAuthClientView{ClientID: "customer_portal-dev-" + definition.suffix},
			PlaintextSecret: "secret-" + definition.suffix,
		})
	}
	return result
}

func createIntegratedProvisionerFixture(t *testing.T, applicationCode string) (string, string, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	platformRoot := filepath.Join(root, "platform")
	contractProject := filepath.Join(root, integratedContractApplicationCode)
	projectCode := applicationCode
	if applicationCode == integratedPortalApplicationCode {
		projectCode = integratedCustomerApplicationCode
	}
	project := filepath.Join(root, projectCode)
	for _, directory := range []string{filepath.Join(platformRoot, "scripts"), filepath.Join(platformRoot, "docker"), contractProject, project} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gatewayScript := filepath.Join(platformRoot, "scripts", "portal-gateway.sh")
	for path, contents := range map[string]string{
		filepath.Join(platformRoot, "compose.local.yaml"):                    "services: {}\n",
		filepath.Join(platformRoot, "docker", ".env.local"):                  "PLATFORM_SETTING=keep\n",
		filepath.Join(platformRoot, "docker", ".env.customer.local"):         "CUSTOMER_MYSQL_PASSWORD=keep\nDEV_AUTH_ENABLED=true\n",
		filepath.Join(platformRoot, "docker", ".env.customer.local.example"): "CUSTOMER_MYSQL_PASSWORD=template\n",
		filepath.Join(platformRoot, "docker", ".env.portal.local.example"): strings.Join([]string{
			"PORTAL_MYSQL_PASSWORD=REPLACE_WITH_GENERATED_PORTAL_PASSWORD",
			"PORTAL_MYSQL_ROOT_PASSWORD=REPLACE_WITH_GENERATED_PORTAL_ROOT_PASSWORD",
			"PORTAL_MYSQL_DSN=portal:REPLACE_WITH_GENERATED_PORTAL_PASSWORD@tcp(portal-mysql:3306)/customer_portal?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=true",
			"PORTAL_ENCRYPTION_KEY_BASE64=REPLACE_WITH_GENERATED_PORTAL_ENCRYPTION_KEY",
			"PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64=REPLACE_WITH_GENERATED_PORTAL_REPORT_DESCRIPTOR_KEY",
			"PORTAL_HMAC_KEY_BASE64=REPLACE_WITH_GENERATED_PORTAL_HMAC_KEY", "",
		}, "\n"),
		filepath.Join(contractProject, ".env.local"): "CONTRACT_SETTING=keep\n",
		gatewayScript: "#!/bin/sh\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, platformRoot, project, gatewayScript
}

func TestLocalDockerSubsystemProvisionerTeardownWithoutProjectDirStillRemovesGateway(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	gatewayDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve gateway dir: %v", err)
	}
	gatewayScript := filepath.Join(gatewayDir, "portal-gateway.sh")
	if err := os.WriteFile(gatewayScript, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write gateway script: %v", err)
	}

	runner := &recordingSubsystemRunner{}
	provisioner, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{
		Enabled:                 true,
		ProjectsRoot:            root,
		GatewayScriptPath:       gatewayScript,
		GatewayIncludePath:      filepath.Join(gatewayDir, "includes.d"),
		PlatformComposeProject:  "platform",
		PlatformFrontendService: "frontend",
		Timeout:                 30 * time.Second,
	}, runner)
	if err != nil {
		t.Fatalf("construct provisioner: %v", err)
	}

	if err := provisioner.Teardown(context.Background(), "tenant-1", "missing_subsystem", "prod"); err != nil {
		t.Fatalf("teardown without project: %v", err)
	}

	for _, call := range runner.calls {
		if call.binary == "docker" && len(call.arguments) > 0 && call.arguments[0] == "compose" {
			t.Fatalf("teardown must skip compose when project dir is missing: %v", call.arguments)
		}
	}
	gatewayCall := runner.firstCallMatching(t, func(call recordingSubsystemRunnerCall) bool {
		return call.binary == "/bin/bash" && len(call.arguments) >= 2 && call.arguments[1] == "remove"
	})
	if gatewayCall.arguments[2] != "missing_subsystem" {
		t.Fatalf("gateway remove code = %q, want missing_subsystem", gatewayCall.arguments[2])
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
