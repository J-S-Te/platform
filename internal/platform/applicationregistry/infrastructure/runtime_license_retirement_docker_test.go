package infrastructure

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// Opt-in execution boundary test: actual Docker stop through the production
// runner/retirement method. Alpine fixtures are not business installation proof.
func TestRuntimeLicenseRetirementDockerIsolation(t *testing.T) {
	if os.Getenv("LICENSE_RETIREMENT_DOCKER_E2E") != "1" {
		t.Skip("explicit isolated Docker retirement opt-in required")
	}
	evidencePath := os.Getenv("LICENSE_RETIREMENT_EVIDENCE_PATH")
	if !filepath.IsAbs(evidencePath) {
		t.Fatal("isolated absolute evidence directory required")
	}
	if info, err := os.Stat(evidencePath); err != nil || !info.IsDir() {
		t.Fatal("evidence directory must already exist")
	}
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(seed[:])
	project := "codex-license-retire-" + suffix
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runner := execSubsystemCommandRunner{}
	if _, err := runner.RunOutput(ctx, "", nil, "docker", "image", "inspect", "alpine:3.21"); err != nil {
		t.Fatal("local alpine:3.21 required; test never pulls a replacement image")
	}
	owned := map[string]string{}
	cleanup := map[string]bool{}
	writeEvidence := func(name string, value any) {
		t.Helper()
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err = os.WriteFile(filepath.Join(evidencePath, name), append(raw, '\n'), 0600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		for name, id := range owned {
			if err := runner.Run(cleanupCtx, "", nil, "docker", "container", "rm", "--force", id); err != nil {
				t.Errorf("owned Docker fixture cleanup failed: %s", name)
			} else {
				cleanup[name] = true
			}
		}
		writeEvidence("retirement-cleanup.json", map[string]any{"project": project, "owned_container_ids": owned, "removed": cleanup, "volumes_created": false})
	})
	start := func(name, projectName string) string {
		t.Helper()
		cidPath := filepath.Join(t.TempDir(), "container.id")
		_, runErr := runner.RunOutput(ctx, "", nil, "docker", "run", "--detach", "--cidfile", cidPath, "--name", name, "--network", "none", "--label", "codex.qa.owner="+project, "--label", "com.docker.compose.project="+projectName, "--label", "com.docker.compose.service=contract-old-worker", "alpine:3.21", "sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done")
		raw, readErr := os.ReadFile(cidPath)
		id := strings.TrimSpace(string(raw))
		if readErr == nil && runtimeRetirementContainerID.MatchString(id) {
			owned[name] = id
		}
		if runErr != nil || readErr != nil {
			t.Fatal("cannot start isolated Docker fixture")
		}
		if !runtimeRetirementContainerID.MatchString(id) {
			t.Fatal("Docker did not return an exact immutable container ID")
		}
		return id
	}
	retiredID := start(project+"-target", project)
	sentinelID := start(project+"-sentinel", project+"-other")
	target, input := runtimeLicenseFixture(t)
	target.runner = runner
	target.config.DockerBinary = "docker"
	target.config.ComposeProject = project
	path := filepath.Join(target.config.DeployRoot, "runtime-license-contract_management-prod.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var release runtimeLicenseRelease
	if err = json.Unmarshal(raw, &release); err != nil {
		t.Fatal(err)
	}
	release.RetireServiceIDs = []string{"contract-old-worker"}
	digest, err := application.RuntimeLicenseReleaseDigest(application.RuntimeLicenseReleaseApproval{
		Components: release.Components, RequiredServiceIDs: []string{"contract-api", "contract-worker"},
		RetireServiceIDs: release.RetireServiceIDs, ReleaseGeneration: release.ReleaseGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range input.RuntimeLicenseCredentials {
		input.RuntimeLicenseCredentials[i].ReleaseDigest = digest
	}
	raw, err = json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	type observed struct {
		ID    string `json:"Id"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Mounts []json.RawMessage `json:"Mounts"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	inspect := func(id string) observed {
		t.Helper()
		raw, err := runner.RunOutput(ctx, "", nil, "docker", "container", "inspect", "--format", "{{json .}}", id)
		if err != nil {
			t.Fatal("isolated container inspection failed")
		}
		var v observed
		if err = json.Unmarshal(raw, &v); err != nil || v.ID != id || len(v.Mounts) != 0 || v.Config.Labels["codex.qa.owner"] != project {
			t.Fatal("fixture scope or no-volume guard failed")
		}
		return v
	}
	beforeTarget, beforeSentinel := inspect(retiredID), inspect(sentinelID)
	if !beforeTarget.State.Running || !beforeSentinel.State.Running {
		t.Fatal("fixtures must start running")
	}
	bad := input
	bad.RuntimeLicenseCredentials = append([]application.RuntimeLicenseCredential(nil), input.RuntimeLicenseCredentials...)
	bad.RuntimeLicenseCredentials[0].ReleaseDigest = "sha256:" + strings.Repeat("f", 64)
	if err = target.retireRuntimeLicenseContainers(ctx, bad); err == nil {
		t.Fatal("unbound release approval allowed a Docker process action")
	}
	if !inspect(retiredID).State.Running || !inspect(sentinelID).State.Running {
		t.Fatal("rejected release binding stopped a fixture")
	}
	if err = target.retireRuntimeLicenseContainers(ctx, input); err != nil {
		t.Fatal(err)
	}
	afterTarget, afterSentinel := inspect(retiredID), inspect(sentinelID)
	if afterTarget.State.Running || !afterSentinel.State.Running {
		t.Fatal("retirement stopped wrong Docker project or failed to stop approved old component")
	}
	// Repetition may stop an already-stopped exact old container, but must never
	// grow into another project's identically named service.
	if err = target.retireRuntimeLicenseContainers(ctx, input); err != nil {
		t.Fatal("retirement retry failed")
	}
	if !inspect(sentinelID).State.Running {
		t.Fatal("retry stopped unrelated project's sentinel")
	}
	writeEvidence("retirement-observations.json", map[string]any{"scope": "actual Docker retirement execution only, not full licensed business installation", "target_before": beforeTarget, "target_after": afterTarget, "sentinel_before": beforeSentinel, "sentinel_after": afterSentinel, "production_method": "retireRuntimeLicenseContainers", "production_runner": "execSubsystemCommandRunner", "volume_count": 0})
	t.Log("real Docker retired only exact approved project/service target; same-service other-project sentinel remained running; no volumes or business containers touched")
}
