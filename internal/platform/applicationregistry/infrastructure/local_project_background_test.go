package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalProjectBackgroundBuildComesFromApprovedImagePin(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	for _, service := range []string{"project-api", "sla-notifier"} {
		environment := localProjectBackgroundEnvironment(service, image)
		if environment["PROJECT_BACKGROUND_BUILD_ID"] != "project-background-sha256-"+strings.Repeat("a", 64) {
			t.Fatalf("%s lost pinned build identity", service)
		}
		if err := validateLocalProjectBackgroundEnvironment(service, image, environment); err != nil {
			t.Fatal(err)
		}
		environment["PROJECT_BACKGROUND_BUILD_ID"] = "project-background-sha256-" + strings.Repeat("b", 64)
		if err := validateLocalProjectBackgroundEnvironment(service, image, environment); err == nil {
			t.Fatal("independent build pin accepted")
		}
	}
	if environment := localProjectBackgroundEnvironment("customer-api", image); len(environment) != 0 {
		t.Fatal("project configuration leaked into another application")
	}
}

func TestLocalProjectBackgroundOverrideCannotInjectConfiguration(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	for _, service := range []string{"project-api", "sla-notifier", "contract-api"} {
		for _, key := range []string{"PROJECT_BACKGROUND_ENGINE", "MYSQL_DSN", "COMMERCIAL_LICENSE_CLIENT_SECRET"} {
			environment := map[string]string{key: "untrusted"}
			if err := validateLocalProjectBackgroundEnvironment(service, image, environment); err == nil {
				t.Fatalf("accepted %s for %s", key, service)
			}
		}
	}
	if err := validateLocalProjectBackgroundEnvironment("project-api", image, nil); err != nil {
		t.Fatal("old pin record without background field is not readable")
	}
}

func TestLocalProjectBackgroundReceivesItsOwnNotificationIdentity(t *testing.T) {
	input := application.SubsystemProvisioningInput{ApplicationCode: "project_management"}
	values := map[string]string{}
	if err := projectNotificationRuntimeValues(input, values); err == nil {
		t.Fatal("missing machine identity silently enabled delivery")
	}
	credential := application.SubsystemServiceCredential{Purpose: application.ServiceCredentialNotificationIngest, PlaintextSecret: "isolated-test-value"}
	credential.OAuthClient.ClientID = "project-notifications"
	input.ServiceCredentials = append(input.ServiceCredentials, credential)
	if err := projectNotificationRuntimeValues(input, values); err != nil {
		t.Fatal(err)
	}
	if values["NOTIFICATION_ENABLED"] != "true" || values["PLATFORM_NOTIFICATION_CLIENT_ID"] != "project-notifications" || values["PLATFORM_NOTIFICATION_CLIENT_SECRET"] != credential.PlaintextSecret {
		t.Fatal("dedicated project credential was not wired")
	}
}

// Inspection and control must use the same Compose inputs and approved pins as mutations.
type localBackgroundOutputRunner struct {
	localLicenseRunner
	args, env []string
}

func (r *localBackgroundOutputRunner) RunOutput(_ context.Context, _ string, env []string, _ string, args ...string) ([]byte, error) {
	r.args = append([]string(nil), args...)
	r.env = append([]string(nil), env...)
	return []byte("owned-background-container"), nil
}
func TestLocalProjectBackgroundInspectionUsesIntegratedPinnedCompose(t *testing.T) {
	p, _, _, _ := localLicenseFixture(t)
	runner := &localBackgroundOutputRunner{}
	p.runner = runner
	path := filepath.Join(p.localPlatformRoot(), "runtime", "local-license.compose.json")
	override := localPinnedCompose{
		Services: map[string]localPinnedService{"sla-notifier": {
			Image: "sha256:" + strings.Repeat("a", 64), PullPolicy: "never",
			EnvFile: []string{filepath.Join(p.localPlatformRoot(), "runtime", "license-sla-notifier.env")},
			Volumes: []string{"license-state-sla-notifier:/var/lib/commercial-license", filepath.Join(p.localPlatformRoot(), "data", "keys") + ":/app/data/keys:ro"},
		}}, Volumes: map[string]struct{}{"license-state-sla-notifier": {}},
	}
	apiPin := override.Services["sla-notifier"]
	apiPin.EnvFile = []string{filepath.Join(p.localPlatformRoot(), "runtime", "license-project-api.env")}
	apiPin.Volumes = []string{"license-state-project-api:/var/lib/commercial-license", filepath.Join(p.localPlatformRoot(), "data", "keys") + ":/app/data/keys:ro"}
	override.Services["project-api"] = apiPin
	override.Volumes["license-state-project-api"] = struct{}{}

	content, err := json.Marshal(override)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}

	out, err := p.runIntegratedPlatformComposeOutput(context.Background(), "ps", "-q", "sla-notifier")
	if err != nil || string(out) != "owned-background-container" {
		t.Fatalf("inspection unavailable: %v", err)
	}
	if !strings.Contains(strings.Join(runner.args, " "), "-f "+path+" ps -q sla-notifier") {
		t.Fatal("inspection lost approved pin file")
	}
	if !strings.Contains(strings.Join(runner.env, "\n"), "PROJECT_RUNTIME_ENV_FILE=") {
		t.Fatal("inspection lost trusted runtime environment")
	}
}

