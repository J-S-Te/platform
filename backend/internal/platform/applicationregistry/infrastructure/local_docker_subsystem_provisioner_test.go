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
