package infrastructure

import "testing"

// contractLinkageValues 构造合同应用全部业务对端 URL 的写入集（模拟清单值）。
// 参数：无。
// 返回值：URL 键到对端端点的映射。
func contractLinkageValues() map[string]string {
	return map[string]string{
		"CRM_REFERENCE_BASE_URL":             "http://customer-api:8090/customer-opportunity",
		"CONTRACT_IMPORT_PROJECT_BASE_URL":   "http://project-api:8082",
		"CONTRACT_IMPORT_PROJECT_TOKEN_URL":  "http://platform-api:8080/oauth2/token",
		"CONTRACT_IMPORT_FINANCE_BASE_URL":   "http://settlement-api:8085",
		"CONTRACT_IMPORT_FINANCE_TOKEN_URL":  "http://platform-api:8080/oauth2/token",
		"SETTLEMENT_API_BASE_URL":            "http://settlement-api:8085",
		"SETTLEMENT_INTEGRATION_TOKEN_URL":   "http://platform-api:8080/oauth2/token",
		"CONTRACT_IMPORT_REPORT_BASE_URL":    "http://data-analysis-api:8080",
		"CONTRACT_IMPORT_REPORT_TOKEN_URL":   "http://platform-api:8080/oauth2/token",
		"FILE_GATEWAY_BASE_URL":              "http://file-gateway:8086",
		"PLATFORM_AUTHORIZATION_CONTEXT_URL": "authorization_context_url",
	}
}

// TestLinkageExplicitOffDropsPeerURLs 验证联动开关显式 false 时对端业务 URL 从
// 写入集移除，平台自身端点不受影响（C07）。
// 参数：t 为当前测试上下文。
// 返回值：无；URL 残留或平台端点被误删时失败。
func TestLinkageExplicitOffDropsPeerURLs(t *testing.T) {
	current := map[string]string{
		"CRM_REFERENCE_ENABLED":                   "false",
		"PROJECT_INTEGRATION_ENABLED":             "false",
		"SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED": "false",
		"CONTRACT_IMPORT_HISTORY_ENABLED":         "false",
	}
	values := contractLinkageValues()
	if err := applyProductionLinkageURLValues(integratedContractApplicationCode, current, values); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"CRM_REFERENCE_BASE_URL",
		"CONTRACT_IMPORT_PROJECT_BASE_URL", "CONTRACT_IMPORT_PROJECT_TOKEN_URL",
		"CONTRACT_IMPORT_FINANCE_BASE_URL", "CONTRACT_IMPORT_FINANCE_TOKEN_URL",
		"SETTLEMENT_API_BASE_URL", "SETTLEMENT_INTEGRATION_TOKEN_URL",
		"CONTRACT_IMPORT_REPORT_BASE_URL", "CONTRACT_IMPORT_REPORT_TOKEN_URL",
	} {
		if _, ok := values[key]; ok {
			t.Fatalf("联动关闭后 %s 仍被注入", key)
		}
	}
	if values["FILE_GATEWAY_BASE_URL"] == "" || values["PLATFORM_AUTHORIZATION_CONTEXT_URL"] == "" {
		t.Fatal("平台自身端点被误删")
	}
}

// TestLocalCounterpartySourceDropsCRMURL 验证本地来源模式（无需显式 false）同样
// 关闭对应联动并移除对端 URL。
// 参数：t 为当前测试上下文。
// 返回值：无；本地模式下 CRM URL 仍被注入时失败。
func TestLocalCounterpartySourceDropsCRMURL(t *testing.T) {
	current := map[string]string{
		"CONTRACT_COUNTERPARTY_SOURCE": "local",
		"CRM_REFERENCE_ENABLED":        "true",
	}
	values := contractLinkageValues()
	if err := applyProductionLinkageURLValues(integratedContractApplicationCode, current, values); err != nil {
		t.Fatal(err)
	}
	if _, ok := values["CRM_REFERENCE_BASE_URL"]; ok {
		t.Fatal("本地交易对方来源下 CRM 对端 URL 仍被注入")
	}
	if values["CONTRACT_IMPORT_PROJECT_BASE_URL"] == "" {
		t.Fatal("不相关的项目联动 URL 被误删")
	}
}