type localBackgroundLifecycleRunner struct {
	localLicenseRunner
	failure  string
	controls []string
}

func (r *localBackgroundLifecycleRunner) RunOutput(ctx context.Context, dir string, env []string, command string, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if len(args) > 0 && args[0] == "inspect" {
		switch args[2] {
		case "{{.Image}}":
			return []byte(r.image.ID), nil
		case `{{index .Config.Labels "io.uip.project-background.protocol"}}`:
			return []byte("1"), nil
		}
	}
	if strings.Contains(joined, " config --format json project-api sla-notifier") {
		env := map[string]string{"PROJECT_BACKGROUND_BUILD_ID": "project-background-sha256-" + strings.Repeat("a", 64), "TEMPORAL_ADDRESS": "temporal:7233", "TEMPORAL_NAMESPACE": "default"}
		service := map[string]any{"environment": env, "depends_on": map[string]any{"temporal": map[string]string{"condition": "service_healthy"}}, "healthcheck": map[string]any{"test": []string{"CMD", "wget", "http://127.0.0.1:9093/healthz"}}}
		return json.Marshal(map[string]any{"services": map[string]any{"project-api": service, "sla-notifier": service}})
	}
	if strings.Contains(joined, " ps -q sla-notifier") {
		return []byte("owned-container"), nil
	}
	if strings.Contains(joined, "/usr/local/bin/project-background-control") {
		action := ""
		for i, a := range args {
			if a == "--action" {
				action = args[i+1]
			}
		}
		r.controls = append(r.controls, action)
		if action == r.failure {
			return nil, errors.New("isolated control failure")
		}
		return json.Marshal(map[string]any{"protocol": 1, "engine": "temporal", "tenant_id": "tenant-a", "environment": "dev", "build_id": "project-background-sha256-" + strings.Repeat("a", 64), "bootstrap_ready": true, "routing_ready": true, "execution_ready": true, "active_activities": 0, "active_leases": 0, "drained": true})
	}
	return r.localLicenseRunner.RunOutput(ctx, dir, env, command, args...)
}
func TestLocalProjectDeploymentCannotMigrateWhenBackgroundDrainFails(t *testing.T) {
	p, _, input, _ := localLicenseFixture(t)
	input.ApplicationCode = "project_management"
	input.TenantID = "tenant-a"
	input.Environment = "dev"
	if err := os.WriteFile(filepath.Join(p.config.ProjectsRoot, "project_management", ".env.local"), []byte("OIDC_TENANT_ID=tenant-a\nPLATFORM_ENVIRONMENT_CODE=dev\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &localBackgroundLifecycleRunner{localLicenseRunner: localLicenseRunner{image: localImageMetadata{ID: "sha256:" + strings.Repeat("a", 64), Version: "local", Protocol: "1"}}, failure: "drain"}
	p.runner = runner
	if err := p.deployLicensedLocalStack(context.Background(), input); err == nil {
		t.Fatal("drain failure was accepted")
	}
	for _, args := range runner.calls {
		call := strings.Join(args, " ")
		if strings.Contains(call, "run --rm --no-deps project-migrate") || strings.Contains(call, "--pull never project-api") {
			t.Fatal("replaced project before drain")
		}
	}
	if strings.Join(runner.controls, ",") != "inspect,pause,drain" {
		t.Fatalf("missing actual lifecycle boundary: %v", runner.controls)
	}
}
