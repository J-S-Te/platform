// Package evidence collects bounded, read-only deployment facts on the isolated
// deployment Agent. A complete report is not itself migration authorization.
package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxOutput = 64 * 1024

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var digest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var containerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Runner must honor cancellation and limit output before allocation. Do not
// adapt an unbounded CombinedOutput runner or return stderr in public reports.
type Runner interface {
	RunOutput(context.Context, int, string, ...string) ([]byte, error)
}

// Service is operator-approved release metadata, never browser-supplied labels.
// ImageDigest is Docker's immutable image config ID, not a mutable image tag.
type Service struct {
	Application string `json:"application"`
	Environment string `json:"environment"`
	Name        string `json:"name"`
	ImageDigest string `json:"image_digest"`
	Version     string `json:"version"`
	Protocol    string `json:"protocol"`
	Replicas    int    `json:"replicas"`
}

type Config struct {
	Project        string
	Services       []Service
	Infrastructure []Infrastructure
	Timeout        time.Duration
}

type Fact struct {
	Application string    `json:"application"`
	Environment string    `json:"environment"`
	Service     string    `json:"service"`
	ContainerID string    `json:"container_id"`
	ImageDigest string    `json:"image_digest"`
	Version     string    `json:"version"`
	Protocol    string    `json:"protocol"`
	Running     bool      `json:"running"`
	StartedAt   time.Time `json:"started_at"`
	Problems    []string  `json:"problems"`
}

type Report struct {
	BoundarySupported   bool      `json:"boundary_supported"`
	Scope               string    `json:"scope"`
	Project             string    `json:"project"`
	CollectedAt         time.Time `json:"collected_at"`
	Complete            bool      `json:"complete"`
	Facts               []Fact    `json:"facts"`
	InfrastructureFacts []Fact    `json:"infrastructure_facts,omitempty"`
	Problems            []string  `json:"problems"`
}

type Collector struct {
	runner    Runner
	config    Config
	migration bool
}

func NewCollector(runner Runner, config Config) (*Collector, error) {
	return newCollector(runner, config, false)
}

func newCollector(runner Runner, config Config, migration bool) (*Collector, error) {
	if runner == nil || !identifier.MatchString(config.Project) || len(config.Services) == 0 || len(config.Services) > 64 {
		return nil, errors.New("approved deployment inventory required")
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	if config.Timeout < time.Second || config.Timeout > time.Minute {
		return nil, errors.New("invalid evidence timeout")
	}
	seen := make(map[string]bool)
	for _, s := range config.Services {
		validRelease := identifier.MatchString(s.Version) && identifier.MatchString(s.Protocol)
		if migration {
			validRelease = (s.Version == "" || identifier.MatchString(s.Version)) && (s.Protocol == "" || identifier.MatchString(s.Protocol))
		}
		if !identifier.MatchString(s.Application) || !identifier.MatchString(s.Environment) || !identifier.MatchString(s.Name) || !digest.MatchString(s.ImageDigest) || !validRelease || s.Replicas < 1 || s.Replicas > 64 || seen[s.Name] {
			return nil, errors.New("invalid approved service inventory")
		}
		seen[s.Name] = true
	}
	config.Services = append([]Service(nil), config.Services...)
	if err := validateInfrastructure(config.Infrastructure, config.Services); err != nil {
		return nil, err
	}
	config.Infrastructure = append([]Infrastructure(nil), config.Infrastructure...)
	return &Collector{runner: runner, config: config, migration: migration}, nil
}

// Projections deliberately exclude environment variables and arbitrary labels.
const containerFormat = `{"id":{{json .Id}},"image":{{json .Image}},"running":{{json .State.Running}},"started_at":{{json .State.StartedAt}},"project":{{json (index .Config.Labels "com.docker.compose.project")}},"service":{{json (index .Config.Labels "com.docker.compose.service")}},"oneoff":{{json (index .Config.Labels "com.docker.compose.oneoff")}}}`
const imageFormat = `{"id":{{json .Id}},"version":{{json (index .Config.Labels "org.opencontainers.image.version")}},"protocol":{{json (index .Config.Labels "com.basic-platform.license.protocol")}}}`
// Some genuine pre-license images omit the entire Labels map, rather than
// only its keys. Read it defensively only in the approved migration collector.
const migrationImageFormat = `{"id":{{json .Id}},"version":{{with index .Config "Labels"}}{{json (index . "org.opencontainers.image.version")}}{{else}}null{{end}},"protocol":{{with index .Config "Labels"}}{{json (index . "com.basic-platform.license.protocol")}}{{else}}null{{end}}}`

type containerProjection struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at"`
	Project   string    `json:"project"`
	Service   string    `json:"service"`
	Oneoff    string    `json:"oneoff"`
}
type imageProjection struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Protocol string `json:"protocol"`
}