// TestLinkageExplicitOnRequiresUsableURL 验证联动显式开启时端点缺失或占位值
// 失败关闭。
// 参数：t 为当前测试上下文。
// 返回值：无；占位端点未拦截时失败。
func TestLinkageExplicitOnRequiresUsableURL(t *testing.T) {
	current := map[string]string{"CRM_REFERENCE_ENABLED": "true"}
	values := map[string]string{"CRM_REFERENCE_BASE_URL": "REPLACE_WITH_CRM_REFERENCE_BASE_URL"}
	if err := applyProductionLinkageURLValues(integratedContractApplicationCode, current, values); err == nil {
		t.Fatal("联动开启但端点为占位值未被拒绝")
	}
	values = map[string]string{"CRM_REFERENCE_BASE_URL": "http://customer-api:8090/customer-opportunity"}
	if err := applyProductionLinkageURLValues(integratedContractApplicationCode, current, values); err != nil {
		t.Fatal(err)
	}
}

// TestLinkageOnFallsBackToLiveValue 验证写入集缺失时以现场值兜底判定可用性。
// 参数：t 为当前测试上下文。
// 返回值：无；现场可用值被忽略导致误拒时失败。
func TestLinkageOnFallsBackToLiveValue(t *testing.T) {
	current := map[string]string{
		"CRM_REFERENCE_ENABLED":  "true",
		"CRM_REFERENCE_BASE_URL": "http://customer-api:8090/customer-opportunity",
	}
	values := map[string]string{}
	if err := applyProductionLinkageURLValues(integratedContractApplicationCode, current, values); err != nil {
		t.Fatalf("现场可用端点未兜底: %v", err)
	}
}

// TestMissingModeKeysKeepLegacyValues 验证模式键缺失时保持既有行为：URL 照常
// 注入且不做可用性强制（既有现场升级零破坏）。
// 参数：t 为当前测试上下文。
// 返回值：无；缺键现场被误改时失败。
func TestMissingModeKeysKeepLegacyValues(t *testing.T) {
	values := contractLinkageValues()
	if err := applyProductionLinkageURLValues(integratedContractApplicationCode, map[string]string{}, values); err != nil {
		t.Fatal(err)
	}
	if values["CRM_REFERENCE_BASE_URL"] == "" {
		t.Fatal("模式键缺失时既有 URL 被误删")
	}
}

// TestProjectApprovalValidationLinkage 验证项目审批联动开关独立生效。
// 参数：t 为当前测试上下文。
// 返回值：无；项目联动判定失效时失败。
func TestProjectApprovalValidationLinkage(t *testing.T) {
	values := map[string]string{
		"CONTRACT_APPROVAL_VALIDATION_URL": "http://contract-api:8081/internal/v1/project/approved-contracts",
		"PLATFORM_OWNER_DIRECTORY_URL":     "http://platform-api:8080/api/v1/internal/owner-directory",
	}
	current := map[string]string{"CONTRACT_APPROVAL_VALIDATION_ENABLED": "false"}
	if err := applyProductionLinkageURLValues(integratedProjectApplicationCode, current, values); err != nil {
		t.Fatal(err)
	}
	if _, ok := values["CONTRACT_APPROVAL_VALIDATION_URL"]; ok {
		t.Fatal("审批联动关闭后合同对端 URL 仍被注入")
	}
	if values["PLATFORM_OWNER_DIRECTORY_URL"] == "" {
		t.Fatal("平台自身端点被误删")
	}
}

// TestUnknownApplicationKeepsValues 验证未声明 URL 组的应用（门户/CRM/数析等）
// 不受影响。
// 参数：t 为当前测试上下文。
// 返回值：无；无组应用被误改时失败。
func TestUnknownApplicationKeepsValues(t *testing.T) {
	values := map[string]string{"SOME_URL": "http://example.internal"}
	if err := applyProductionLinkageURLValues(integratedPortalApplicationCode, map[string]string{}, values); err != nil {
		t.Fatal(err)
	}
	if values["SOME_URL"] == "" {
		t.Fatal("无 URL 组的应用被误改")
	}
}
