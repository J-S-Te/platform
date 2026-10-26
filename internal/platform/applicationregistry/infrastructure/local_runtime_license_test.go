package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

type localLicenseRunner struct {
	calls [][]string
	image localImageMetadata
	fail  bool
}

func (r *localLicenseRunner) Run(_ context.Context, _ string, _ []string, _ string, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	return nil
}
func (r *localLicenseRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	if r.fail {
		return []byte("invalid"), nil
	}
	if len(args) > 0 && args[0] == "exec" {
		return []byte("1"), nil
	}
	if len(args) > 0 && args[0] == "ps" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "label=com.docker.compose.service=") {
				name := strings.TrimPrefix(arg, "label=com.docker.compose.service=")
				for i, id := range localLicenseServices["customer_and_opportunity"] {
					if id == name {
						return []byte(fmt.Sprintf("%064x", i+1)), nil
					}
				}
			}
		}
		return nil, nil
	}
	if len(args) > 1 && args[0] == "container" && args[1] == "inspect" {
		index, e := strconv.ParseInt(args[len(args)-1], 16, 64)
		if e != nil || index < 1 || index > 9 {
			return nil, fmt.Errorf("unexpected container")
		}
		return json.Marshal(map[string]any{"id": args[len(args)-1], "image": r.image.ID, "running": true, "started_at": time.Unix(1790000000, 0).UTC(), "project": "basic-platform-local", "service": localLicenseServices["customer_and_opportunity"][index-1], "oneoff": "False"})
	}
	return json.Marshal(r.image)
}
func localLicenseFixture(t *testing.T) (*LocalDockerSubsystemProvisioner, localRuntimeRelease, application.SubsystemProvisioningInput, *localLicenseRunner) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	platform := filepath.Join(root, "platform")
	for _, path := range []string{"platform/scripts", "platform/runtime-approvals", "platform/runtime", "platform/data/keys", "platform/docker", "contract_management", "customer_and_opportunity", "customer_portal", "project_management", "Settlement", "data_analysis"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"platform/compose.local.yaml", "platform/docker/.env.local", "platform/docker/.env.customer.local", "platform/docker/.env.portal.local", "contract_management/.env.local", "project_management/.env.local", "Settlement/.env.local", "data_analysis/.env.local", "customer_and_opportunity/go.mod", "customer_and_opportunity/Dockerfile"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	image := localImageMetadata{ID: "sha256:" + strings.Repeat("a", 64), Version: "local", Protocol: "1"}
	runner := &localLicenseRunner{image: image}
	p, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{Enabled: true, ProjectsRoot: root, GatewayScriptPath: filepath.Join(platform, "scripts", "portal-gateway.sh"), PlatformComposeProject: "basic-platform-local"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	source, err := localSourceDigest(filepath.Join(root, "customer_and_opportunity"))
	if err != nil {
		t.Fatal(err)
	}
	release := localRuntimeRelease{Migration: localLicenseMigration{Service: "customer-migrate", ImageDigest: image.ID, Version: image.Version, Protocol: image.Protocol}, Version: 1, Project: "basic-platform-local", Application: "customer_and_opportunity", Environment: "dev", Operator: "tester", Reviewed: true, ComposeSHA256: localDigest([]byte("fixture\n")), SourceDigest: source}
	for _, id := range localLicenseServices[release.Application] {
		release.Components = append(release.Components, application.RuntimeLicenseApproval{ServiceID: id, Protocol: 1, CoverageDigest: localDigest([]byte("dev-source-fingerprint\x00" + release.Application + "\x00" + source)), ImageDigest: image.ID})
		release.Services = append(release.Services, licenseevidence.Service{Application: release.Application, Environment: "dev", Name: id, ImageDigest: image.ID, Version: "local", Protocol: "1", Replicas: 1})
	}
	writeLocalReleaseFixture(t, p, release)
	digest, err := application.RuntimeLicenseReleaseDigest(localLifecycle(release))
	if err != nil {
		t.Fatal(err)
	}
	input := application.SubsystemProvisioningInput{ApplicationCode: release.Application, Environment: "dev", RuntimeLicenseSettings: application.RuntimeLicenseSettings{InstanceID: "dev-installation", Environment: "production", PlatformBaseURL: "http://platform-api:8080", PlatformPublicKeyPath: "/app/data/keys/jwt-ed25519-public.pem", StateDirectory: "/var/lib/commercial-license", AllowHTTP: true}}
	for _, c := range release.Components {
		input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{Approval: c, ClientID: "license-" + c.ServiceID, ClientSecret: "secret-" + c.ServiceID, ReleaseDigest: digest})
	}
	return p, release, input, runner
}
func writeLocalReleaseFixture(t *testing.T, p *LocalDockerSubsystemProvisioner, r localRuntimeRelease) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.localPlatformRoot(), "runtime-approvals", "local-"+r.Application+"-dev.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestLocalRuntimeLicenseRequiresReviewedCompleteImmutableRelease(t *testing.T) {
	for _, mutation := range []string{"valid", "unreviewed", "compose", "source", "missing", "duplicate", "image", "crossenv", "protocol", "permissions", "symlink", "unknown"} {
		t.Run(mutation, func(t *testing.T) {
			p, r, _, runner := localLicenseFixture(t)
			app, env := r.Application, "dev"
			switch mutation {
			case "unreviewed":
				r.Reviewed = false
			case "compose":
				r.ComposeSHA256 = "sha256:" + strings.Repeat("b", 64)
			case "source":
				r.SourceDigest = "sha256:" + strings.Repeat("b", 64)
			case "missing":
				r.Components = r.Components[1:]
			case "duplicate":
				r.Services[1] = r.Services[0]
			case "image":
				runner.image.ID = "sha256:" + strings.Repeat("b", 64)
			case "crossenv":
				env = "prod"
			case "protocol":
				runner.image.Protocol = "0"
			case "unknown":
				app = "arbitrary"
			}
			writeLocalReleaseFixture(t, p, r)
			path := filepath.Join(p.localPlatformRoot(), "runtime-approvals", "local-"+r.Application+"-dev.json")
			if mutation == "permissions" {
				os.Chmod(path, 0644)
			}
			if mutation == "symlink" {
				os.Rename(path, path+".real")
				os.Symlink(path+".real", path)
			}
			got, err := p.ApprovedRuntimeLicenseComponents(context.Background(), app, env)
			if mutation == "valid" {
				if err != nil || len(got) != 9 {
					t.Fatalf("valid: %v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("accepted unsafe local release")
			}
		})
	}
}
func TestLocalRuntimeLicenseCredentialDeliveryAndPinnedCompleteStartup(t *testing.T) {
	p, r, input, runner := localLicenseFixture(t)
	if err := p.deliverLocalRuntimeLicense(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	for _, c := range input.RuntimeLicenseCredentials {
		path := filepath.Join(p.localPlatformRoot(), "runtime", "license-"+c.Approval.ServiceID+".env")
		values, err := readEnvironmentValues(path)
		if err != nil {
			t.Fatal(err)
		}
		if values["COMMERCIAL_LICENSE_CLIENT_SECRET"] != c.ClientSecret || values["COMMERCIAL_LICENSE_STATE_PATH"] != runtimeLicenseStatePath(input.RuntimeLicenseSettings, c) {
			t.Fatal("identity or state not bound")
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal("secret permissions")
		}
		b, _ := os.ReadFile(path)
		for _, other := range input.RuntimeLicenseCredentials {
			if other.ClientID != c.ClientID && strings.Contains(string(b), "COMMERCIAL_LICENSE_CLIENT_SECRET="+other.ClientSecret+"\n") {
				t.Fatal("cross-component secret leak")
			}
		}
	}
	path, err := p.localRuntimeOverride()
	if err != nil || path == "" {
		t.Fatal(err)
	}
	var override localPinnedCompose
	if readLocalPrivateJSON(path, &override) != nil || len(override.Services) != len(r.Components)+1 {
		t.Fatal("incomplete pins")
	}
	for _, svc := range override.Services {
		if svc.PullPolicy != "never" {
			t.Fatal("pinned one-off commands may pull images")
		}
	}
	if err = p.deployLicensedLocalStack(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	started := map[string]bool{}
	for _, args := range runner.calls {
		if containsString(args, "run") && (containsString(args, "--no-build") || containsString(args, "--pull")) {
			t.Fatal("unsupported compose run flags")
		}
		for _, arg := range args {
			if arg == "build" || arg == "--build" {
				t.Fatal("rebuilt approved image")
			}
		}
		if !containsString(args, "-f") || !containsString(args, path) {
			t.Fatal("lost pinned override")
		}
		if containsString(args, "up") {
			for _, id := range localLicenseServices[r.Application] {
				if containsString(args, id) {
					started[id] = true
				}
			}
		}
	}
	if len(started) != 9 {
		t.Fatalf("missing consumers: %v", started)
	}
	before, _ := os.ReadFile(path)
	input.RuntimeLicenseCredentials[0].ClientSecret = "rotated-secret"
	if err = p.deliverLocalRuntimeLicense(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("secret rotation changed image/state binding")
	}
}

// 已批准发行缺少组件凭据时，错误必须能被上层归类为“授权实例未初始化”，
// 并给出可执行的中文指引，而不是让用户看到与实际原因无关的凭据报错。
func TestLocalRuntimeLicenseMissingCredentialsNamesTheLicenseCause(t *testing.T) {
	p, _, input, _ := localLicenseFixture(t)
	input.RuntimeLicenseCredentials = nil
	err := p.deliverLocalRuntimeLicense(context.Background(), input)
	if err == nil {
		t.Fatal("expected failure when an approved release has no component credentials")
	}
	if got := application.RuntimeLicenseApprovalErrorCode(err); got != "LICENSE_NOT_INITIALIZED" {
		t.Fatalf("code = %q, want LICENSE_NOT_INITIALIZED (err=%v)", got, err)
	}
	if !strings.Contains(err.Error(), "授权") {
		t.Fatalf("error must be actionable, got %v", err)
	}
}

func TestLocalRuntimeLicenseRejectsPartialOrForeignCredentialsBeforeWriting(t *testing.T) {
	for _, mutation := range []string{"partial", "duplicate", "digest", "secret", "release", "url", "no-credentials"} {
		t.Run(mutation, func(t *testing.T) {
			p, _, input, _ := localLicenseFixture(t)
			switch mutation {
			case "partial":
				input.RuntimeLicenseCredentials = input.RuntimeLicenseCredentials[1:]
			case "duplicate":
				input.RuntimeLicenseCredentials[1] = input.RuntimeLicenseCredentials[0]
			case "digest":
				input.RuntimeLicenseCredentials[0].Approval.ImageDigest = "sha256:" + strings.Repeat("b", 64)
			case "secret":
				input.RuntimeLicenseCredentials[0].ClientSecret = "x\nINJECT=true"
			case "release":
				input.RuntimeLicenseCredentials[0].ReleaseDigest = "bad"
			case "url":
				input.RuntimeLicenseSettings.PlatformBaseURL = "http://localhost:8081"
			case "no-credentials":
				input.RuntimeLicenseCredentials = nil
			}
			if err := p.deliverLocalRuntimeLicense(context.Background(), input); err == nil {
				t.Fatal("unsafe delivery accepted")
			}
			entries, _ := os.ReadDir(filepath.Join(p.localPlatformRoot(), "runtime"))
			if len(entries) != 0 {
				t.Fatal("partial credential files written")
			}
		})
	}
}
