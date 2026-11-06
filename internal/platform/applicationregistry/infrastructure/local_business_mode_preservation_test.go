package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// localModeRuntimeFile 保存只包含合成业务选择的临时运行文件。
// 参数：t为测试上下文，content为该用例的显式配置。
// 返回值：独立临时文件路径，不读取或改写工作区运行文件。
func localModeRuntimeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env.local")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// localModeContractInput 交付真实自动更新分支需要的合成业务凭据。
// 参数：无。
// 返回值：具有隔离测试身份的合同更新输入，不启动容器。
func localModeContractInput() application.SubsystemProvisioningInput {
	input := contractNotificationTestInput()
	for _, purpose := range []string{application.ServiceCredentialCRMContractReferenceRead,
		application.ServiceCredentialProjectContractImport, application.ServiceCredentialCRMContractLinkWrite} {
		input.ServiceCredentials = append(input.ServiceCredentials, application.SubsystemServiceCredential{
			Purpose: purpose, OAuthClient: application.OAuthClientView{ClientID: "fixture-" + purpose},
			PlaintextSecret: "fixture-secret"})
	}
	return input
}

// TestLocalAutomaticUpdatePreservesExplicitNativeSelections 验证正式自动更新不重开已选独立来源的联动。
// 参数：t为测试上下文，全部写入只发生在临时文件。
// 返回值：无；自动生成的true覆盖用户false时失败。
func TestLocalAutomaticUpdatePreservesExplicitNativeSelections(t *testing.T) {
	p := &LocalDockerSubsystemProvisioner{}
	t.Run("contract", func(t *testing.T) {
		path := localModeRuntimeFile(t, "CONTRACT_COUNTERPARTY_SOURCE=local\nCRM_REFERENCE_ENABLED=false\nPROJECT_INTEGRATION_ENABLED=false\nSETTLEMENT_INTEGRATION_OUTBOUND_ENABLED=false\nCRM_LINK_ENABLED=false\n")
		if err := p.updateServiceCredentialRuntimeConfiguration(localModeContractInput(), path); err != nil {
			t.Fatal(err)
		}
		values, err := readEnvironmentValues(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"CRM_REFERENCE_ENABLED", "PROJECT_INTEGRATION_ENABLED", "SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED", "CRM_LINK_ENABLED"} {
			if values[key] != "false" {
				t.Errorf("native contract reopened %s=%q", key, values[key])
			}
		}
	})
	t.Run("customer", func(t *testing.T) {
		path := localModeRuntimeFile(t, "CRM_WIN_CONFIRMATION_MODE=local_evidence\nCONTRACT_VERIFICATION_ENABLED=false\nCONTRACT_SIGNED_COUNT_ENABLED=false\n")
		if err := p.updateServiceCredentialRuntimeConfiguration(customerContractIdentityTestInput(), path); err != nil {
			t.Fatal(err)
		}
		values, err := readEnvironmentValues(path)
		if err != nil {
			t.Fatal(err)
		}
		if values["CONTRACT_VERIFICATION_ENABLED"] != "false" || values["CONTRACT_SIGNED_COUNT_ENABLED"] != "false" {
			t.Fatal("local-evidence contract integration reopened")
		}
	})
	t.Run("project", func(t *testing.T) {
		path := localModeRuntimeFile(t, "PROJECT_NATIVE_ENABLED=true\nCONTRACT_INTEGRATION_ENABLED=false\nCONTRACT_INTEGRATION_REQUIRE_BEARER=false\n")
		input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{ApplicationCode: integratedProjectApplicationCode,
			Environment: "dev", PublicURL: "http://localhost:8081/project_management/",
			ServiceCredentials: []application.SubsystemServiceCredential{{Purpose: application.ServiceCredentialNotificationIngest,
				OAuthClient: application.OAuthClientView{ClientID: "project-notify"}, PlaintextSecret: "fixture-secret"}}})
		if err := p.updatePublicRuntimeConfiguration(input, path); err != nil {
			t.Fatal(err)
		}
		values, err := readEnvironmentValues(path)
		if err != nil {
			t.Fatal(err)
		}
		if values["CONTRACT_INTEGRATION_ENABLED"] != "false" || values["CONTRACT_INTEGRATION_REQUIRE_BEARER"] != "false" {
			t.Fatal("native project integration pair reopened")
		}
	})
}

