package infrastructure

import (
	"slices"

	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

// 部署运行时与许可消费者是两份不同清点。只有受审的上游 BI 引擎属于代理管制的
// 基础设施；它仍需要不可变的安装证据，且绝不能成为排除项。
func runtimeLicenseConsumerServices(profile productionSubsystemProfile) ([]string, error) {
	result := []string{}
	for _, name := range profile.Manifest.Compose.RuntimeServices {
		if licenseevidence.ReviewedControlledBIInfrastructure(profile.Manifest.Application.Code, name) {
			_, conditionalEngine := profile.Manifest.Compose.ConditionalRuntimeServices[name]
			_, conditionalGateway := profile.Manifest.Compose.ConditionalRuntimeServices["data-analysis-api"]
			if !slices.Contains(profile.Manifest.Compose.RuntimeServices, "data-analysis-api") || conditionalEngine || conditionalGateway {
				return nil, provisioningError("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: BI gateway boundary missing")
			}
			continue
		}
		result = append(result, name)
	}
	return result, nil
}

func (target *productionComposeTarget) selectedRuntimeLicenseConsumers() []string {
	result := []string{}
	for _, name := range target.selectedRuntimeServices() {
		if !licenseevidence.ReviewedControlledBIInfrastructure(target.config.Profile.Manifest.Application.Code, name) {
			result = append(result, name)
		}
	}
	return result
}
