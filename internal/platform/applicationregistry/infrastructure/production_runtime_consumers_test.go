package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type runtimeConsumerRunner struct {
	running bool
	project string
	service string
	image   string
	oneoff  string
	count   int
	fail    string
	starts  [][]string
	pinned  map[string]map[string]string
	paths   []string
}

func (r *runtimeConsumerRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	if len(args) > 1 && args[0] == "container" && args[1] == "ls" {
		if r.fail == "inventory" {
			return nil, errors.New("sensitive output must not leak")
		}
		if !r.running {
			return nil, nil
		}
		id := strings.Repeat("a", 64)
		if r.count > 1 {
			return []byte(id + "\n" + strings.Repeat("b", 64)), nil
		}
		return []byte(id), nil
	}
	if len(args) > 1 && args[0] == "container" && args[1] == "inspect" {
		if r.fail == "inspect" {
			return nil, errors.New("sensitive output must not leak")
		}
		if strings.Contains(strings.Join(args, " "), ".Config.Env") {
			return nil, errors.New("secret environment inspection forbidden")
		}
		labels, _ := json.Marshal(map[string]string{"com.docker.compose.project": r.project, "com.docker.compose.service": r.service, "com.docker.compose.oneoff": r.oneoff})
		return []byte(`"` + strings.Repeat("a", 64) + `" "` + r.image + `" true ` + string(labels)), nil
	}
	return nil, errors.New("unexpected CLI")
}

func (r *runtimeConsumerRunner) Run(_ context.Context, _ string, _ []string, _ string, args ...string) error {
	r.starts = append(r.starts, append([]string(nil), args...))
	for index := 1; index < len(args); index++ {
		if args[index-1] == "--file" && strings.Contains(args[index], ".consumer-refresh-") {
			r.paths = append(r.paths, args[index])
			content, err := os.ReadFile(args[index])
			if err != nil {
				return err
			}
			var override struct{ Services map[string]map[string]string }
			if err = json.Unmarshal(content, &override); err != nil {
				return err
			}
			r.pinned = override.Services
			info, err := os.Stat(args[index])
			if err != nil || info.Mode().Perm() != 0o600 {
				return errors.New("private override required")
			}
		}
	}
	if r.fail == "refresh" {
		return errors.New("secret-bearing CLI failure")
	}
	return nil
}

func runtimeConsumerFixture(t *testing.T, runner *runtimeConsumerRunner) *productionComposeTarget {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	if runner.project == "" {
		runner.project = "owned-project"
	}
	if runner.service == "" {
		runner.service = "customer-api"
	}
	if runner.image == "" {
		runner.image = "sha256:" + strings.Repeat("c", 64)
	}
	return &productionComposeTarget{runner: runner, enabledServices: map[string]bool{"portal-api": true, "customer-api": true},
		config: productionComposeTargetConfig{DeployRoot: root, ComposeProject: "owned-project", DockerBinary: "docker",
			RuntimeConsumers: map[string][]string{"runtime/customer.env": {"customer-api", "customer-api", "portal-api"}},
			Profile: productionSubsystemProfile{Manifest: productionSubsystemManifest{
				Runtime: productionSubsystemRuntimeManifest{Files: []productionSubsystemRuntimeFileManifest{{Path: "runtime/customer.env"}}},
				Compose: productionSubsystemComposeManifest{RuntimeServices: []string{"portal-api"}},
			}}}}
}

func TestRuntimeConsumerRefreshPinsActualImageAndNoDependencies(t *testing.T) {
	runner := &runtimeConsumerRunner{running: true}
	target := runtimeConsumerFixture(t, runner)
	if err := target.refreshRuntimeConsumers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.starts) != 1 || len(runner.pinned) != 1 || runner.pinned["customer-api"]["image"] != runner.image {
		t.Fatalf("unexpected reload scope/pinned image: %#v", runner.starts)
	}
	args := runner.starts[0]
	for _, arg := range []string{"--no-deps", "--force-recreate", "--no-build", "--wait"} {
		if !containsString(args, arg) {
			t.Fatalf("missing safe option %s", arg)
		}
	}
	if args[len(args)-1] != "customer-api" || strings.Contains(strings.Join(args, " "), "portal-api") || !strings.Contains(strings.Join(args, " "), "--pull never") {
		t.Fatal("current app or unsafe image update included")
	}
	for _, path := range runner.paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("override not cleaned")
		}
	}
}

func TestRuntimeConsumerDoesNotStartStoppedOrDisabledSystem(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		runner := &runtimeConsumerRunner{running: disabled}
		target := runtimeConsumerFixture(t, runner)
		if disabled {
			delete(target.enabledServices, "customer-api")
		}
		if err := target.refreshRuntimeConsumers(context.Background()); err != nil || len(runner.starts) != 0 {
			t.Fatal("stopped/disabled subsystem was started", err)
		}
	}
}

func TestCRMRuntimeUpdateRefreshesOnlyExistingPortalCompensation(t *testing.T) {
	runner := &runtimeConsumerRunner{running: true, service: "portal-invite-compensation-worker"}
	target := runtimeConsumerFixture(t, runner)
	target.config.Profile.Manifest.Compose.RuntimeServices = []string{"customer-api"}
	target.config.RuntimeConsumers["runtime/customer.env"] = []string{"customer-api", "portal-invite-compensation-worker"}
	target.enabledServices["portal-invite-compensation-worker"] = true
	if err := target.refreshRuntimeConsumers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.pinned) != 1 || runner.pinned["portal-invite-compensation-worker"]["image"] != runner.image ||
		!reflect.DeepEqual(target.config.Profile.Manifest.Compose.RuntimeServices, []string{"customer-api"}) {
		t.Fatal("CRM refresh changed Portal image or application component ownership")
	}
}

