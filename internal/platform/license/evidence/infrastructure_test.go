package evidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type biRunner struct {
	missing, extra, ports, badNetwork, host, wrongImage, wrongProject, stopped bool
}

func (r *biRunner) RunOutput(_ context.Context, limit int, command string, args ...string) ([]byte, error) {
	if limit != maxOutput || command != "docker" {
		panic("unbounded command")
	}
	apiID, biID := strings.Repeat("a", 64), strings.Repeat("c", 64)
	if args[0] == "ps" {
		filter := strings.Join(args, " ")
		if strings.Contains(filter, "service=data-analysis-api") {
			return []byte(apiID), nil
		}
		if strings.Contains(filter, "service=data-analysis-metabase") {
			if r.missing {
				return nil, nil
			}
			if r.extra {
				return []byte(biID + "\n" + strings.Repeat("d", 64)), nil
			}
			return []byte(biID), nil
		}
		if r.missing {
			return []byte(apiID), nil
		}
		return []byte(apiID + "\n" + biID), nil
	}
	if args[0] == "image" {
		if args[3] == infrastructureImageFormat {
			return json.Marshal(struct {
				ID string `json:"id"`
			}{args[len(args)-1]})
		}
		return json.Marshal(imageProjection{ID: args[len(args)-1], Version: "release-1", Protocol: "1"})
	}
	id := args[len(args)-1]
	p := infrastructureProjection{containerProjection: containerProjection{ID: id, Image: "sha256:" + strings.Repeat("b", 64), Running: true, StartedAt: time.Now().Add(-time.Hour), Project: "platform-prod", Service: "data-analysis-api"}, NetworkMode: "platform-prod_default", Networks: map[string]struct {
		ID string `json:"NetworkID"`
	}{"default": {ID: strings.Repeat("e", 64)}}}
	if id != apiID {
		p.Service = "data-analysis-metabase"
		p.Image = "sha256:" + strings.Repeat("f", 64)
		if r.wrongImage {
			p.Image = "sha256:" + strings.Repeat("9", 64)
		}
		if r.wrongProject {
			p.Project = "foreign"
		}
		if r.stopped {
			p.Running = false
		}
		if r.host {
			p.NetworkMode = "host"
		}
		if r.badNetwork {
			p.Networks = nil
		}
		if r.ports {
			p.Ports = map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			}{"3000/tcp": {{HostPort: "3000"}}}
		}
	}
	return json.Marshal(p)
}
func biConfig() Config {
	return Config{Project: "platform-prod", Services: []Service{{Application: "data_analysis", Environment: "prod", Name: "data-analysis-api", ImageDigest: "sha256:" + strings.Repeat("b", 64), Version: "release-1", Protocol: "1", Replicas: 1}}, Infrastructure: []Infrastructure{{Application: "data_analysis", Environment: "prod", Name: "data-analysis-metabase", ImageDigest: "sha256:" + strings.Repeat("f", 64), Replicas: 1, GatewayService: "data-analysis-api"}}}
}
func TestControlledBIInfrastructureEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		runner biRunner
		pass   bool
	}{
		{"official-no-protocol-labels", biRunner{}, true}, {"missing", biRunner{missing: true}, false}, {"extra-replica", biRunner{extra: true}, false}, {"published-port", biRunner{ports: true}, false}, {"unshared-network", biRunner{badNetwork: true}, false}, {"host-network", biRunner{host: true}, false}, {"substituted-image", biRunner{wrongImage: true}, false}, {"foreign-project", biRunner{wrongProject: true}, false}, {"stopped", biRunner{stopped: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := biConfig()
			c, err := NewCollector(&test.runner, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := c.Collect(context.Background())
			if got.Complete != test.pass || len(got.Facts) != 1 {
				t.Fatalf("application: %+v", got)
			}
			if test.pass && (len(got.InfrastructureFacts) != 1 || got.InfrastructureFacts[0].Protocol != "") {
				t.Fatal("infrastructure incorrectly became a consumer", got)
			}
			i, err := NewInstallationCollector(&test.runner, InstallationConfig{Project: cfg.Project, Services: cfg.Services, Infrastructure: cfg.Infrastructure})
			if err != nil {
				t.Fatal(err)
			}
			if got := i.Collect(context.Background()); got.Complete != test.pass {
				t.Fatalf("installation: %+v", got)
			}
		})
	}
}
func TestInfrastructureCannotLaunderBusinessOrBypassGateway(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Infrastructure[0].Name = "project-api" },
		func(c *Config) { c.Infrastructure[0].Application = "project_management" },
		func(c *Config) { c.Infrastructure[0].GatewayService = "project-api" },
		func(c *Config) { c.Infrastructure[0].ImageDigest = "metabase:latest" },
		func(c *Config) { c.Infrastructure[0].Environment = "other" },
		func(c *Config) { c.Infrastructure = append(c.Infrastructure, c.Infrastructure[0]) },
		func(c *Config) { c.Services = nil },
	} {
		cfg := biConfig()
		mutate(&cfg)
		if _, err := NewCollector(&biRunner{}, cfg); err == nil {
			t.Fatal("application accepted unsafe inventory")
		}
		if _, err := NewInstallationCollector(&biRunner{}, InstallationConfig{Project: cfg.Project, Services: cfg.Services, Infrastructure: cfg.Infrastructure}); err == nil {
			t.Fatal("installation accepted unsafe inventory")
		}
	}
	cfg := biConfig()
	if _, err := NewInstallationCollector(&biRunner{}, InstallationConfig{Project: cfg.Project, Services: cfg.Services, Infrastructure: cfg.Infrastructure, ExcludedServices: []string{"data-analysis-metabase"}}); err == nil {
		t.Fatal("controlled infrastructure excluded")
	}
	if _, err := NewInstallationCollector(&biRunner{}, InstallationConfig{Project: cfg.Project, ExcludedServices: []string{"data-analysis-metabase"}}); err == nil {
		t.Fatal("controlled infrastructure became unchecked exclusion")
	}
	i, err := NewInstallationCollector(&biRunner{}, InstallationConfig{Project: cfg.Project, Services: cfg.Services})
	if err != nil {
		t.Fatal(err)
	}
	if got := i.Collect(context.Background()); got.Complete || !strings.Contains(strings.Join(got.Problems, ","), "unknown_installation_execution_unit") {
		t.Fatal("unknown infrastructure accepted", got)
	}
}
