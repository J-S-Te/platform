package infrastructure

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

var runtimeRetirementContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func runtimeLicenseRetirementPrefix(app string) string {
	return map[string]string{"contract_management": "contract-", "project_management": "project-", "customer_and_opportunity": "customer-", "customer_portal": "portal-", "settlement": "settlement-", "data_analysis": "data-analysis-"}[app]
}

// 注销登记无法使已缓存到线下的签名快照失效；必须先精确停止旧进程，再收编替代进程。
func (target *productionComposeTarget) retireRuntimeLicenseContainers(ctx context.Context, input application.SubsystemProvisioningInput) error {
	if len(input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
	path, err := productionRuntimeApprovalPath(target.config.DeployRoot, "runtime-license-"+target.config.Profile.Manifest.Application.Code+"-"+target.config.Profile.Manifest.Application.Environment+".json")
	if err != nil {
		return err
	}
	release, err := loadRuntimeLicenseReleaseDocument(path, target.config.Profile)
	if err != nil {
		return err
	}
	if err = target.verifyRuntimeLicenseRelease(input, release); err != nil {
		return err
	}
	if len(release.RetireServiceIDs) == 0 {
		return nil
	}
	outputRunner, ok := target.runner.(interface {
		RunOutput(context.Context, string, []string, string, ...string) ([]byte, error)
	})
	if !ok {
		return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: container inspection unavailable")
	}
	forbidden := reviewedPlatformLicenseEvidenceInfrastructure()
	// 经审核的 install/init/可观测性单元绝不属于业务退役对象，包括本项目内属于其他
	// 子系统的基础设施。
	for _, id := range []string{"temporal", "temporal-ui", "platform-key-init", "subsystem-provisioner-socket-init", "prometheus", "keycloak-backup-metrics", "contract-mysql", "contract-migrate", "project-mysql", "project-migrate", "customer-mysql", "customer-migrate", "portal-mysql", "portal-migrate", "settlement-mysql", "settlement-migrate", "settlement-catalog-sync", "data-analysis-mysql", "data-analysis-migrate", "data-analysis-metabase-init", "data-analysis-metabase"} {
		forbidden[id] = true
	}
	for _, id := range target.config.Profile.Manifest.Compose.DependencyServices {
		forbidden[id] = true
	}
	forbidden[target.config.Profile.Manifest.Compose.MigrateService] = true
	for _, id := range target.selectedRuntimeServices() {
		forbidden[id] = true
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, service := range release.RetireServiceIDs {
		prefix := runtimeLicenseRetirementPrefix(target.config.Profile.Manifest.Application.Code)
		if prefix == "" || !strings.HasPrefix(service, prefix) || forbidden[service] {
			return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: protected or selected component")
		}
		output, err := outputRunner.RunOutput(ctx, target.config.DeployRoot, nil, target.config.DockerBinary, "container", "ls", "--all", "--quiet", "--no-trunc", "--filter", "label=com.docker.compose.project="+target.config.ComposeProject, "--filter", "label=com.docker.compose.service="+service)
		if err != nil {
			return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: container enumeration failed")
		}
		if len(output) > 16384 {
			return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: container inventory too large")
		}
		listed := strings.Fields(string(output))
		if len(listed) > 128 {
			return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: container inventory too large")
		}
		for _, id := range listed {
			if !runtimeRetirementContainerID.MatchString(id) || seen[id] {
				return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: invalid container inventory")
			}
			// 只检查归属字段，绝不检查容器环境——其中可能含有上一版发布的凭据。
			inspect, err := outputRunner.RunOutput(ctx, target.config.DeployRoot, nil, target.config.DockerBinary, "container", "inspect", "--format", `{"Id":{{json .Id}},"Config":{"Labels":{{json .Config.Labels}}}}`, id)
			if err != nil {
				return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: container inspection failed")
			}
			var actual struct {
				ID     string `json:"Id"`
				Config struct {
					Labels map[string]string `json:"Labels"`
				} `json:"Config"`
			}
			if len(inspect) > 1024*1024 || json.Unmarshal(inspect, &actual) != nil || actual.ID != id || actual.Config.Labels["com.docker.compose.project"] != target.config.ComposeProject || actual.Config.Labels["com.docker.compose.service"] != service {
				return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: container scope mismatch")
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	// 在第一个破坏性进程操作之前重新校验 Agent 固有的已批准快照；
	// 不允许按前缀、服务名或未检视的 ID 停止进程。
	current, err := loadRuntimeLicenseReleaseDocument(path, target.config.Profile)
	if err != nil || !reflect.DeepEqual(release, current) {
		return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: release changed during inspection")
	}
	for _, id := range ids {
		if err := target.runner.Run(ctx, target.config.DeployRoot, nil, target.config.DockerBinary, "container", "stop", "--time", "40", id); err != nil {
			return provisioningError("LICENSE_RUNTIME_RETIREMENT_FAILED: container stop failed")
		}
	}
	return nil
}
