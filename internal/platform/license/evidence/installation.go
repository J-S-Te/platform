package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// InstallationConfig must describe the entire operator-approved Compose
// project. ExcludedServices are explicit reviewed infrastructure/init units,
// never values inferred from container labels. Empty Services is permitted only
// here, and only a successful complete project scan can prove platform-only.
type InstallationConfig struct {
	Project          string
	Services         []Service
	Infrastructure   []Infrastructure
	ExcludedServices []string
	Timeout          time.Duration
}
type InstallationCollector struct {
	collector *Collector
	excluded  map[string]bool
}

func NewInstallationCollector(r Runner, cfg InstallationConfig) (*InstallationCollector, error) {
	return newInstallationCollector(r, cfg, false)
}

func newInstallationCollector(r Runner, cfg InstallationConfig, migration bool) (*InstallationCollector, error) {
	if r == nil || !identifier.MatchString(cfg.Project) || len(cfg.ExcludedServices) > 128 || len(cfg.Services) > 64 {
		return nil, errors.New("approved installation boundary required")
	}
	base := Config{Project: cfg.Project, Services: cfg.Services, Infrastructure: cfg.Infrastructure, Timeout: cfg.Timeout}
	if base.Timeout == 0 {
		base.Timeout = 30 * time.Second
	}
	if base.Timeout < time.Second || base.Timeout > time.Minute {
		return nil, errors.New("invalid evidence timeout")
	}
	var c *Collector
	if len(cfg.Services) > 0 {
		var err error
		c, err = newCollector(r, base, migration)
		if err != nil {
			return nil, err
		}
	} else {
		if err := validateInfrastructure(cfg.Infrastructure, cfg.Services); err != nil {
			return nil, err
		}
		c = &Collector{runner: r, config: base, migration: migration}
	}
	excluded := map[string]bool{}
	for _, name := range cfg.ExcludedServices {
		if !identifier.MatchString(name) || excluded[name] || name == "data-analysis-metabase" {
			return nil, errors.New("invalid excluded unit")
		}
		excluded[name] = true
	}
	for _, service := range cfg.Services {
		if excluded[service.Name] {
			return nil, errors.New("business unit cannot be excluded")
		}
	}
	for _, infrastructure := range cfg.Infrastructure {
		if excluded[infrastructure.Name] {
			return nil, errors.New("controlled infrastructure cannot be excluded")
		}
	}
	return &InstallationCollector{collector: c, excluded: excluded}, nil
}

func (c *InstallationCollector) Collect(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, c.collector.config.Timeout)
	defer cancel()
	r := Report{Scope: "INSTALLATION", BoundarySupported: true, Project: c.collector.config.Project, CollectedAt: time.Now().UTC(), Complete: true, Facts: []Fact{}, Problems: []string{}}
	if c.collector.migration {
		r.Scope = "MIGRATION_INSTALLATION"
	}
	b, err := c.collector.run(ctx, "ps", "--all", "--no-trunc", "--filter", "label=com.docker.compose.project="+r.Project, "--format", "{{.ID}}")
	if err != nil {
		r.Complete = false
		r.Problems = append(r.Problems, "installation_collection_failed")
		return r
	}
	ids := strings.Fields(string(b))
	if len(ids) > 128 {
		r.Complete = false
		r.Problems = append(r.Problems, "installation_inventory_limit_exceeded")
		return r
	}
	approved := map[string]Service{}
	infrastructure := map[string]bool{}
	for _, item := range c.collector.config.Infrastructure {
		infrastructure[item.Name] = true
	}
	count := map[string]int{}
	seen := map[string]bool{}
	for _, service := range c.collector.config.Services {
		approved[service.Name] = service
	}
	for _, id := range ids {
		if !containerID.MatchString(id) || seen[id] {
			r.Complete = false
			r.Problems = append(r.Problems, "invalid_installation_container")
			continue
		}
		seen[id] = true
		b, err := c.collector.run(ctx, "container", "inspect", "--format", containerFormat, id)
		var p containerProjection
		if err != nil || json.Unmarshal(b, &p) != nil || p.ID != id || p.Project != r.Project {
			r.Complete = false
			r.Problems = append(r.Problems, "installation_container_inspection_failed")
			continue
		}
		if c.excluded[p.Service] {
			continue
		}
		if infrastructure[p.Service] {
			continue
		}
		s, ok := approved[p.Service]
		if !ok {
			r.Complete = false
			r.Problems = append(r.Problems, "unknown_installation_execution_unit")
			continue
		}
		count[s.Name]++
		f := c.collector.inspect(ctx, id, s)
		if len(f.Problems) > 0 {
			r.Complete = false
		}
		r.Facts = append(r.Facts, f)
	}
	for name, s := range approved {
		if count[name] != s.Replicas {
			r.Complete = false
			r.Problems = append(r.Problems, name+":replica_count_mismatch")
		}
	}
	c.collector.collectInfrastructure(ctx, &r)
	return r
}
