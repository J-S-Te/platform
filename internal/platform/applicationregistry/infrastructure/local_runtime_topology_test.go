package infrastructure

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"gopkg.in/yaml.v3"
)

func localTopologyWorkspace(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate topology test")
	}
	root := filepath.Dir(file)
	for i := 0; i < 5; i++ {
		root = filepath.Dir(root)
	}
	return root
}

type localTopologyManifest struct {
	Version      int `json:"version"`
	Applications map[string]struct {
		Directory string            `json:"directory"`
		Services  map[string]string `json:"services"`
		Migration struct {
			Service string `json:"service"`
			Image   string `json:"image"`
		} `json:"migration"`
	} `json:"applications"`
}

func readLocalTopologyManifest(t *testing.T) localTopologyManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(localTopologyWorkspace(t), "platform/config/local-license-components.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest localTopologyManifest
	if err = json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestLocalLicenseTopologyManifestAndCompiledConsumerSets(t *testing.T) {
	manifest := readLocalTopologyManifest(t)
	if manifest.Version != 1 || len(manifest.Applications) != 6 || len(manifest.Applications) != len(localLicenseServices) {
		t.Fatal("expected the complete six-application reviewed dev topology")
	}
	total := 0
	seen := map[string]bool{}
	for app, target := range manifest.Applications {
		if target.Migration.Service != localLicenseMigrations[app] || target.Migration.Image == "" {
			t.Fatalf("%s missing or incorrect fixed migration image", app)
		}
		actual := []string{}
		for service := range target.Services {
			if seen[service] {
				t.Fatalf("consumer %s appears in multiple applications", service)
			}
			seen[service] = true
			actual = append(actual, service)
		}
		expected := append([]string(nil), localLicenseServices[app]...)
		sort.Strings(actual)
		sort.Strings(expected)
		if !reflect.DeepEqual(expected, actual) {
			t.Fatalf("%s manifest differs from Agent consumers: %v versus %v", app, actual, expected)
		}
		total += len(actual)
	}
	if total != 21 || !seen["contract-transfer-worker"] || !seen["portal-report-worker"] {
		t.Fatalf("expected 21 real local consumers, including extra delivery/report workers; got %d", total)
	}
}

type localTopologyBuild struct {
	Context string            `yaml:"context"`
	Target  string            `yaml:"target"`
	Args    map[string]string `yaml:"args"`
}
type localTopologyService struct {
	Image string             `yaml:"image"`
	Build localTopologyBuild `yaml:"build"`
}

// 检查选定构建阶段及其父阶段，避免接受仅存在于
// 多阶段 Dockerfile 无关构建器或目标阶段中的 LABEL。
func localTopologyStageHasLicenseLabels(t *testing.T, path, target string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type stage struct {
		name, parent string
		version      bool
		protocol     bool
	}
	stages := []*stage{}
	byName := map[string]*stage{}
	var current *stage
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		words := strings.Fields(line)
		if len(words) == 0 || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.EqualFold(words[0], "FROM") {
			base := 1
			if len(words) > 1 && strings.HasPrefix(words[1], "--platform=") {
				base++
			}
			if len(words) <= base {
				t.Fatal("invalid Dockerfile stage")
			}
			current = &stage{parent: strings.ToLower(words[base])}
			if len(words) > base+2 && strings.EqualFold(words[base+1], "AS") {
				current.name = strings.ToLower(words[base+2])
				byName[current.name] = current
			}
			stages = append(stages, current)
		}
		if current != nil && strings.EqualFold(words[0], "LABEL") {
			current.version = current.version || strings.Contains(line, "org.opencontainers.image.version=${APP_VERSION}")
			current.protocol = current.protocol || strings.Contains(line, `com.basic-platform.license.protocol="1"`)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(stages) == 0 {
		return false
	}
	current = stages[len(stages)-1]
	if target != "" {
		current = byName[strings.ToLower(target)]
	}
	version, protocol := false, false
	for depth := 0; current != nil && depth <= len(stages); depth++ {
		version = version || current.version
		protocol = protocol || current.protocol
		current = byName[current.parent]
	}
	return version && protocol
}

func TestLocalLicenseTopologyComposeImagesSelectLicensedRuntimeStages(t *testing.T) {
	workspace := localTopologyWorkspace(t)
	platform := filepath.Join(workspace, "platform")
	raw, err := os.ReadFile(filepath.Join(platform, "compose.local.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]localTopologyService `yaml:"services"`
	}
	if err = yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	manifest := readLocalTopologyManifest(t)
	runtimeTargets := map[string]string{
		"contract-management/backend:local":               "",
		"customer-opportunity/backend:local":              "crm-runtime",
		"customer-opportunity/presale-worker:local":       "presale-worker-runtime",
		"customer-opportunity/presale-alert-worker:local": "presale-alert-worker-runtime",
		"customer-portal/backend:local":                   "portal-runtime",
		"project-management/backend:local":                "",
		"settlement/backend:local":                        "",
		"data-analysis/dashboard-api:local":               "dashboard-api",
		"data-analysis/aggregation-worker:local":          "aggregation-worker",
		"data-analysis/alert-worker:local":                "alert-worker",
		"data-analysis/local-migrate:local":               "local-migrate",
	}
	for app, target := range manifest.Applications {
		images := map[string]string{}
		for name, image := range target.Services {
			images[name] = image
		}
		images[target.Migration.Service] = target.Migration.Image
		for name, image := range images {
			t.Run(app+"/"+name, func(t *testing.T) {
				service, ok := compose.Services[name]
				if !ok || service.Image != image {
					t.Fatalf("missing consumer or unexpected image: %#v", service)
				}
				// 大多数 worker 刻意复用对应 API 已构建的镜像。
				builds := []localTopologyBuild{}
				for _, candidate := range compose.Services {
					if candidate.Image == image && candidate.Build.Context != "" {
						builds = append(builds, candidate.Build)
					}
				}
				if len(builds) == 0 {
					t.Fatal("no reviewed local build produces this image")
				}
				for _, build := range builds {
					expectedTarget, known := runtimeTargets[image]
					if !known || build.Target != expectedTarget {
						t.Fatalf("image %s uses unexpected Docker target %q", image, build.Target)
					}
					if build.Args["APP_VERSION"] != "${APP_VERSION:-local}" {
						t.Fatal("local build does not provide a nonempty version label")
					}
					dockerfile := filepath.Join(platform, build.Context, "Dockerfile")
					if _, err := os.Stat(dockerfile); os.IsNotExist(err) && !strings.HasPrefix(filepath.Clean(dockerfile), platform+string(os.PathSeparator)) {
						t.Log("sibling repository Dockerfile unavailable in isolated checkout")
						continue
					} else if err != nil {
						t.Fatal(err)
					}
					if !localTopologyStageHasLicenseLabels(t, dockerfile, build.Target) {
						t.Fatalf("build target %q does not inherit protocol/version runtime labels", build.Target)
					}
				}
			})
		}
	}
}

func TestLocalLicenseTopologySourceDigestRejectsSymlinkParents(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(root, "app")
	actual := filepath.Join(root, "actual", "license-core")
	if err := os.MkdirAll(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app, 0700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{filepath.Join(app, "go.mod"): "module app", filepath.Join(app, "Dockerfile"): "FROM scratch", filepath.Join(actual, "license.go"): "package license"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "actual"), filepath.Join(app, "third_party")); err != nil {
		t.Fatal(err)
	}
	if _, err := localSourceDigest(app); err == nil {
		t.Fatal("intermediate source-directory symlink was accepted")
	}
}

func TestLocalLicenseTopologyPrivateJSONRejectsDuplicateKeys(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"top-level": `{"services":{},"services":{},"volumes":{}}`,
		"nested":    `{"services":{"customer-api":{"image":"a","image":"b","env_file":[],"volumes":[]}},"volumes":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			var value localPinnedCompose
			if err := readLocalPrivateJSON(path, &value); err == nil {
				t.Fatal("duplicate JSON key was accepted")
			}
		})
	}
}

func TestLocalLicenseTopologyLifecycleSeparatesApplicationsAndRejectsProduction(t *testing.T) {
	total := 0
	for app, ids := range localLicenseServices {
		t.Run(app, func(t *testing.T) {
			p, release, _, runner := localLicenseFixture(t)
			directory := app
			if app == "customer_portal" {
				directory = "customer_and_opportunity"
			} else if app == "settlement" {
				directory = "Settlement"
			}
			root := filepath.Join(filepath.Dir(p.localPlatformRoot()), directory)
			for _, name := range []string{"go.mod", "Dockerfile"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("fixture\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			source, err := localSourceDigest(root)
			if err != nil {
				t.Fatal(err)
			}
			release.Application, release.SourceDigest = app, source
			release.Migration = localLicenseMigration{Service: localLicenseMigrations[app], ImageDigest: runner.image.ID, Version: runner.image.Version, Protocol: "1"}
			release.Components, release.Services = nil, nil
			for _, id := range ids {
				release.Components = append(release.Components, application.RuntimeLicenseApproval{ServiceID: id, Protocol: 1, ImageDigest: runner.image.ID, CoverageDigest: localDigest([]byte("dev-source-fingerprint\x00" + app + "\x00" + source))})
				release.Services = append(release.Services, licenseevidence.Service{Application: app, Environment: "dev", Name: id, ImageDigest: runner.image.ID, Version: runner.image.Version, Protocol: "1", Replicas: 1})
			}
			writeLocalReleaseFixture(t, p, release)
			approval, err := p.ApprovedRuntimeLicenseLifecycle(context.Background(), app, "dev")
			if err != nil || len(approval.Components) != len(ids) || len(approval.RequiredServiceIDs) != len(ids) {
				t.Fatalf("incomplete %s lifecycle: %v", app, err)
			}
			if _, err := application.RuntimeLicenseReleaseDigest(approval); err != nil {
				t.Fatal(err)
			}
			for _, env := range []string{"prod", "production", "staging", "test"} {
				if _, err := p.ApprovedRuntimeLicenseLifecycle(context.Background(), app, env); err == nil {
					t.Fatalf("dev approval accepted for %s", env)
				}
			}
			total += len(approval.Components)
		})
	}
	if total != 21 {
		t.Fatalf("expected 21 consumers across separate app lifecycles, got %d", total)
	}
}

func TestLocalLicenseTopologyRejectsUnknownApprovalAndOverrideServices(t *testing.T) {
	t.Run("approval", func(t *testing.T) {
		p, release, _, _ := localLicenseFixture(t)
		release.Components[0].ServiceID = "unknown-worker"
		release.Services[0].Name = "unknown-worker"
		writeLocalReleaseFixture(t, p, release)
		if _, err := p.ApprovedRuntimeLicenseComponents(context.Background(), release.Application, "dev"); err == nil {
			t.Fatal("unknown approval service was accepted")
		}
	})
	t.Run("override", func(t *testing.T) {
		p, _, input, _ := localLicenseFixture(t)
		if err := p.deliverLocalRuntimeLicense(context.Background(), input); err != nil {
			t.Fatal(err)
		}
		path, err := p.localRuntimeOverride()
		if err != nil {
			t.Fatal(err)
		}
		var override localPinnedCompose
		if err := readLocalPrivateJSON(path, &override); err != nil {
			t.Fatal(err)
		}
		override.Services["unknown-worker"] = localPinnedService{Image: "sha256:" + strings.Repeat("a", 64)}
		raw, err := json.Marshal(override)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.localRuntimeOverride(); err == nil {
			t.Fatal("unknown override service was accepted")
		}
	})
}
