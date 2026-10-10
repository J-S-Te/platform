package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Infrastructure is separately approved deployment evidence, never a protocol
// consumer or an exemption from inspection. Only the reviewed BI engine is valid.
type Infrastructure struct {
	Application    string `json:"application"`
	Environment    string `json:"environment"`
	Name           string `json:"name"`
	ImageDigest    string `json:"image_digest"`
	Replicas       int    `json:"replicas"`
	GatewayService string `json:"gateway_service"`
}

func ReviewedControlledBIInfrastructure(application, name string) bool {
	return application == "data_analysis" && name == "data-analysis-metabase"
}

func validateInfrastructure(items []Infrastructure, services []Service) error {
	if len(items) > 1 {
		return errors.New("invalid controlled infrastructure inventory")
	}
	for _, item := range items {
		if !ReviewedControlledBIInfrastructure(item.Application, item.Name) || !identifier.MatchString(item.Environment) || !digest.MatchString(item.ImageDigest) || item.Replicas < 1 || item.Replicas > 64 || item.GatewayService != "data-analysis-api" {
			return errors.New("invalid controlled infrastructure inventory")
		}
		gateway := false
		for _, service := range services {
			if service.Name == item.Name {
				return errors.New("infrastructure cannot be a business consumer")
			}
			if service.Name == item.GatewayService && service.Application == item.Application && service.Environment == item.Environment {
				gateway = true
			}
		}
		if !gateway {
			return errors.New("controlled infrastructure requires approved BI gateway")
		}
	}
	return nil
}

// No environment, secrets, or unrestricted label/host configuration is read.
const infrastructureContainerFormat = `{"id":{{json .Id}},"image":{{json .Image}},"running":{{json .State.Running}},"started_at":{{json .State.StartedAt}},"project":{{json (index .Config.Labels "com.docker.compose.project")}},"service":{{json (index .Config.Labels "com.docker.compose.service")}},"oneoff":{{json (index .Config.Labels "com.docker.compose.oneoff")}},"network_mode":{{json .HostConfig.NetworkMode}},"ports":{{json .NetworkSettings.Ports}},"networks":{{json .NetworkSettings.Networks}}}`
const infrastructureImageFormat = `{"id":{{json .Id}}}`

type infrastructureProjection struct {
	containerProjection
	NetworkMode string `json:"network_mode"`
	Ports       map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	} `json:"ports"`
	Networks map[string]struct {
		ID string `json:"NetworkID"`
	} `json:"networks"`
}

func (c *Collector) collectInfrastructure(ctx context.Context, report *Report) {
	for _, item := range c.config.Infrastructure {
		b, err := c.run(ctx, "ps", "--all", "--no-trunc", "--filter", "label=com.docker.compose.project="+c.config.Project, "--filter", "label=com.docker.compose.service="+item.Name, "--format", "{{.ID}}")
		if err != nil {
			report.Complete = false
			report.Problems = append(report.Problems, item.Name+":collection_failed")
			continue
		}
		ids := strings.Fields(string(b))
		if len(ids) != item.Replicas {
			report.Complete = false
			report.Problems = append(report.Problems, item.Name+":replica_count_mismatch")
		}
		if len(ids) > 64 {
			report.Complete = false
			report.Problems = append(report.Problems, item.Name+":inventory_limit_exceeded")
			continue
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !containerID.MatchString(id) || seen[id] {
				report.Complete = false
				report.Problems = append(report.Problems, item.Name+":invalid_container_inventory")
				continue
			}
			seen[id] = true
			fact := c.inspectInfrastructure(ctx, id, item, report.Facts)
			if len(fact.Problems) > 0 {
				report.Complete = false
			}
			report.InfrastructureFacts = append(report.InfrastructureFacts, fact)
		}
	}
}

func (c *Collector) inspectInfrastructure(ctx context.Context, id string, item Infrastructure, business []Fact) Fact {
	f := Fact{Application: item.Application, Environment: item.Environment, Service: item.Name, ContainerID: id, Problems: []string{}}
	b, err := c.run(ctx, "container", "inspect", "--format", infrastructureContainerFormat, id)
	var p infrastructureProjection
	if err != nil || json.Unmarshal(b, &p) != nil {
		f.Problems = append(f.Problems, "container_inspection_failed")
		return f
	}
	if p.ID != id || p.Project != c.config.Project || p.Service != item.Name || strings.EqualFold(p.Oneoff, "true") {
		f.Problems = append(f.Problems, "compose_binding_mismatch")
	}
	f.Running, f.StartedAt = p.Running, p.StartedAt
	if !p.Running || p.StartedAt.IsZero() || p.StartedAt.After(time.Now().UTC().Add(time.Minute)) {
		f.Problems = append(f.Problems, "not_running")
	}
	if !digest.MatchString(p.Image) || p.Image != item.ImageDigest {
		f.Problems = append(f.Problems, "unapproved_image_digest")
		return f
	}
	f.ImageDigest = p.Image
	b, err = c.run(ctx, "image", "inspect", "--format", infrastructureImageFormat, p.Image)
	var image imageProjection
	if err != nil || json.Unmarshal(b, &image) != nil || image.ID != p.Image {
		f.Problems = append(f.Problems, "image_inspection_failed")
	}
	if p.NetworkMode == "host" || p.NetworkMode == "none" || strings.HasPrefix(p.NetworkMode, "container:") {
		f.Problems = append(f.Problems, "unsafe_network_mode")
	}
	for _, bindings := range p.Ports {
		if len(bindings) > 0 {
			f.Problems = append(f.Problems, "published_infrastructure_port")
			break
		}
	}
	shared := false
	for _, gateway := range business {
		if gateway.Service != item.GatewayService || gateway.Application != item.Application || gateway.Environment != item.Environment || !gateway.Running || len(gateway.Problems) != 0 {
			continue
		}
		b, err := c.run(ctx, "container", "inspect", "--format", infrastructureContainerFormat, gateway.ContainerID)
		var g infrastructureProjection
		if err != nil || json.Unmarshal(b, &g) != nil || g.ID != gateway.ContainerID || g.Project != c.config.Project || g.Service != item.GatewayService || !g.Running || strings.EqualFold(g.Oneoff, "true") || g.Image != gateway.ImageDigest {
			continue
		}
		if g.NetworkMode == "host" || g.NetworkMode == "none" || strings.HasPrefix(g.NetworkMode, "container:") {
			continue
		}
		for _, network := range p.Networks {
			for _, other := range g.Networks {
				if containerID.MatchString(network.ID) && network.ID == other.ID {
					shared = true
				}
			}
		}
	}
	if !shared {
		f.Problems = append(f.Problems, "gateway_network_unverified")
	}
	return f
}
