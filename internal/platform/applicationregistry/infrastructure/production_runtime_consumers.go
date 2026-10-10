package infrastructure

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// The index is derived exclusively from reviewed profiles. It intentionally
// does not alter selectedRuntimeServices, license approval, or teardown scope.
func productionRuntimeConsumers(profiles []productionSubsystemProfile) map[string][]string {
	index := make(map[string][]string)
	for _, profile := range profiles {
		for _, file := range profile.Manifest.Runtime.Files {
			for _, service := range file.Consumers {
				if !slices.Contains(index[file.Path], service) {
					index[file.Path] = append(index[file.Path], service)
				}
			}
		}
	}
	for path := range index {
		sort.Strings(index[path])
	}
	return index
}

func (target *productionComposeTarget) deployAndRefreshLocked(ctx context.Context, redactValues ...string) error {
	if err := target.deployLocked(ctx, redactValues...); err != nil {
		return err
	}
	return target.refreshRuntimeConsumers(ctx)
}

func (target *productionComposeTarget) refreshRuntimeConsumers(ctx context.Context) (result error) {
	current := make(map[string]bool)
	for _, service := range target.selectedRuntimeServices() {
		current[service] = true
	}
	candidates := make(map[string]bool)
	// Refresh selected shared paths on retry as well, even if the file now has
	// identical bytes: a prior failure may have left a process on the old secret.
	for _, file := range target.selectedRuntimeFiles() {
		for _, service := range target.config.RuntimeConsumers[file.Path] {
			if !current[service] && target.enabledServices[service] {
				candidates[service] = true
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	outputRunner, ok := target.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return provisioningError("runtime consumer refresh inspection unavailable")
	}
	names := make([]string, 0, len(candidates))
	for service := range candidates {
		names = append(names, service)
	}
	sort.Strings(names)
	images := make(map[string]map[string]string)
	for _, service := range names {
		if !validProductionComposeService(service) {
			return provisioningError("runtime consumer service policy is invalid")
		}
		listed, err := outputRunner.RunOutput(ctx, target.config.DeployRoot, nil, target.config.DockerBinary,
			"container", "ls", "--quiet", "--no-trunc", "--filter", "status=running",
			"--filter", "label=com.docker.compose.project="+target.config.ComposeProject,
			"--filter", "label=com.docker.compose.service="+service)
		if err != nil || len(listed) > 16384 {
			return provisioningError("runtime consumer container inventory failed")
		}
		ids := strings.Fields(string(listed))
		if len(ids) == 0 {
			continue // Never install or restart a stopped/unonboarded subsystem.
		}
		if len(ids) != 1 || !runtimeRetirementContainerID.MatchString(ids[0]) {
			return provisioningError("runtime consumer container ownership is ambiguous")
		}
		// Do not inspect Config.Env: it contains secrets. Only immutable identity,
		// running state and Compose ownership labels are needed for the reload.
		format := `{{json .Id}} {{json .Image}} {{json .State.Running}} {{json .Config.Labels}}`
		inspected, err := outputRunner.RunOutput(ctx, target.config.DeployRoot, nil, target.config.DockerBinary,
			"container", "inspect", "--format", format, ids[0])
		if err != nil || len(inspected) > 16384 {
			return provisioningError("runtime consumer container inspection failed")
		}
		decoder := json.NewDecoder(strings.NewReader(string(inspected)))
		var id, image string
		var running bool
		var labels map[string]string
		if decoder.Decode(&id) != nil || decoder.Decode(&image) != nil || decoder.Decode(&running) != nil || decoder.Decode(&labels) != nil ||
			id != ids[0] || !strings.HasPrefix(image, "sha256:") || !runtimeRetirementContainerID.MatchString(strings.TrimPrefix(image, "sha256:")) ||
			!running || labels["com.docker.compose.project"] != target.config.ComposeProject || labels["com.docker.compose.service"] != service ||
			labels["com.docker.compose.oneoff"] == "True" || labels["com.docker.compose.oneoff"] == "true" {
			return provisioningError("runtime consumer container scope mismatch")
		}
		images[service] = map[string]string{"image": image}
	}
	if len(images) == 0 {
		return nil
	}
	// Pin every cross-application consumer to its actual running image. This
	// reload must never adopt a new release or modify license component coverage.
	content, err := json.Marshal(map[string]any{"services": images})
	if err != nil {
		return provisioningError("runtime consumer refresh override failed")
	}
	file, err := os.CreateTemp(filepath.Join(target.config.DeployRoot, "runtime"), ".consumer-refresh-*.json")
	if err != nil {
		return provisioningError("runtime consumer refresh override unavailable")
	}
	path := file.Name()
	defer func() {
		if cleanupErr := os.Remove(path); cleanupErr != nil && result == nil {
			result = provisioningError("runtime consumer refresh override cleanup failed")
		}
	}()
	if _, err = file.Write(content); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return provisioningError("runtime consumer refresh override write and close failed")
		}
		return provisioningError("runtime consumer refresh override write failed")
	}
	if err = file.Close(); err != nil {
		return provisioningError("runtime consumer refresh override close failed")
	}
	services := make([]string, 0, len(images))
	for service := range images {
		services = append(services, service)
	}
	sort.Strings(services)
	arguments, environment := target.composeCommand("--file", path, "up", "-d", "--wait", "--wait-timeout", "240",
		"--force-recreate", "--no-deps", "--no-build", "--pull", "never")
	arguments = append(arguments, services...)
	target.stepLog("step=runtime-consumer-refresh services=%v", services)
	if err = target.runner.Run(ctx, target.config.DeployRoot, environment, target.config.DockerBinary, arguments...); err != nil {
		return provisioningError("runtime consumer refresh failed; controlled retry required")
	}
	return nil
}
