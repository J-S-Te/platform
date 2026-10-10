package evidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type migrationRunner struct {
	*fixtureRunner
	service   string
	startedAt time.Time
}

func (r migrationRunner) RunOutput(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	b, err := r.fixtureRunner.RunOutput(ctx, limit, name, args...)
	if err != nil {
		return b, err
	}
	if args[0] == "image" {
		return json.Marshal(imageProjection{ID: r.image})
	}
	if args[0] == "container" {
		var p containerProjection
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		p.Service, p.StartedAt = r.service, r.startedAt
		return json.Marshal(p)
	}
	return b, nil
}

func TestMigrationEvidenceLegacyLabelsAndStrictIsolation(t *testing.T) {
	cfg := InstallationConfig{Project: "platform-prod", Services: validConfig().Services}
	cfg.Services[0].Version, cfg.Services[0].Protocol = "", ""
	r := migrationRunner{fixtureRunner: validRunner(), service: "project-api", startedAt: time.Now().Add(-time.Hour)}
	c, err := NewMigrationInstallationCollector(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Collect(context.Background())
	if !got.Complete || got.Scope != "MIGRATION_INSTALLATION" || !got.BoundarySupported || len(got.Facts) != 1 {
		t.Fatal(got)
	}
	if _, err := NewInstallationCollector(r, cfg); err == nil {
		t.Fatal("ordinary installation accepted missing release approval")
	}
	strict, _ := NewCollector(r, validConfig())
	if strict.Collect(context.Background()).Complete {
		t.Fatal("ordinary collector accepted legacy image labels")
	}
}

func TestMigrationEvidenceRuntimeAndImmutableBoundary(t *testing.T) {
	for _, tc := range []string{"stopped", "missing-start", "new-image", "unknown-unit", "wrong-project", "missing-runtime"} {
		t.Run(tc, func(t *testing.T) {
			cfg := InstallationConfig{Project: "platform-prod", Services: validConfig().Services}
			cfg.Services[0].Version, cfg.Services[0].Protocol = "", ""
			r := migrationRunner{fixtureRunner: validRunner(), service: "project-api", startedAt: time.Now().Add(-time.Hour)}
			switch tc {
			case "stopped":
				r.running = false
			case "missing-start":
				r.startedAt = time.Time{}
			case "new-image":
				r.image = "sha256:" + strings.Repeat("c", 64)
			case "unknown-unit":
				r.service = "unknown-worker"
			case "wrong-project":
				r.project = "other-project"
			case "missing-runtime":
				r.empty = true
			}
			c, err := NewMigrationInstallationCollector(r, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := c.Collect(context.Background())
			if got.Complete {
				t.Fatal(got)
			}
		})
	}
}