// TestLocalBusinessFlagWhitelistAndLegacyDefaults 验证保留范围与旧全栈默认严格分开。
// 参数：t为测试上下文，模式及旗标均为临时合成文件。
// 返回值：无；未选新模式、安全开关或已有true被错误锁定时失败。
func TestLocalBusinessFlagWhitelistAndLegacyDefaults(t *testing.T) {
	for _, item := range []struct {
		code, mode string
		flags      []string
	}{
		{integratedContractApplicationCode, "CONTRACT_COUNTERPARTY_SOURCE=local\n", []string{"CRM_REFERENCE_ENABLED", "PROJECT_INTEGRATION_ENABLED", "SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED", "CRM_LINK_ENABLED"}},
		{integratedContractApplicationCode, "CONTRACT_CATEGORY_SOURCE=local\n", []string{"CRM_REFERENCE_ENABLED", "PROJECT_INTEGRATION_ENABLED", "SETTLEMENT_INTEGRATION_OUTBOUND_ENABLED", "CRM_LINK_ENABLED"}},
		{integratedCustomerApplicationCode, "CRM_WIN_CONFIRMATION_MODE=local_evidence\n", []string{"CONTRACT_VERIFICATION_ENABLED", "CONTRACT_SIGNED_COUNT_ENABLED"}},
		{integratedProjectApplicationCode, "PROJECT_NATIVE_ENABLED=true\n", []string{"CONTRACT_INTEGRATION_ENABLED", "CONTRACT_APPROVAL_VALIDATION_ENABLED", "PROJECT_CONTRACT_IMPORT_ENABLED", "SETTLEMENT_REFERENCE_ENABLED"}},
		{integratedSettlementApplicationCode, "SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true\n", []string{"SETTLEMENT_INTEGRATION_ENABLED", "SETTLEMENT_CONTRACT_IMPORT_ENABLED", "SETTLEMENT_CRM_CREDIT_SYNC_ENABLED"}},
		{integratedPortalApplicationCode, "PORTAL_CUSTOMER_DIRECTORY_MODE=local\n", []string{"PORTAL_INVITE_ENABLED"}},
	} {
		t.Run(item.mode, func(t *testing.T) {
			for _, selected := range []bool{false, true} {
				for _, current := range []string{"false", "true", "", "not-a-bool"} {
					content := ""
					if selected {
						content = item.mode
					}
					values := map[string]string{}
					for _, key := range item.flags {
						content += key + "=" + current + "\n"
						values[key] = "true"
					}
					security := []string{"DEV_AUTH_ENABLED", "OWNER_DIRECTORY_ENABLED", "PLATFORM_AUDIT_ENABLED", "COMMERCIAL_LICENSE_ENABLED", "PLATFORM_EXTERNAL_IDENTITY_ENABLED", "CRM_SIGNED_COUNT_MACHINE_ENABLED", "APP_CORS_ALLOW_CREDENTIALS"}
					for _, key := range security {
						content += key + "=false\n"
						values[key] = "true"
					}
					path := localModeRuntimeFile(t, content)
					if err := preserveLocalBusinessIntegrationFlags(item.code, path, path, values); err != nil {
						t.Fatal(err)
					}
					want := "true"
					if selected && current == "false" {
						want = "false"
					}
					for _, key := range item.flags {
						if values[key] != want {
							t.Fatalf("%s selected=%v old=%q value=%q want=%q", key, selected, current, values[key], want)
						}
					}
					for _, key := range security {
						if values[key] != "true" {
							t.Fatalf("business preservation reached security flag %s", key)
						}
					}
				}
			}
		})
	}
}

// TestLocalProjectBearerPairNeverWeakensActiveIntegration 核对项目合法关闭对及开启时严格认证。
// 参数：t为测试上下文，existing控制原入口状态。
// 返回值：无；开启入口沿用false认证或关闭入口被写为无效组合时失败。
func TestLocalProjectBearerPairNeverWeakensActiveIntegration(t *testing.T) {
	for _, existing := range []string{"false", "true"} {
		path := localModeRuntimeFile(t, "PROJECT_NATIVE_ENABLED=true\nCONTRACT_INTEGRATION_ENABLED="+existing+"\nCONTRACT_INTEGRATION_REQUIRE_BEARER=false\n")
		values := map[string]string{"CONTRACT_INTEGRATION_ENABLED": "true", "CONTRACT_INTEGRATION_REQUIRE_BEARER": "true"}
		if err := preserveLocalBusinessIntegrationFlags(integratedProjectApplicationCode, path, path, values); err != nil {
			t.Fatal(err)
		}
		if values["CONTRACT_INTEGRATION_ENABLED"] != existing || values["CONTRACT_INTEGRATION_REQUIRE_BEARER"] != existing {
			t.Fatalf("invalid integration/bearer pair: %v", values)
		}
	}
	path := localModeRuntimeFile(t, "PROJECT_NATIVE_ENABLED=true\nCONTRACT_INTEGRATION_ENABLED=false\nCONTRACT_INTEGRATION_REQUIRE_BEARER=false\n")
	values := map[string]string{"CONTRACT_INTEGRATION_REQUIRE_BEARER": "true"}
	if err := preserveLocalBusinessIntegrationFlags(integratedProjectApplicationCode, path, path, values); err != nil {
		t.Fatal(err)
	}
	if values["CONTRACT_INTEGRATION_REQUIRE_BEARER"] != "true" {
		t.Fatal("Bearer changed without preserving same-update integration disable")
	}
}

