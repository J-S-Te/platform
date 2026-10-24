package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fixtureRunner struct {
	calls    [][]string
	fail     bool
	empty    bool
	running  bool
	project  string
	version  string
	image    string
	oversize bool
}

func (r *fixtureRunner) RunOutput(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if limit != maxOutput {
		panic("missing output bound")
	}
	if r.fail {
		return []byte("SECRET_PASSWORD"), errors.New("SECRET_PASSWORD")
	}
	if r.oversize {
		return []byte(strings.Repeat("x", maxOutput+1)), nil
	}
	if args[0] == "ps" {
		if r.empty {
			return nil, nil
		}
		return []byte(strings.Repeat("a", 64)), nil
	}
	if args[0] == "container" {
		return json.Marshal(containerProjection{ID: strings.Repeat("a", 64), Image: r.image, Running: r.running, StartedAt: time.Now().Add(-time.Hour), Project: r.project, Service: "project-api", Oneoff: "False"})
	}
	return json.Marshal(imageProjection{ID: r.image, Version: r.version, Protocol: "1"})
}
func validConfig() Config {
	return Config{Project: "platform-prod", Services: []Service{{Application: "project_management", Environment: "prod", Name: "project-api", ImageDigest: "sha256:" + strings.Repeat("b", 64), Version: "release-1", Protocol: "1", Replicas: 1}}}
}
func validRunner() *fixtureRunner {
	return &fixtureRunner{running: true, project: "platform-prod", version: "release-1", image: "sha256:" + strings.Repeat("b", 64)}
}

func TestApprovedReleaseFactsOnly(t *testing.T) {
	r := validRunner()
	config := validConfig()
	c, err := NewCollector(r, config)
	if err != nil {
		t.Fatal(err)
	}
	config.Services[0].ImageDigest = "mutated"
	report := c.Collect(context.Background())
	if !report.Complete || len(report.Facts) != 1 || report.Facts[0].Protocol != "1" {
		t.Fatalf("unexpected facts: %+v", report)
	}
	if len(r.calls) != 3 {
		t.Fatal(r.calls)
	}
	for _, call := range r.calls {
		all := strings.Join(call, " ")
		if strings.Contains(all, ".Env") || strings.Contains(all, "json .Config.Labels") {
			t.Fatal("unrestricted inspect", call)
		}
	}
	if !strings.Contains(strings.Join(r.calls[0], " "), "--all --no-trunc") {
		t.Fatal("stopped containers would be hidden")
	}
}

func TestFailuresDistinctFromEmptyInventory(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*fixtureRunner)
		want  string
	}{
		{"failed", func(r *fixtureRunner) { r.fail = true }, "collection_failed"},
		{"empty", func(r *fixtureRunner) { r.empty = true }, "replica_count_mismatch"},
		{"oversize", func(r *fixtureRunner) { r.oversize = true }, "collection_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := validRunner()
			tt.setup(r)
			c, _ := NewCollector(r, validConfig())
			got := c.Collect(context.Background())
			if got.Complete || len(got.Problems) != 1 || !strings.Contains(got.Problems[0], tt.want) {
				t.Fatal(got)
			}
			b, _ := json.Marshal(got)
			if strings.Contains(string(b), "SECRET") {
				t.Fatal("error leaked")
			}
		})
	}
}

func TestLabelsAndRunningCannotAuthorizeUnapprovedImage(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*fixtureRunner)
		want  string
	}{
		{"stopped", func(r *fixtureRunner) { r.running = false }, "not_running"},
		{"wrongproject", func(r *fixtureRunner) { r.project = "attacker" }, "compose_binding_mismatch"},
		{"tag", func(r *fixtureRunner) { r.image = "latest" }, "unapproved_image_digest"},
		{"wrongdigest", func(r *fixtureRunner) { r.image = "sha256:" + strings.Repeat("c", 64) }, "unapproved_image_digest"},
		{"version", func(r *fixtureRunner) { r.version = "release-2" }, "release_evidence_mismatch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := validRunner()
			tt.setup(r)
			c, _ := NewCollector(r, validConfig())
			got := c.Collect(context.Background())
			if got.Complete || len(got.Facts) != 1 || !strings.Contains(strings.Join(got.Facts[0].Problems, ","), tt.want) {
				t.Fatal(got)
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	for _, mutate := range []func(*Config){func(c *Config) { c.Services = nil }, func(c *Config) { c.Project = "p;uname" }, func(c *Config) { c.Services[0].ImageDigest = "registry/image:latest" }, func(c *Config) { c.Services = append(c.Services, c.Services[0]) }, func(c *Config) { c.Timeout = time.Hour }, func(c *Config) { c.Services[0].Replicas = 0 }} {
		c := validConfig()
		mutate(&c)
		if _, err := NewCollector(validRunner(), c); err == nil {
			t.Fatal("accepted invalid inventory")
		}
	}
}

func TestCanceledCollectionFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := NewCollector(validRunner(), validConfig())
	if c.Collect(ctx).Complete {
		t.Fatal("canceled collection accepted")
	}
}

func TestBoundedBuffer(t *testing.T) {
	b := &boundedBuffer{limit: 4}
	if _, err := b.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("5")); err == nil || b.Len() != 4 {
		t.Fatal("output bound failed")
	}
}
