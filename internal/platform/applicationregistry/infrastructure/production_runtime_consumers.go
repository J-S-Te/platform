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

// 该索引仅由受审清单推导；它有意不改变 selectedRuntimeServices、许可批准或下线范围。
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
	// 重试时同样刷新已选共享路径，即使文件字节已相同：此前一次失败可能让某个进程
	// 仍拿着旧秘密。
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
			continue // 绝不安装或重启已停止或尚未接入的子系统。
		}
		if len(ids) != 1 || !runtimeRetirementContainerID.MatchString(ids[0]) {
			return provisioningError("runtime consumer container ownership is ambiguous")
		}
		// 不要检查 Config.Env：其中含秘密。重载只需要不可变身份、运行状态与 Compose
		// 归属 label。
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
	// 把每个跨应用消费者钉在其实际运行镜像上。该重载绝不能收编新发布版本，
	// 也不能修改许可组件覆盖范围。
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
