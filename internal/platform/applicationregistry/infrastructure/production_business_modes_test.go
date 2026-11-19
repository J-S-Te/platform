package infrastructure

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestProductionBusinessModesSurviveControlledUpdate 核验明确选择的业务联动不会被更新静默覆盖。
// 参数：t为测试上下文，所有文件和身份均为临时合成配置。
// 返回值：无；受控更新覆盖产品选择时失败。
func TestProductionBusinessModesSurviveControlledUpdate(t *testing.T) {
	for _, item := range []struct {
		code, profile, runtime string
		values                 map[string]string
	}{
		{"contract_management", "contract_management-prod.yaml", "contract.env", map[string]string{
			"CRM_REFERENCE_ENABLED": "false", "PROJECT_INTEGRATION_ENABLED": "false",
			"SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED": "false", "CRM_LINK_ENABLED": "false",
		}},
		{"customer_and_opportunity", "customer_and_opportunity-prod.yaml", "customer.env", map[string]string{
			"CONTRACT_VERIFICATION_ENABLED": "false", "CONTRACT_SIGNED_COUNT_ENABLED": "false",
		}},
		{"settlement", "settlement-prod.yaml", "settlement.env", map[string]string{
			"SETTLEMENT_CRM_CREDIT_SYNC_ENABLED": "true",
		}},
		{"project_management", "project_management-prod.yaml", "project.env", map[string]string{
			"CONTRACT_APPROVAL_VALIDATION_ENABLED": "false", "CONTRACT_INTEGRATION_ENABLED": "false",
			"CONTRACT_INTEGRATION_REQUIRE_BEARER": "false",
		}},
	} {
		t.Run(item.code, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			profile, err := decodeProductionSubsystemProfile(platformModuleRoot(t),
				filepath.Join(platformModuleRoot(t), "deploy/production/subsystems.d", item.profile))
			if err != nil {
				t.Fatal(err)
			}
			file := &profile.Manifest.Runtime.Files[0]
			selected := map[string]string{}
			for key := range item.values {
				if value, exists := file.Values[key]; exists {
					selected[key] = value
				}
			}
			file.Values, file.Bindings, file.GeneratedKeys = selected, nil, nil
			if err := os.Mkdir(filepath.Join(root, "runtime"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "runtime", item.runtime)
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := updateProductionSubsystemEnvironment(path, item.values); err != nil {
				t.Fatal(err)
			}
			target := &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}}
			input := application.SubsystemProvisioningInput{
				ApplicationCode: item.code,
				Environment:     profile.Manifest.Application.Environment,
			}
			if item.code == integratedCustomerApplicationCode {
				prepareCRMProjectHistoryBusinessModeRuntime(t, target, input)
			}
			if err := target.writeRuntimeFixedValues(input); err != nil {
				t.Fatal(err)
			}
			actual, err := readEnvironmentValues(path)
			if err != nil {
				t.Fatal(err)
			}
			for key, expected := range item.values {
				if actual[key] != expected {
					t.Errorf("controlled update overwrote %s", key)
				}
			}
		})
	}
}

// prepareCRMProjectHistoryBusinessModeRuntime 用正式模板模拟 CRM 受控更新前的扩展初始化。
// 参数：t 为测试上下文，target 指向临时部署目录，input 为测试目标的应用和环境。
// 返回值：无；缺少清单、模板或初始化失败时中止当前测试。
func prepareCRMProjectHistoryBusinessModeRuntime(t *testing.T, target *productionComposeTarget, input application.SubsystemProvisioningInput) {
	t.Helper()
	for _, file := range target.config.Profile.Manifest.Runtime.Files {
		if file.Path != productionCRMProjectHistoryRuntimePath {
			continue
		}
		content, err := os.ReadFile(filepath.Join(platformModuleRoot(t), "deploy/production", file.TemplatePath))
		if err != nil {
			t.Fatal(err)
		}
		template := filepath.Join(target.config.DeployRoot, file.TemplatePath)
		if err := os.MkdirAll(filepath.Dir(template), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(template, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := target.initializeInheritedRuntimeExtensions(input); err != nil {
			t.Fatalf("initialize CRM project history runtime: %v", err)
		}
		return
	}
	t.Fatal("CRM project history runtime file missing from reviewed profile")
}
