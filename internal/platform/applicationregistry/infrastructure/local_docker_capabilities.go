package infrastructure

import (
	"context"
	"os"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// AvailableCapabilities 不调用Docker，只读投影本机安装可用性及明确结算来源模式。
// 参数：ctx控制文件读取；运行凭据不向调用者输出。
// 返回值：可用应用及精确原生结算环境，配置错误失败关闭，守护进程仍由Preflight核验。
func (provisioner *LocalDockerSubsystemProvisioner) AvailableCapabilities(ctx context.Context) (application.SubsystemProvisioningCapabilities, error) {
	capabilities := application.SubsystemProvisioningCapabilities{
		Mode: "local", SupportedApplicationCodes: []string{}, Targets: []application.SubsystemProvisioningTarget{},
		SupportedEnvironments: []string{"dev", "test", "staging", "prod"},
		DefaultEnvironment:    "dev", DefaultClientType: "confidential",
	}
	if err := ctx.Err(); err != nil {
		return application.SubsystemProvisioningCapabilities{}, err
	}
	if provisioner == nil || !provisioner.config.Enabled {
		return capabilities, nil
	}
	capabilities.Enabled = true
	if info, err := os.Stat(provisioner.config.GatewayScriptPath); err != nil || !info.Mode().IsRegular() || strings.TrimSpace(provisioner.config.GatewayIncludePath) == "" {
		return capabilities, nil
	}
	for _, code := range []string{integratedContractApplicationCode, integratedCustomerApplicationCode, integratedPortalApplicationCode, integratedProjectApplicationCode, integratedSettlementApplicationCode, "data_analysis"} {
		if err := ctx.Err(); err != nil {
			return application.SubsystemProvisioningCapabilities{}, err
		}
		directory, err := provisioner.projectDirectory(code)
		if err != nil {
			continue
		}
		if isIntegratedSubsystem(code) {
			if _, _, _, _, _, _, _, _, err := provisioner.integratedComposeConfiguration(); err != nil {
				continue
			}
		} else if _, err := locateComposeFile(directory); err != nil {
			continue
		}
		environmentSource, _, err := provisioner.subsystemEnvironmentPaths(code, directory)
		if err != nil {
			continue
		}
		capabilities.SupportedApplicationCodes = append(capabilities.SupportedApplicationCodes, code)
		if code == integratedSettlementApplicationCode {
			current, err := readSettlementSourceValues(environmentSource)
			if err != nil {
				return application.SubsystemProvisioningCapabilities{}, err
			}
			optional, err := nativeSettlementContractOptional(current)
			if err != nil {
				return application.SubsystemProvisioningCapabilities{}, err
			}
			if optional {
				environment := localRuntimeEnvironment(environmentSource, code, "OIDC_CLIENT_ID")
				if environment == "" {
					environment = "dev"
				}
				capabilities.SettlementContractOptionalEnvironments = append(capabilities.SettlementContractOptionalEnvironments, environment)
			}
		}
	}
	if err := application.ValidateRelatedServiceCapabilityProjection(capabilities); err != nil {
		return application.SubsystemProvisioningCapabilities{}, provisioningError("Settlement source capability is invalid")
	}
	return capabilities, nil
}
