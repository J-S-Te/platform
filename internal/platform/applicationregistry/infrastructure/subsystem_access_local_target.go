package infrastructure

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// 本机拓扑与统一 Compose 的既有网关一致，不代表生产审核清单或额外角色授权。
var localAccessTargets = map[string]struct{ name, upstream, path string }{
	"contract_management": {
		"合同管理", "http://contract-api:8081", "/contract_management",
	},
	"customer_and_opportunity": {
		"客户与商机管理", "http://customer-api:8090", "/customer-opportunity",
	},
	"customer_portal": {
		"客户门户", "http://portal-api:8091", "/customer-portal",
	},
	"project_management": {
		"项目管理", "http://project-api:8082", "/project_management",
	},
	"settlement": {
		"结算开票", "http://settlement-api:8085", "/settlement",
	},
	"data_analysis": {
		"数据看板", "http://dashboard-api:8080", "/data_analysis",
	},
	"reimbursement": {
		"报销管理系统", "http://reimbursement-api:8087", "/reimbursement",
	},
}

// subsystemAccessTarget 按执行模式解析精确目标，本机只接受执行器已确认可用的拓扑。
// 参数：capabilities 为可信执行器的能力，operation 为当前授权目标。
// 返回值：返回目标及命中标记；生产和未知模式不回退到本机拓扑。
func subsystemAccessTarget(capabilities application.SubsystemProvisioningCapabilities,
	operation application.SubsystemAccessOperation,
) (application.SubsystemProvisioningTarget, bool) {
	for _, target := range capabilities.Targets {
		if strings.EqualFold(strings.TrimSpace(target.ApplicationCode), strings.TrimSpace(operation.ApplicationCode)) &&
			strings.EqualFold(strings.TrimSpace(target.Environment), strings.TrimSpace(operation.EnvironmentCode)) {
			return target, true
		}
	}
	code := strings.ToLower(strings.TrimSpace(operation.ApplicationCode))
	environment := strings.ToLower(strings.TrimSpace(operation.EnvironmentCode))
	if !strings.EqualFold(strings.TrimSpace(capabilities.Mode), "local") || !capabilities.Enabled ||
		!containsCapability(capabilities.SupportedApplicationCodes, code) ||
		!containsCapability(capabilities.SupportedEnvironments, environment) {
		return application.SubsystemProvisioningTarget{}, false
	}
	definition, found := localAccessTargets[code]
	if !found {
		return application.SubsystemProvisioningTarget{}, false
	}
	target := application.SubsystemProvisioningTarget{
		ApplicationCode: code, Environment: environment, ApplicationName: definition.name,
		UpstreamURL: definition.upstream, PathPrefix: definition.path, ClientType: "confidential",
	}
	// 绑定实际本机配置，不把空生产清单摘要当作部署成功证据。
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"local-subsystem-target/v1", code, environment, definition.name,
		definition.upstream, definition.path, target.ClientType,
	}, "\n")))
	target.ManifestChecksum = hex.EncodeToString(digest[:])
	return target, true
}
