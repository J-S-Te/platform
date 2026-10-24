package evidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type initializationRunner struct {
	fixtureRunner
	service string
}

func (r *initializationRunner) RunOutput(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	if args[0] == "container" {
		r.calls = append(r.calls, append([]string{name}, args...))
		return json.Marshal(containerProjection{ID: strings.Repeat("a", 64), Project: "platform-prod", Service: r.service, Running: false})
	}
	return r.fixtureRunner.RunOutput(ctx, limit, name, args...)
}

func TestExitedInitializationRequiresExplicitExclusion(t *testing.T) {
	for _, name := range []string{"platform-key-init", "subsystem-provisioner-socket-init"} {
		for _, approved := range []bool{false, true} {
			r := &initializationRunner{fixtureRunner: *validRunner(), service: name}
			cfg := InstallationConfig{Project: "platform-prod"}
			if approved {
				cfg.ExcludedServices = []string{name}
			}
			c, err := NewInstallationCollector(r, cfg)
			if err != nil {
				t.Fatal(err)
			}
			report := c.Collect(context.Background())
			if report.Complete != approved || len(report.Facts) != 0 {
				t.Fatalf("exited init boundary incorrect: %+v", report)
			}
			if !approved && !strings.Contains(strings.Join(report.Problems, ","), "unknown_installation_execution_unit") {
				t.Fatal("unknown stopped init hidden", report)
			}
			if len(r.calls) < 2 || !strings.Contains(strings.Join(r.calls[0], " "), "ps --all --no-trunc") {
				t.Fatal("stopped units not inventoried", r.calls)
			}
		}
	}
}

func TestInstallationEvidenceFullBoundary(t *testing.T) {
	for _, tt := range []struct {
		name            string
		empty, approved bool
		complete        bool
	}{{"platform-only", true, false, true}, {"unknown-business", false, false, false}, {"approved-business", false, true, true}, {"missing-business", true, true, false}} {
		t.Run(tt.name, func(t *testing.T) {
			r := validRunner()
			r.empty = tt.empty
			cfg := InstallationConfig{Project: "platform-prod"}
			if tt.approved {
				cfg.Services = validConfig().Services
			}
			c, err := NewInstallationCollector(r, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := c.Collect(context.Background())
			if got.Complete != tt.complete || got.Scope != "INSTALLATION" || !got.BoundarySupported {
				t.Fatal(got)
			}
		})
	}
}
func TestInstallationFailureCannotProveEmpty(t *testing.T) {
	r := validRunner()
	r.fail = true
	c, _ := NewInstallationCollector(r, InstallationConfig{Project: "platform-prod"})
	got := c.Collect(context.Background())
	if got.Complete || len(got.Problems) == 0 {
		t.Fatal(got)
	}
}
func TestBusinessExclusionRejected(t *testing.T) {
	cfg := InstallationConfig{Project: "platform-prod", Services: validConfig().Services, ExcludedServices: []string{"project-api"}}
	if _, err := NewInstallationCollector(validRunner(), cfg); err == nil {
		t.Fatal("business exclusion accepted")
	}
}