func TestConditionalSharedRuntimeNotOnboardedSkipsConsumerRefresh(t *testing.T) {
	runner := &runtimeConsumerRunner{running: true}
	target := runtimeConsumerFixture(t, runner)
	target.config.Profile.Manifest.Runtime.Files[0].WhenService = "customer-api"
	target.config.Profile.Manifest.Runtime.Files[0].RequiredExistingKeys = []string{"OIDC_CLIENT_ID", "OIDC_TENANT_ID"}
	if err := target.refreshRuntimeConsumers(context.Background()); err != nil || len(runner.starts) != 0 {
		t.Fatal("dependency with incomplete onboarding was refreshed", err)
	}
}

func TestRuntimeConsumerFailsClosedOnOwnershipOrCLIError(t *testing.T) {
	for _, test := range []struct {
		name, project, service, image, oneoff, failure string
		count                                          int
	}{
		{name: "foreign project", project: "foreign"}, {name: "foreign service", service: "platform-api"},
		{name: "non immutable image", image: "latest"}, {name: "oneoff", oneoff: "True"},
		{name: "ambiguous", count: 2}, {name: "inventory", failure: "inventory"},
		{name: "inspect", failure: "inspect"}, {name: "refresh", failure: "refresh"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &runtimeConsumerRunner{running: true, project: test.project, service: test.service, image: test.image, oneoff: test.oneoff, fail: test.failure, count: test.count}
			target := runtimeConsumerFixture(t, runner)
			err := target.refreshRuntimeConsumers(context.Background())
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("unsafe success or secret failure disclosure", err)
			}
			if test.failure != "refresh" && len(runner.starts) > 0 {
				t.Fatal("unverified consumer was refreshed")
			}
		})
	}
}

func TestRuntimeConsumerRetryRefreshesExistingSameRuntime(t *testing.T) {
	runner := &runtimeConsumerRunner{running: true, fail: "refresh"}
	target := runtimeConsumerFixture(t, runner)
	if target.refreshRuntimeConsumers(context.Background()) == nil {
		t.Fatal("expected first failure")
	}
	runner.fail = ""
	if err := target.refreshRuntimeConsumers(context.Background()); err != nil || len(runner.starts) != 2 {
		t.Fatal("retry did not reconcile old process configuration", err)
	}
}

func TestRuntimeConsumerIndexDoesNotExpandLicenseRuntimeScope(t *testing.T) {
	portal := productionSubsystemProfile{Manifest: productionSubsystemManifest{
		Runtime: productionSubsystemRuntimeManifest{Files: []productionSubsystemRuntimeFileManifest{{Path: "runtime/customer.env", Consumers: []string{"portal-invite-compensation-worker"}}}},
		Compose: productionSubsystemComposeManifest{RuntimeServices: []string{"portal-api", "portal-invite-compensation-worker"}},
	}}
	crm := productionSubsystemProfile{Manifest: productionSubsystemManifest{Runtime: productionSubsystemRuntimeManifest{Files: []productionSubsystemRuntimeFileManifest{{Path: "runtime/customer.env", Consumers: []string{"customer-api"}}}}}}
	index := productionRuntimeConsumers([]productionSubsystemProfile{portal, crm})
	if !reflect.DeepEqual(index["runtime/customer.env"], []string{"customer-api", "portal-invite-compensation-worker"}) || !reflect.DeepEqual(portal.Manifest.Compose.RuntimeServices, []string{"portal-api", "portal-invite-compensation-worker"}) {
		t.Fatal("refresh consumers polluted license ownership")
	}
}

func TestRuntimeConsumerProfileRejectsCrossApplicationAndInfrastructure(t *testing.T) {
	for _, service := range []string{"platform-api", "customer-api", "sample-mysql", "sample-migrate", "unlisted-worker"} {
		t.Run(service, func(t *testing.T) {
			content := strings.Replace(minimalProductionProfileYAML, "- path: runtime/sample.env", "- path: runtime/sample.env\n      consumers: ["+service+"]", 1)
			var manifest productionSubsystemManifest
			if err := yaml.Unmarshal([]byte(content), &manifest); err != nil {
				t.Fatal(err)
			}
			if err := normalizeAndValidateProductionSubsystemManifest(&manifest); err == nil {
				t.Fatal("unowned/infrastructure runtime consumer accepted")
			}
		})
	}
}

func TestRuntimeConsumerProfileAcceptsOwnedRuntimeOnly(t *testing.T) {
	content := strings.Replace(minimalProductionProfileYAML, "- path: runtime/sample.env", "- path: runtime/sample.env\n      consumers: [sample-api]", 1)
	var manifest productionSubsystemManifest
	if err := yaml.Unmarshal([]byte(content), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := normalizeAndValidateProductionSubsystemManifest(&manifest); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedCRMPortalConsumerIndexPreservesCommercialCoverage(t *testing.T) {
	root := filepath.Join(platformModuleRoot(t), "deploy", "production")
	profiles, _, err := loadProductionSubsystemProfiles(root, filepath.Join(root, "subsystems.d"))
	if err != nil {
		t.Fatal(err)
	}
	index := productionRuntimeConsumers(profiles)
	if len(index["runtime/customer.env"]) != 9 {
		t.Fatalf("shared CRM runtime needs 8 CRM and 1 Portal consumer: %v", index["runtime/customer.env"])
	}
	for _, profile := range profiles {
		switch profile.Manifest.Application.Code {
		case "customer_portal":
			if len(profile.Manifest.Compose.RuntimeServices) != 2 {
				t.Fatal("portal commercial component coverage expanded")
			}
		case "customer_and_opportunity":
			if len(profile.Manifest.Compose.RuntimeServices) != 8 {
				t.Fatal("CRM commercial component coverage expanded")
			}
		}
	}
}
