package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
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
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED":   "true",
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
		"PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED=true",
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

type noOpSubsystemRunner struct{}

func (noOpSubsystemRunner) Run(_ context.Context, _ string, _ []string, _ string, _ ...string) error {
	return nil
}

type recordingSubsystemRunnerCall struct {
	directory    string
	environment  []string
	binary       string
	arguments    []string
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

func TestLocalDockerSubsystemProvisionerUpdateRebuildsWithoutTouchingGatewayOrEnvFile(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	project := filepath.Join(root, "contract_management")
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
		ApplicationCode: "contract_management",
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
	project := filepath.Join(root, "contract_management")
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

	if err := provisioner.Teardown(context.Background(), "contract_management", "prod"); err != nil {
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
	if gatewayCall.arguments[2] != "contract_management" {
		t.Fatalf("gateway call application code = %q, want contract_management", gatewayCall.arguments[2])
	}

	if _, err := os.Stat(envPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".env.local still exists after teardown: err = %v", err)
	}
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

	if err := provisioner.Teardown(context.Background(), "missing_subsystem", "prod"); err != nil {
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
