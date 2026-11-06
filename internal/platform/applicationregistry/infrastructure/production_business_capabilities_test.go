package infrastructure

import "testing"

// TestBusinessCapabilitiesCatalogCoversKnownApplications 验证目录覆盖六个业务的
// 已审核模式键，且应用编码合法。
// 参数：t 为当前测试上下文。
// 返回值：无；已知应用缺失声明时失败。
func TestBusinessCapabilitiesCatalogCoversKnownApplications(t *testing.T) {
	expected := map[string]int{
		integratedContractApplicationCode:   6,
		integratedCustomerApplicationCode:   2,
		integratedProjectApplicationCode:    3,
		integratedSettlementApplicationCode: 3,
		integratedPortalApplicationCode:     1,
		"data_analysis":                     3,
	}
	counts := map[string]int{}
	for _, capability := range productionBusinessCapabilities() {
		counts[capability.Application]++
	}
	for code, want := range expected {
		if counts[code] != want {
			t.Fatalf("应用 %s 声明了 %d 项能力，want %d", code, counts[code], want)
		}
	}
}

// TestDescribeCapabilitiesReportsLocalSourceAsDisabled 验证本地来源模式把对应
// 联动渲染为 disabled，模式型能力返回登记值。
// 参数：t 为当前测试上下文。
// 返回值：无；描述与现场语义不符时失败。
func TestDescribeCapabilitiesReportsLocalSourceAsDisabled(t *testing.T) {
	states := DescribeProductionBusinessCapabilities(integratedContractApplicationCode, map[string]string{
		"CONTRACT_COUNTERPARTY_SOURCE": "local",
		"CRM_REFERENCE_ENABLED":        "true",
		"CONTRACT_CATEGORY_SOURCE":     "project",
	})
	byName := map[string]BusinessCapabilityState{}
	for _, state := range states {
		byName[state.Name] = state
	}
	if byName["crm_reference"].State != "disabled" || byName["crm_reference"].Enabled {
		t.Fatalf("crm_reference = %+v, want disabled（本地交易对方来源优先于开关）", byName["crm_reference"])
	}
	if byName["project_integration"].State != "ready" || !byName["project_integration"].Enabled {
		t.Fatalf("project_integration = %+v, want ready", byName["project_integration"])
	}
	if byName["counterparty_source"].Mode != "local" || byName["counterparty_source"].State != "ready" {
		t.Fatalf("counterparty_source = %+v, want mode=local ready", byName["counterparty_source"])
	}
}

// TestDescribeCapabilitiesReportsNativeSettlement 验证结算原生+集成关闭的组合
// 描述（C04/C14 同一口径）。
// 参数：t 为当前测试上下文。
// 返回值：无；描述不符时失败。
func TestDescribeCapabilitiesReportsNativeSettlement(t *testing.T) {
	states := DescribeProductionBusinessCapabilities(integratedSettlementApplicationCode, map[string]string{
		"SETTLEMENT_NATIVE_FINANCIAL_ENABLED": "true",
		"SETTLEMENT_INTEGRATION_ENABLED":      "false",
	})
	byName := map[string]BusinessCapabilityState{}
	for _, state := range states {
		byName[state.Name] = state
	}
	if byName["native_financial"].State != "ready" {
		t.Fatalf("native_financial = %+v, want ready", byName["native_financial"])
	}
	if byName["settlement_integration"].State != "disabled" {
		t.Fatalf("settlement_integration = %+v, want disabled", byName["settlement_integration"])
	}
}

// TestDescribeCapabilitiesUnregisteredModeIsNotConfigured 验证模式值漂移被显式
// 标记为 not_configured，而不是冒充 ready。
// 参数：t 为当前测试上下文。
// 返回值：无；漂移模式未被标记时失败。
func TestDescribeCapabilitiesUnregisteredModeIsNotConfigured(t *testing.T) {
	states := DescribeProductionBusinessCapabilities(integratedCustomerApplicationCode, map[string]string{
		"CRM_WIN_CONFIRMATION_MODE": "verbal",
	})
	byName := map[string]BusinessCapabilityState{}
	for _, state := range states {
		byName[state.Name] = state
	}
	if byName["win_confirmation"].State != "not_configured" || byName["win_confirmation"].Mode != "verbal" {
		t.Fatalf("win_confirmation = %+v, want not_configured mode=verbal", byName["win_confirmation"])
	}
}

// TestDescribeCapabilitiesMissingKeysDefaultReady 验证键缺失的保守口径：开关型
// 视为开启（既有现场零破坏），模式型缺失标记 not_configured。
// 参数：t 为当前测试上下文。
// 返回值：无；口径漂移时失败。
func TestDescribeCapabilitiesMissingKeysDefaultReady(t *testing.T) {
	states := DescribeProductionBusinessCapabilities(integratedProjectApplicationCode, map[string]string{})
	byName := map[string]BusinessCapabilityState{}
	for _, state := range states {
		byName[state.Name] = state
	}
	if byName["native_project"].State != "ready" {
		t.Fatalf("native_project = %+v, want ready（键缺失保守视为开启）", byName["native_project"])
	}
	if byName["contract_integration"].State != "ready" {
		t.Fatalf("contract_integration = %+v, want ready", byName["contract_integration"])
	}
}
