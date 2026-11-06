package infrastructure

import (
	"strconv"
	"strings"
)

// preserveLocalBusinessIntegrationFlags 保留已明确选择独立来源后的业务联动关闭决定。
// 参数：code为目标应用，modePath为来源选择文件，currentPath为旗标文件，values为自动更新值。
// 返回值：读取失败时返回错误；只保护白名单且不改变通用显式配置写入行为。
func preserveLocalBusinessIntegrationFlags(code, modePath, currentPath string, values map[string]string) error {
	modes, err := readEnvironmentValues(modePath)
	if err != nil {
		return err
	}
	keys := localBusinessModeFlagKeys(code, modes)
	if len(keys) == 0 {
		return nil
	}
	current := modes
	if currentPath != modePath {
		current, err = readEnvironmentValues(currentPath)
		if err != nil {
			return err
		}
	}
	retainedProjectIntegration := false
	for _, key := range keys {
		if explicitLocalRuntimeBool(current[key], false) && explicitLocalRuntimeBool(values[key], true) {
			values[key] = current[key]
			retainedProjectIntegration = retainedProjectIntegration || key == "CONTRACT_INTEGRATION_ENABLED"
		}
	}
	// 项目配置拒绝“入口关闭但RequireBearer开启”；只保留同次关闭入口的原合法成对选择。
	if code == integratedProjectApplicationCode && retainedProjectIntegration &&
		explicitLocalRuntimeBool(current["CONTRACT_INTEGRATION_REQUIRE_BEARER"], false) &&
		explicitLocalRuntimeBool(values["CONTRACT_INTEGRATION_REQUIRE_BEARER"], true) {
		values["CONTRACT_INTEGRATION_REQUIRE_BEARER"] = current["CONTRACT_INTEGRATION_REQUIRE_BEARER"]
	}
	return nil
}

// explicitLocalRuntimeBool 区分明确布尔选择与缺失、空值或非法配置。
// 参数：raw为已有或自动生成配置，expected为本次需要判断的语义值。
// 返回值：合法且与期望一致时返回true，不把空值当成显式false。
func explicitLocalRuntimeBool(raw string, expected bool) bool {
	value, err := strconv.ParseBool(raw)
	return err == nil && value == expected
}

// localBusinessModeFlagKeys 为每种明确独立来源返回最小业务开关白名单。
// 参数：code为应用，modes为运维已有来源配置，不从凭据或URL猜测来源。
// 返回值：允许保留明确false的业务键；身份、审计、许可、同源与一般目录开关不在其中。
func localBusinessModeFlagKeys(code string, modes map[string]string) []string {
	switch code {
	case integratedContractApplicationCode:
		if modes["CONTRACT_COUNTERPARTY_SOURCE"] == "local" || modes["CONTRACT_CATEGORY_SOURCE"] == "local" {
			return []string{"CRM_REFERENCE_ENABLED", "PROJECT_INTEGRATION_ENABLED",
				"SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED", "CRM_LINK_ENABLED"}
		}
	case integratedCustomerApplicationCode:
		if modes["CRM_WIN_CONFIRMATION_MODE"] == "local_evidence" {
			return []string{"CONTRACT_VERIFICATION_ENABLED", "CONTRACT_SIGNED_COUNT_ENABLED"}
		}
	case integratedProjectApplicationCode:
		if explicitLocalRuntimeBool(modes["PROJECT_NATIVE_ENABLED"], true) {
			return []string{"CONTRACT_INTEGRATION_ENABLED", "CONTRACT_APPROVAL_VALIDATION_ENABLED",
				"PROJECT_CONTRACT_IMPORT_ENABLED", "SETTLEMENT_REFERENCE_ENABLED"}
		}
	case integratedSettlementApplicationCode:
		if explicitLocalRuntimeBool(modes["SETTLEMENT_NATIVE_FINANCIAL_ENABLED"], true) {
			return []string{"SETTLEMENT_INTEGRATION_ENABLED", "SETTLEMENT_CONTRACT_IMPORT_ENABLED",
				"SETTLEMENT_CRM_CREDIT_SYNC_ENABLED"}
		}
	case integratedPortalApplicationCode:
		if modes["PORTAL_CUSTOMER_DIRECTORY_MODE"] == "local" {
			return []string{"PORTAL_INVITE_ENABLED"}
		}
	}
	return nil
}

// explicitBusinessModeKeys 是各应用在受控更新中必须保留运维明确选择的联动开关。
// 与 localBusinessModeFlagKeys 的差异：这里用于正式环境更新，已有运行文件里的显式
// 布尔值代表运维已经做出的来源/联动决定，不能在下发清单默认值时被静默覆盖。
var explicitBusinessModeKeys = map[string][]string{
	integratedContractApplicationCode: {
		"CRM_REFERENCE_ENABLED", "PROJECT_INTEGRATION_ENABLED",
		"SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED", "CRM_LINK_ENABLED",
	},
	integratedCustomerApplicationCode: {
		"CONTRACT_VERIFICATION_ENABLED", "CONTRACT_SIGNED_COUNT_ENABLED",
	},
	integratedProjectApplicationCode: {
		"CONTRACT_INTEGRATION_ENABLED", "CONTRACT_APPROVAL_VALIDATION_ENABLED",
		"CONTRACT_INTEGRATION_REQUIRE_BEARER", "PROJECT_CONTRACT_IMPORT_ENABLED",
		"SETTLEMENT_REFERENCE_ENABLED",
	},
	integratedSettlementApplicationCode: {
		"SETTLEMENT_INTEGRATION_ENABLED", "SETTLEMENT_CONTRACT_IMPORT_ENABLED",
		"SETTLEMENT_CRM_CREDIT_SYNC_ENABLED",
	},
	integratedPortalApplicationCode: {"PORTAL_INVITE_ENABLED"},
}

// preserveExplicitBusinessModeValues 保留正式运行文件中已明确写入的联动布尔选择。
// 参数：code为应用，current为当前运行文件，values为本次受控更新准备写入的键值。
// 返回值：无；仅覆盖 values 中同时存在、且 current 为合法布尔值的联动键。
func preserveExplicitBusinessModeValues(code string, current, values map[string]string) {
	for _, key := range explicitBusinessModeKeys[code] {
		raw, exists := current[key]
		if !exists {
			continue
		}
		if _, err := strconv.ParseBool(strings.TrimSpace(raw)); err != nil {
			continue
		}
		if _, exists := values[key]; !exists {
			continue
		}
		values[key] = raw
	}
}
