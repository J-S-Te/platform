package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type retirementRunner struct {
	listed, project, service, inspectedID string
	calls                                 [][]string
	stopFailure                           bool
}

func (r *retirementRunner) Run(_ context.Context, _ string, _ []string, _ string, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	if r.stopFailure {
		return errors.New("isolated stop failure")
	}
	return nil
}
func (r *retirementRunner) RunOutput(_ context.Context, _ string, _ []string, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 1 && args[1] == "ls" {
		return []byte(r.listed), nil
	}
	return json.Marshal(map[string]any{"Id": r.inspectedID, "Config": map[string]any{"Labels": map[string]string{"com.docker.compose.project": r.project, "com.docker.compose.service": r.service}}})
}

func TestRuntimeRetirementStopsOnlyInspectedExactIDs(t *testing.T) {
	for _, tt := range []struct {
		name, retired, project, service, id string
		empty, stopFail, fail               bool
	}{
		{"removed service", "contract-old-worker", "isolated-project", "contract-old-worker", strings.Repeat("a", 64), false, false, false},
		{"absent is idempotent", "contract-old-worker", "isolated-project", "contract-old-worker", "", true, false, false},
		{"other project", "contract-old-worker", "another-project", "contract-old-worker", strings.Repeat("a", 64), false, false, true},
		{"other service", "contract-old-worker", "isolated-project", "customer-api", strings.Repeat("a", 64), false, false, true},
		{"short ID", "contract-old-worker", "isolated-project", "contract-old-worker", "aaaa", false, false, true},
		{"stop failure", "contract-old-worker", "isolated-project", "contract-old-worker", strings.Repeat("a", 64), false, true, true},
		{"selected service", "contract-api", "isolated-project", "contract-api", strings.Repeat("a", 64), false, false, true},
		{"core infrastructure", "platform-api", "isolated-project", "platform-api", strings.Repeat("a", 64), false, false, true},
		{"other subsystem DB", "customer-mysql", "isolated-project", "customer-mysql", strings.Repeat("a", 64), false, false, true},
		{"other subsystem business", "customer-api", "isolated-project", "customer-api", strings.Repeat("a", 64), false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target, input := runtimeLicenseFixture(t)
			path := filepath.Join(target.config.DeployRoot, "runtime-license-contract_management-prod.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var release runtimeLicenseRelease
			if err := json.Unmarshal(raw, &release); err != nil {
				t.Fatal(err)
			}
			release.RetireServiceIDs = []string{tt.retired}
			raw, err = json.Marshal(release)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			digest, err := target.runtimeLicenseReleaseDigest(release)
			if err == nil {
				for i := range input.RuntimeLicenseCredentials {
					input.RuntimeLicenseCredentials[i].ReleaseDigest = digest
				}
			}
			runner := &retirementRunner{listed: tt.id, project: tt.project, service: tt.service, inspectedID: tt.id, stopFailure: tt.stopFail}
			target.runner = runner
			target.config.ComposeProject = "isolated-project"
			target.config.DockerBinary = "docker"
			err = target.retireRuntimeLicenseContainers(context.Background(), input)
			if (err != nil) != tt.fail {
				t.Fatalf("error=%v want failure=%v", err, tt.fail)
			}
			stops := 0
			for _, call := range runner.calls {
				if len(call) > 1 && call[1] == "ls" && (!containsString(call, "--no-trunc") || !containsString(call, "label=com.docker.compose.project=isolated-project") || !containsString(call, "label=com.docker.compose.service="+tt.retired)) {
					t.Fatalf("unscoped enumeration: %v", call)
				}
				if len(call) > 1 && call[1] == "stop" {
					stops++
					if len(call) != 5 || call[4] != strings.Repeat("a", 64) {
						t.Fatalf("unsafe stop %v", call)
					}
				}
				if containsString(call, "rm") || containsString(call, "down") {
					t.Fatalf("destructive command %v", call)
				}
			}
			wantStops := 0
			if !tt.fail && !tt.empty || tt.stopFail {
				wantStops = 1
			}
			if stops != wantStops {
				t.Fatalf("stops=%d want=%d", stops, wantStops)
			}
		})
	}
}
