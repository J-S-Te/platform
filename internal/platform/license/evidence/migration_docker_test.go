package evidence

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This opt-in acceptance test exercises actual Docker projections and isolated
// containers. It is evidence collection coverage, not a full business install.
func TestMigrationDockerInstallationEvidence(t *testing.T) {
	if os.Getenv("LICENSE_MIGRATION_DOCKER_TEST") != "1" {
		t.Skip("set LICENSE_MIGRATION_DOCKER_TEST=1 for isolated actual Docker evidence")
	}
	binary, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("explicit Docker evidence test requires Docker:", err)
	}
	runner := ExecRunner{DockerBinary: binary}
	command := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return runner.RunOutput(ctx, maxOutput, "docker", args...)
	}
	var image string
	for _, tag := range []string{"alpine:3.21", "alpine:3.20"} {
		b, err := command("image", "inspect", "--format", "{{.Id}}", tag)
		if err == nil && digest.MatchString(strings.TrimSpace(string(b))) {
			image = strings.TrimSpace(string(b))
			break
		}
	}
	if image == "" {
		t.Fatal("explicit Docker test requires cached alpine:3.21 or alpine:3.20; no image is downloaded")
	}
	// Some legacy images omit Config.Labels itself, not only the protocol key.
	const labelsProjection = `{"id":{{json .Id}},"version":{{with index .Config "Labels"}}{{json (index . "org.opencontainers.image.version")}}{{else}}null{{end}},"protocol":{{with index .Config "Labels"}}{{json (index . "com.basic-platform.license.protocol")}}{{else}}null{{end}}}`
	b, err := command("image", "inspect", "--format", labelsProjection, image)
	var actual imageProjection
	if err != nil || json.Unmarshal(b, &actual) != nil || actual.ID != image || actual.Version != "" || actual.Protocol != "" {
		t.Fatal("cached image must actually have no release/version protocol labels", err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	project := "license-migration-test-" + hex.EncodeToString(nonce[:])
	var owned []string
	t.Cleanup(func() {
		for i := len(owned) - 1; i >= 0; i-- {
			id := owned[i]
			if !containerID.MatchString(id) {
				t.Errorf("refusing invalid cleanup ID %q", id)
				continue
			}
			if _, err := command("container", "rm", "--force", id); err != nil {
				t.Errorf("exact owned container cleanup failed %s: %v", id, err)
			} else {
				t.Logf("removed exact owned container %s", id)
			}
		}
	})
	start := func(service string) {
		b, err := command("container", "create", "--name", project+"-"+service, "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--label", "com.docker.compose.project="+project, "--label", "com.docker.compose.service="+service, image, "sleep", "300")
		if err != nil {
			t.Fatal("create isolated container:", err)
		}
		id := strings.TrimSpace(string(b))
		if !containerID.MatchString(id) {
			t.Fatal("unexpected Docker create ID")
		}
		owned = append(owned, id)
		if _, err := command("container", "start", id); err != nil {
			t.Fatal("start isolated container:", err)
		}
		t.Logf("created exact owned container %s service=%s project=%s image=%s", id, service, project, image)
	}
	cfg := InstallationConfig{Project: project}
	collect := func(t *testing.T) Report {
		t.Helper()
		c, err := NewMigrationInstallationCollector(runner, cfg)
		if err != nil {
			t.Fatal(err)
		}
		r := c.Collect(context.Background())
		if r.Scope != "MIGRATION_INSTALLATION" || !r.BoundarySupported || r.Project != project {
			t.Fatal(r)
		}
		return r
	}
	t.Run("empty-project", func(t *testing.T) {
		r := collect(t)
		if !r.Complete || len(r.Facts) != 0 {
			t.Fatal(r)
		}
	})
	start("contract-api")
	cfg.Services = append(cfg.Services, Service{Application: "contract_management", Environment: "prod", Name: "contract-api", ImageDigest: image, Replicas: 1})
	t.Run("actual-legacy-image", func(t *testing.T) {
		r := collect(t)
		if !r.Complete || len(r.Facts) != 1 {
			t.Fatal(r)
		}
		f := r.Facts[0]
		if f.ImageDigest != image || f.Protocol != "" || f.Version != "" || !f.Running || f.StartedAt.IsZero() || !containerID.MatchString(f.ContainerID) {
			t.Fatal(f)
		}
	})
	start("project-api")
	cfg.Services = append(cfg.Services, Service{Application: "project_management", Environment: "prod", Name: "project-api", ImageDigest: image, Replicas: 1})
	t.Run("two-actual-business-units", func(t *testing.T) {
		r := collect(t)
		if !r.Complete || len(r.Facts) != 2 {
			t.Fatal(r)
		}
	})
	start("customer-api")
	t.Run("unknown-business-rejected", func(t *testing.T) {
		r := collect(t)
		if r.Complete || !strings.Contains(strings.Join(r.Problems, ","), "unknown_installation_execution_unit") {
			t.Fatal(r)
		}
	})
}
