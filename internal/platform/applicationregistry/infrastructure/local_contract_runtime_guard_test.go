package infrastructure

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type retainedRuntimeGuardRunner struct {
	inspectError error
	listError    error
	listOutput   string
}

func retainedContractRuntimeFixture() string {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	return "CONTRACT_MYSQL_PASSWORD=test-database-password\nCONTRACT_MYSQL_ROOT_PASSWORD=test-root-password\nOIDC_SESSION_ENCRYPTION_KEY_BASE64=" + key + "\nSIGNING_PHONE_ENCRYPTION_KEY_BASE64=" + key + "\n"
}

func assertRetainedContractVolumeInspection(t *testing.T, call recordingSubsystemRunnerCall) {
	t.Helper()
	if !reflect.DeepEqual(call.arguments, []string{"volume", "inspect", "basic-platform-local-contract-mysql-data"}) {
		t.Fatalf("missing retained contract volume inspection: %v", call.arguments)
	}
}

type firstInstallContractRunner struct{ recordingSubsystemRunner }

func (r *firstInstallContractRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	if err := r.recordingSubsystemRunner.Run(ctx, dir, env, name, args...); err != nil {
		return err
	}
	if len(args) > 1 && args[0] == "volume" && args[1] == "inspect" {
		return errors.New("fixture: volume does not exist")
	}
	return nil
}

func (r *firstInstallContractRunner) RunOutput(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if err := r.recordingSubsystemRunner.Run(ctx, dir, env, name, args...); err != nil {
		return nil, err
	}
	if len(args) < 2 || args[0] != "volume" || args[1] != "ls" {
		return nil, errors.New("fixture: unexpected output command")
	}
	return []byte{}, nil
}

func TestRetainedContractMissingRuntimeStopsBeforeWritesAndRebuild(t *testing.T) {
	for _, mode := range []string{"provision", "update"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "contract_management")
			if err := os.MkdirAll(project, 0700); err != nil {
				t.Fatal(err)
			}
			templatePath := filepath.Join(project, ".env.example")
			original := []byte("CONTRACT_MYSQL_PASSWORD=REPLACE_WITH_PASSWORD\n")
			if err := os.WriteFile(templatePath, original, 0600); err != nil {
				t.Fatal(err)
			}
			runner := &recordingSubsystemRunner{}
			p, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{Enabled: true, ProjectsRoot: root, PlatformComposeProject: "basic-platform-local", Timeout: time.Second}, runner)
			if err != nil {
				t.Fatal(err)
			}
			input := application.SubsystemProvisioningInput{ApplicationCode: "contract_management", Environment: "dev"}
			if mode == "provision" {
				err = p.Provision(context.Background(), input)
			} else {
				err = p.Update(context.Background(), input)
			}
			if err == nil {
				t.Fatal("retained database with missing runtime was deployed")
			}
			if len(runner.calls) != 1 {
				t.Fatalf("guard allowed subsequent commands: %v", runner.calls)
			}
			assertRetainedContractVolumeInspection(t, runner.calls[0])
			if _, err := os.Stat(filepath.Join(project, ".env.local")); !os.IsNotExist(err) {
				t.Fatal("missing runtime created")
			}
			current, err := os.ReadFile(templatePath)
			if err != nil || string(current) != string(original) {
				t.Fatal("source template was modified")
			}
		})
	}
}

func (r *retainedRuntimeGuardRunner) Run(_ context.Context, _ string, _ []string, _ string, arguments ...string) error {
	return r.inspectError
}

func (r *retainedRuntimeGuardRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, error) {
	return []byte(r.listOutput), r.listError
}

func TestValidateRetainedContractRuntime(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	valid := "CONTRACT_MYSQL_PASSWORD=test-database-password\nCONTRACT_MYSQL_ROOT_PASSWORD=test-root-password\nOIDC_SESSION_ENCRYPTION_KEY_BASE64=" + key + "\nSIGNING_PHONE_ENCRYPTION_KEY_BASE64=" + key + "\n"
	for _, tc := range []struct {
		name      string
		content   string
		missing   bool
		runner    retainedRuntimeGuardRunner
		wantError bool
	}{
		{name: "first installation", missing: true, runner: retainedRuntimeGuardRunner{inspectError: errors.New("not found")}},
		{name: "existing volume missing file", missing: true, wantError: true},
		{name: "existing valid configuration", content: valid},
		{name: "placeholder password", content: strings.Replace(valid, "test-database-password", "REPLACE_WITH_PASSWORD", 1), wantError: true},
		{name: "missing root password", content: strings.Replace(valid, "test-root-password", "", 1), wantError: true},
		{name: "invalid session key", content: strings.Replace(valid, "OIDC_SESSION_ENCRYPTION_KEY_BASE64="+key, "OIDC_SESSION_ENCRYPTION_KEY_BASE64=invalid", 1), wantError: true},
		{name: "missing signing key", content: strings.Replace(valid, "SIGNING_PHONE_ENCRYPTION_KEY_BASE64="+key, "SIGNING_PHONE_ENCRYPTION_KEY_BASE64=", 1), wantError: true},
		{name: "daemon failure", missing: true, runner: retainedRuntimeGuardRunner{inspectError: errors.New("daemon failed"), listError: errors.New("daemon failed")}, wantError: true},
		{name: "inspect fails despite retained volume", missing: true, runner: retainedRuntimeGuardRunner{inspectError: errors.New("inspect error"), listOutput: "basic-platform-local-contract-mysql-data\n"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtime.env")
			if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p := &LocalDockerSubsystemProvisioner{config: LocalDockerSubsystemProvisionerConfig{PlatformComposeProject: "basic-platform-local", DockerBinary: "docker"}, runner: &tc.runner}
			err := p.validateRetainedContractRuntime(context.Background(), path)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want error=%v", err, tc.wantError)
			}
			if err != nil && (strings.Contains(err.Error(), "test-database-password") || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), path)) {
				t.Fatal("error exposes runtime values")
			}
			if !tc.missing {
				current, readErr := os.ReadFile(path)
				if readErr != nil || string(current) != tc.content {
					t.Fatal("guard changed runtime configuration")
				}
			} else if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatal("guard created runtime file")
			}
		})
	}
}

type retainedRuntimeGuardNoOutputRunner struct{}

func (retainedRuntimeGuardNoOutputRunner) Run(context.Context, string, []string, string, ...string) error {
	return errors.New("inspect unavailable")
}

func TestValidateRetainedContractRuntimeCannotInferAbsence(t *testing.T) {
	p := &LocalDockerSubsystemProvisioner{config: LocalDockerSubsystemProvisionerConfig{PlatformComposeProject: "basic-platform-local"}, runner: retainedRuntimeGuardNoOutputRunner{}}
	if err := p.validateRetainedContractRuntime(context.Background(), filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("inspect error inferred first installation without a successful inventory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.validateRetainedContractRuntime(ctx, "missing.env"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context ignored: %v", err)
	}
}
