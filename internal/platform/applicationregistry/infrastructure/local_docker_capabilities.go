package infrastructure

import (
	"context"
	"os"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// AvailableCapabilities exposes local installation availability without reading runtime
// credentials or invoking Docker. Preflight remains responsible for daemon connectivity
// and deployment validation; a local workspace is not a prepared production release.
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
		if _, _, err := provisioner.subsystemEnvironmentPaths(code, directory); err != nil {
			continue
		}
		capabilities.SupportedApplicationCodes = append(capabilities.SupportedApplicationCodes, code)
	}
	return capabilities, nil
}
