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
			if err := target.writeRuntimeFixedValues(application.SubsystemProvisioningInput{ApplicationCode: item.code}); err != nil {
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