// TestLocalExplicitReenableStillUsesNormalWriter 验证管理员显式重开不被自动保留逻辑永久锁住。
// 参数：t为测试上下文，直接使用既有通用配置写入契约。
// 返回值：无；此前false状态阻止显式true或来源字段丢失时失败。
func TestLocalExplicitReenableStillUsesNormalWriter(t *testing.T) {
	path := localModeRuntimeFile(t, "PROJECT_NATIVE_ENABLED=true\nCONTRACT_INTEGRATION_ENABLED=false\nCONTRACT_INTEGRATION_REQUIRE_BEARER=false\n")
	if err := updateSubsystemEnvironment(path, path, map[string]string{"CONTRACT_INTEGRATION_ENABLED": "true", "CONTRACT_INTEGRATION_REQUIRE_BEARER": "true"}); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"CONTRACT_INTEGRATION_ENABLED": "true", "CONTRACT_INTEGRATION_REQUIRE_BEARER": "true"}
	if err := preserveLocalBusinessIntegrationFlags(integratedProjectApplicationCode, path, path, values); err != nil {
		t.Fatal(err)
	}
	if values["CONTRACT_INTEGRATION_ENABLED"] != "true" || values["CONTRACT_INTEGRATION_REQUIRE_BEARER"] != "true" {
		t.Fatal("explicit reenable was latched off")
	}
}

// TestLocalPortalAdoptRetainsCRMInviteSelection 验证门户来源选择约束自动写入另一个应用的邀请旗标。
// 参数：t为测试上下文，Provision仅使用记录runner和临时目录。
// 返回值：无；不会启动任何进程或连接数据库。
func TestLocalPortalAdoptRetainsCRMInviteSelection(t *testing.T) {
	root, platformRoot, _, gatewayScript := createIntegratedProvisionerFixture(t, integratedPortalApplicationCode)
	modePath := filepath.Join(platformRoot, "docker", ".env.portal.local.example")
	content, err := os.ReadFile(modePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modePath, append(content, []byte("\nPORTAL_CUSTOMER_DIRECTORY_MODE=local\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	crmPath := filepath.Join(platformRoot, "docker", ".env.customer.local")
	if err := os.WriteFile(crmPath, []byte("PORTAL_INVITE_ENABLED=false\nPLATFORM_EXTERNAL_IDENTITY_ENABLED=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{Enabled: true,
		ProjectsRoot: root, GatewayScriptPath: gatewayScript, PlatformComposeProject: "basic-platform-local", Timeout: time.Second}, &recordingSubsystemRunner{})
	if err != nil {
		t.Fatal(err)
	}
	input := portalLocalRuntimeInput()
	if err := p.Provision(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	current, err := readEnvironmentValues(crmPath)
	if err != nil {
		t.Fatal(err)
	}
	if current["PORTAL_INVITE_ENABLED"] != "false" || current["PLATFORM_EXTERNAL_IDENTITY_ENABLED"] != "false" {
		t.Fatal("local Portal adoption wrote unrelated CRM runtime")
	}
	portal, err := os.ReadFile(filepath.Join(platformRoot, "docker", ".env.portal.local"))
	if err != nil || !strings.Contains(string(portal), "PORTAL_CUSTOMER_DIRECTORY_MODE=local") {
		t.Fatal("Portal mode lost during adoption")
	}
}

// TestLocalNativeSettlementUpdateRetainsDisabledIngest 验证结算专用重建分支不会绕过来源保留。
// 参数：t为测试上下文，Update仅向记录runner提交命令，不执行进程。
// 返回值：无；原生财务模式被自动重开合同接入时失败。
func TestLocalNativeSettlementUpdateRetainsDisabledIngest(t *testing.T) {
	root, _, _, gatewayScript := createIntegratedProvisionerFixture(t, integratedCustomerApplicationCode)
	directory := filepath.Join(root, "Settlement")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, ".env.local")
	if err := os.WriteFile(path, []byte("SETTLEMENT_NATIVE_FINANCIAL_ENABLED=true\nSETTLEMENT_INTEGRATION_ENABLED=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{Enabled: true,
		ProjectsRoot: root, GatewayScriptPath: gatewayScript, PlatformComposeProject: "basic-platform-local", Timeout: time.Second}, &recordingSubsystemRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Update(context.Background(), p1FixtureProvisioningInput(application.SubsystemProvisioningInput{
		ApplicationCode: integratedSettlementApplicationCode, Environment: "dev"})); err != nil {
		t.Fatal(err)
	}
	values, err := readEnvironmentValues(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["SETTLEMENT_INTEGRATION_ENABLED"] != "false" || values["SETTLEMENT_NATIVE_FINANCIAL_ENABLED"] != "true" {
		t.Fatal("native Settlement update reset business selection")
	}
}
