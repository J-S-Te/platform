package infrastructure

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// SubsystemImageReadiness reads the selected reviewed Compose topology. V2 images
// remain immutable release inputs, but do not require legacy V1 enrollment files.
func (p *ProductionComposeSubsystemProvisioner) SubsystemImageReadiness(ctx context.Context, app, env string) ([]application.SubsystemImageReadiness, error) {
	original, err := p.target(app, env)
	if err != nil {
		return nil, err
	}
	// Read-only inventory must not change the target cached by a concurrent deployment.
	target := &productionComposeTarget{config: original.config, runner: original.runner}
	if err = target.validateEnabledServices(ctx); err != nil {
		return nil, err
	}
	outputRunner, ok := target.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return nil, application.ErrRuntimeLicenseDockerUnavailable
	}
	if err = target.runner.Run(ctx, target.config.DeployRoot, os.Environ(), target.config.DockerBinary, "version", "--format", "{{.Server.Version}}"); err != nil {
		return nil, application.ErrRuntimeLicenseDockerUnavailable
	}
	raw, err := target.runComposeOutput(ctx, "config", "--format", "json")
	if err != nil {
		return nil, application.ErrRuntimeLicenseDockerUnavailable
	}
	// Resolved configuration can contain secrets. Keep it in memory, expose images only.
	var config struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if json.Unmarshal(raw, &config) != nil {
		return nil, application.ErrValidation
	}
	services := append([]string{}, target.selectedRuntimeServices()...)
	services = append(services, target.config.Profile.Manifest.Compose.InitializationServices...)
	services = append(services, target.config.Profile.Manifest.Compose.MigrateService, target.config.Profile.Manifest.Compose.CatalogSyncService)
	slices.Sort(services)
	services = slices.Compact(services)
	images := []application.SubsystemImageReadiness{}
	for _, service := range services {
		if service == "" {
			continue
		}
		image := strings.TrimSpace(config.Services[service].Image)
		if !isImmutableProductionImageReference(image) {
			return nil, provisioningError("selected service requires immutable release image: " + service)
		}
		inspected, inspectErr := outputRunner.RunOutput(ctx, target.config.DeployRoot, os.Environ(), target.config.DockerBinary, "image", "inspect", "--format", "{{json .RepoDigests}}", image)
		var refs []string
		present := false
		if inspectErr == nil && json.Unmarshal(inspected, &refs) == nil {
			for _, ref := range refs {
				if strings.HasSuffix(ref, image[strings.LastIndex(image, "@"):]) {
					present = true
					break
				}
			}
		}
		images = append(images, application.SubsystemImageReadiness{ServiceID: service, ImageDigest: image, Present: present})
	}
	if len(images) == 0 {
		return nil, application.ErrValidation
	}
	return images, nil
}
