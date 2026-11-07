package infrastructure

import (
	"os"
	"path/filepath"
	"testing"
)

// writePeerRuntime 写入一个"接入就绪"的对端运行文件（目录同步启用+应用/环境可用），
// 供解析器读取；返回部署根路径。
// 参数：t 为当前测试上下文，apps 为视为已就绪的应用编码集合。
// 返回值：部署根目录。
func writePeerRuntime(t *testing.T, readyApps ...string) string {
	t.Helper()
	root := t.TempDir()
	ready := map[string]bool{}
	for _, app := range readyApps {
		ready[app] = true
	}
	for app, rel := range contractSourcePeerRuntimeFiles {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if !ready[app] {
			continue
		}
		body := "PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED=true\n" +
			"PLATFORM_APPLICATION_CODE=" + app + "\n" +
			"PLATFORM_ENVIRONMENT_CODE=prod\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestSourceResolutionStandaloneContractIsLocal 验证平台+合同（对端均未接入）自动
// 解析为独立来源，且联动开关连带关闭（C01 开张缺口）。
// 参数：t 为当前测试上下文。
// 返回值：无；独立形态仍默认 crm/project 时失败。
func TestSourceResolutionStandaloneContractIsLocal(t *testing.T) {
	root := writePeerRuntime(t)
	current := map[string]string{}
	values := map[string]string{}
	resolveContractSourceAutoDefaults(root, current, values)
	if values["CONTRACT_COUNTERPARTY_SOURCE"] != "local" || values["CONTRACT_CATEGORY_SOURCE"] != "local" {
		t.Fatalf("独立合同来源解析错误: %v", values)
	}
	if values["CRM_REFERENCE_ENABLED"] != "false" || values["PROJECT_INTEGRATION_ENABLED"] != "false" || values["PROJECT_CATEGORY_DIRECTORY_ENABLED"] != "false" {
		t.Fatalf("联动开关未随独立来源关闭: %v", values)
	}
}

// TestSourceResolutionCombinedInstallFollowsPeers 验证对端接入就绪时解析为组合来源。
// 参数：t 为当前测试上下文。
// 返回值：无；组合形态被误判为独立时失败。
func TestSourceResolutionCombinedInstallFollowsPeers(t *testing.T) {
	root := writePeerRuntime(t, integratedCustomerApplicationCode, integratedProjectApplicationCode)
	values := map[string]string{}
	resolveContractSourceAutoDefaults(root, map[string]string{}, values)
	if values["CONTRACT_COUNTERPARTY_SOURCE"] != "crm" || values["CONTRACT_CATEGORY_SOURCE"] != "project" {
		t.Fatalf("组合合同来源解析错误: %v", values)
	}
	if values["CRM_REFERENCE_ENABLED"] != "true" || values["PROJECT_INTEGRATION_ENABLED"] != "true" {
		t.Fatalf("联动开关未随组合来源开启: %v", values)
	}
}

// TestSourceResolutionMixedPeers 验证混合形态（CRM 已接入、项目未接入）得到
// crm + local 的独立解析，且类别目录开关跟随关闭。
// 参数：t 为当前测试上下文。
// 返回值：无；混合解析错误时失败。
func TestSourceResolutionMixedPeers(t *testing.T) {
	root := writePeerRuntime(t, integratedCustomerApplicationCode)
	values := map[string]string{}
	resolveContractSourceAutoDefaults(root, map[string]string{}, values)
	if values["CONTRACT_COUNTERPARTY_SOURCE"] != "crm" || values["CONTRACT_CATEGORY_SOURCE"] != "local" {
		t.Fatalf("混合来源解析错误: %v", values)
	}
	if values["PROJECT_INTEGRATION_ENABLED"] != "false" {
		t.Fatalf("类别联动未随 local 关闭: %v", values)
	}
}

// TestSourceResolutionExplicitValuesPreserved 验证运维显式值（含显式 crm/project）
// 一律保留，连带开关也不被覆盖。
// 参数：t 为当前测试上下文。
// 返回值：无；显式值被覆盖时失败。
func TestSourceResolutionExplicitValuesPreserved(t *testing.T) {
	root := writePeerRuntime(t)
	current := map[string]string{
		"CONTRACT_COUNTERPARTY_SOURCE": "local",
		"CONTRACT_CATEGORY_SOURCE":     "project",
		"CRM_REFERENCE_ENABLED":        "false",
		"PROJECT_INTEGRATION_ENABLED":  "true",
	}
	values := map[string]string{}
	resolveContractSourceAutoDefaults(root, current, values)
	if _, ok := values["CONTRACT_COUNTERPARTY_SOURCE"]; ok {
		t.Fatal("显式交易对方来源被覆盖")
	}
	if _, ok := values["CONTRACT_CATEGORY_SOURCE"]; ok {
		t.Fatal("显式类别来源被覆盖")
	}
	if _, ok := values["CRM_REFERENCE_ENABLED"]; ok {
		t.Fatal("显式 CRM 开关被覆盖")
	}
	if _, ok := values["PROJECT_INTEGRATION_ENABLED"]; ok {
		t.Fatal("显式项目开关被覆盖")
	}
}

// TestSourceResolutionExplicitFlagNotTouched 验证来源缺失但联动开关已显式设置时：
// 解析来源但不改写该开关（运维显式开关保留）。
// 参数：t 为当前测试上下文。
// 返回值：无；显式开关被改写时失败。
func TestSourceResolutionExplicitFlagNotTouched(t *testing.T) {
	root := writePeerRuntime(t)
	current := map[string]string{"CRM_REFERENCE_ENABLED": "true"}
	values := map[string]string{}
	resolveContractSourceAutoDefaults(root, current, values)
	if values["CONTRACT_COUNTERPARTY_SOURCE"] != "local" {
		t.Fatalf("独立来源未解析: %v", values)
	}
	if values["CRM_REFERENCE_ENABLED"] != "" {
		t.Fatalf("现场显式 CRM 开关被改写: %v", values)
	}
}

// TestSourceResolutionSettlementOutboundCornerSkipped 验证已知角落：结算出站被
// 显式开启而 CRM 未就绪时，不代运维挑选来源（键保持缺失，回落合同既有默认）。
// 参数：t 为当前测试上下文。
// 返回值：无；角落被静默挑选时失败。
func TestSourceResolutionSettlementOutboundCornerSkipped(t *testing.T) {
	root := writePeerRuntime(t)
	current := map[string]string{"SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED": "true"}
	values := map[string]string{}
	resolveContractSourceAutoDefaults(root, current, values)
	if _, ok := values["CONTRACT_COUNTERPARTY_SOURCE"]; ok {
		t.Fatal("结算出站显式开启与 CRM 未就绪的矛盾被静默裁决")
	}
	if values["CONTRACT_CATEGORY_SOURCE"] != "local" {
		t.Fatalf("类别来源解析应不受该角落影响: %v", values)
	}
}
