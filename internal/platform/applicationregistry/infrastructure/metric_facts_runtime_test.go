package infrastructure

import (
	"path/filepath"
	"testing"
)

// TestMetricFactsRuntimePreservesExplicitSelection 验证受控更新不静默覆盖运维明确的关闭选择。
// 参数：t为仅临时元数据测试的上下文。
// 返回值：无；关闭或空值被默认启用时失败。
func TestMetricFactsRuntimePreservesExplicitSelection(t *testing.T) {
	for _, value := range []string{"false", ""} {
		values := map[string]string{"METRIC_FACTS_SYNC_ENABLED": "true"}
		preserveDataAnalysisDashboardSources("data_analysis",
			map[string]string{"METRIC_FACTS_SYNC_ENABLED": value}, values)
		if values["METRIC_FACTS_SYNC_ENABLED"] != value {
			t.Fatal("explicit metric facts selection overwritten")
		}
	}
}

// TestMetricFactsProductionDefaultDelivered 验证正式清单将显式指标开关交付运行文件。
// 参数：t为只使用临时配置及合成身份的测试上下文。
// 返回值：无；新增来源开关漏接或更新后关闭选择丢失时失败。
func TestMetricFactsProductionDefaultDelivered(t *testing.T) {
	target, input := settlementDashboardProductionFixture(t, "data_analysis")
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/data-analysis.env")
	values, err := readEnvironmentValues(path)
	if err != nil || values["METRIC_FACTS_SYNC_ENABLED"] != "true" {
		t.Fatalf("flag missing: %v", err)
	}
	if err := updateProductionSubsystemEnvironment(path, map[string]string{"METRIC_FACTS_SYNC_ENABLED": "false"}); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	values, err = readEnvironmentValues(path)
	if err != nil || values["METRIC_FACTS_SYNC_ENABLED"] != "false" {
		t.Fatalf("flag replaced: %v", err)
	}
}
