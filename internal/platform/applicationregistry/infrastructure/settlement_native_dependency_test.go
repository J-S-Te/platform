package infrastructure

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestNativeSettlementProductionBindingsAllowExplicitStandalone 验证生产正式写入允许显式独售无合同。
// 参数：t为测试上下文，仅使用已有临时profile夹具。
// 返回值：无；默认模式或已登记漂移仍必须拒绝。
func TestNativeSettlementProductionBindingsAllowExplicitStandalone(t *testing.T) {
	for _, mode := range []string{"standalone", "default", "integration_enabled", "drifted", "blank_source_identity"} {
		t.Run(mode, func(t *testing.T) {
			target, input := settlementRoleRuntimeFixture(t, "settlement")
			path := filepath.Join(target.config.DeployRoot, "runtime/settlement.env")
			flags := map[string]string{"SETTLEMENT_NATIVE_FINANCIAL_ENABLED": "true", "SETTLEMENT_INTEGRATION_ENABLED": "false"}
			if mode == "default" {
				flags["SETTLEMENT_NATIVE_FINANCIAL_ENABLED"] = "false"
			}
			if mode == "integration_enabled" {
				flags["SETTLEMENT_INTEGRATION_ENABLED"] = "true"
			}
			if err := updateProductionSubsystemEnvironment(path, flags); err != nil {
				t.Fatal(err)
			}
			if mode == "drifted" {
				input.RelatedServiceClients[0].Client.Scopes = []string{"*"}
			} else if mode == "blank_source_identity" {
				input.RelatedServiceClients[0].ApplicationID = ""
				input.RelatedServiceClients[0].Client.ApplicationID = ""
			} else {
				input.RelatedServiceClients = nil
			}
			err := target.writeRuntimeConfiguration(input)
			if mode != "standalone" {
				if err == nil {
					t.Fatal("missing or drifted default source accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("explicit native-only configuration still requires contract: %v", err)
			}
			values, err := readEnvironmentValues(path)
			if err != nil {
				t.Fatal(err)
			}
			if values["SETTLEMENT_INTEGRATION_ENABLED"] != "false" {
				t.Fatal("native contract ingest reopened")
			}
			for _, key := range []string{"CLIENT_ID", "OAUTH_CLIENT_ID", "APPLICATION_ID", "ENVIRONMENT_ID"} {
				if values["CONTRACT_INTEGRATION_"+key] != "" {
					t.Fatal("missing contract produced a placeholder identity")
				}
			}
		})
	}
}

// TestNativeSettlementPolicyRejectsUnsafeRuntimeFile 验证独立模式放宽前必须保护运行文件权限。
// 参数：t为测试上下文，临时文件可安全调整权限。
// 返回值：无；group/world访问权限不得产生可选策略，旧缺省不扩大校验。
func TestNativeSettlementPolicyRejectsUnsafeRuntimeFile(t *testing.T) {
	path := localModeRuntimeFile(t, "SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true\nSETTLEMENT_INTEGRATION_ENABLED=false\n")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSettlementSourceValues(path); err == nil {
		t.Fatal("unsafe native runtime created optional policy")
	}
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{ApplicationCode: "settlement", Environment: "dev"})
	input.RelatedServiceClients = nil
	if err := (&LocalDockerSubsystemProvisioner{}).p1LocalRuntimeValues(input, path, map[string]string{}, false); err == nil {
		t.Fatal("local binding bypassed runtime permission requirement")
	}
	if err := os.WriteFile(path, []byte("SETTLEMENT_INTEGRATION_ENABLED=true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSettlementSourceValues(path); err != nil {
		t.Fatal("legacy mode permissions were newly restricted")
	}
}

// TestNativeSettlementLocalBindingsAllowExplicitStandalone 验证本地正式值生成不先强制解析缺失合同。
// 参数：t为测试上下文，仅写临时本地运行文件。
// 返回值：无；缺省保留旧严格依赖，显式关闭时不造身份。
func TestNativeSettlementLocalBindingsAllowExplicitStandalone(t *testing.T) {
	for _, local := range []bool{false, true} {
		content := "SETTLEMENT_INTEGRATION_ENABLED=false\n"
		if local {
			content += "SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true\n"
		}
		path := localModeRuntimeFile(t, content)
		input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{ApplicationCode: "settlement", Environment: "dev"})
		input.RelatedServiceClients = nil
		values := map[string]string{}
		err := (&LocalDockerSubsystemProvisioner{}).p1LocalRuntimeValues(input, path, values, false)
		if !local {
			if err == nil {
				t.Fatal("default local source no longer requires contract")
			}
			continue
		}
		if err != nil {
			t.Fatalf("native local source still requires contract: %v", err)
		}
		if values["SETTLEMENT_INTEGRATION_ENABLED"] != "false" || values["CONTRACT_INTEGRATION_CLIENT_ID"] != "" {
			t.Fatal("local mode created or enabled a fake source")
		}
	}
}