func (c *Collector) run(ctx context.Context, args ...string) ([]byte, error) {
	b, err := c.runner.RunOutput(ctx, maxOutput, "docker", args...)
	if err != nil || ctx.Err() != nil || len(b) > maxOutput {
		return nil, errors.New("evidence command failed")
	}
	return b, nil
}

func (c *Collector) Collect(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	r := Report{Scope: "APPLICATION", Project: c.config.Project, CollectedAt: time.Now().UTC(), Complete: true, Facts: []Fact{}, Problems: []string{}}
	for _, service := range c.config.Services {
		b, err := c.run(ctx, "ps", "--all", "--no-trunc", "--filter", "label=com.docker.compose.project="+c.config.Project, "--filter", "label=com.docker.compose.service="+service.Name, "--format", "{{.ID}}")
		if err != nil {
			r.Problems = append(r.Problems, service.Name+":collection_failed")
			r.Complete = false
			continue
		}
		ids := strings.Fields(string(b))
		if len(ids) != service.Replicas {
			r.Problems = append(r.Problems, service.Name+":replica_count_mismatch")
			r.Complete = false
		}
		if len(ids) > 64 {
			r.Problems = append(r.Problems, service.Name+":inventory_limit_exceeded")
			r.Complete = false
			continue
		}
		seen := make(map[string]bool)
		for _, id := range ids {
			if !containerID.MatchString(id) || seen[id] {
				r.Problems = append(r.Problems, service.Name+":invalid_container_inventory")
				r.Complete = false
				continue
			}
			seen[id] = true
			f := c.inspect(ctx, id, service)
			if len(f.Problems) > 0 {
				r.Complete = false
			}
			r.Facts = append(r.Facts, f)
		}
	}
	sort.Slice(r.Facts, func(i, j int) bool {
		if r.Facts[i].Service == r.Facts[j].Service {
			return r.Facts[i].ContainerID < r.Facts[j].ContainerID
		}
		return r.Facts[i].Service < r.Facts[j].Service
	})
	c.collectInfrastructure(ctx, &r)
	return r
}

func (c *Collector) inspect(ctx context.Context, id string, s Service) Fact {
	f := Fact{Application: s.Application, Environment: s.Environment, Service: s.Name, ContainerID: id, Problems: []string{}}
	b, err := c.run(ctx, "container", "inspect", "--format", containerFormat, id)
	var p containerProjection
	if err != nil || json.Unmarshal(b, &p) != nil {
		f.Problems = append(f.Problems, "container_inspection_failed")
		return f
	}
	f.ImageDigest = p.Image
	f.Running = p.Running
	f.StartedAt = p.StartedAt
	if len(p.Image) > 71 {
		f.ImageDigest = ""
		f.Problems = append(f.Problems, "invalid_image_projection")
		return f
	}
	if p.ID != id || p.Project != c.config.Project || p.Service != s.Name || strings.EqualFold(p.Oneoff, "true") {
		f.Problems = append(f.Problems, "compose_binding_mismatch")
	}
	if !p.Running || p.StartedAt.IsZero() || p.StartedAt.After(time.Now().UTC().Add(time.Minute)) {
		f.Problems = append(f.Problems, "not_running")
	}
	if !digest.MatchString(p.Image) || p.Image != s.ImageDigest {
		f.Problems = append(f.Problems, "unapproved_image_digest")
		return f
	}
	format := imageFormat
	if c.migration {
		format = migrationImageFormat
	}
	b, err = c.run(ctx, "image", "inspect", "--format", format, p.Image)
	var image imageProjection
	if err != nil || json.Unmarshal(b, &image) != nil {
		f.Problems = append(f.Problems, "image_inspection_failed")
		return f
	}
	validRelease := identifier.MatchString(image.Version) && identifier.MatchString(image.Protocol)
	if c.migration {
		validRelease = (image.Version == "" || identifier.MatchString(image.Version)) && (image.Protocol == "" || identifier.MatchString(image.Protocol))
	}
	if !validRelease {
		f.Problems = append(f.Problems, "invalid_release_projection")
		return f
	}
	f.Version = image.Version
	f.Protocol = image.Protocol
	if image.ID != p.Image || image.Version != s.Version || image.Protocol != s.Protocol {
		f.Problems = append(f.Problems, "release_evidence_mismatch")
	}
	return f
}
