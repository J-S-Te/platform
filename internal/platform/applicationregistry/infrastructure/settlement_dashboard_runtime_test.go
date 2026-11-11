package infrastructure

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"os"
	"path/filepath"
	"testing"
)

// dashboardReceiverFixture 构造完全隔离的财务接收端元数据。
// 参数：无。
// 返回值：同租户与环境的合成登记身份。
func dashboardReceiverFixture() application.SubsystemProvisioningInput {
	return application.SubsystemProvisioningInput{ApplicationCode: "settlement", TenantID: "tenant", Environment: "prod",
		RelatedServiceClients: []application.RelatedSubsystemServiceClient{{ApplicationCode: "data_analysis", ApplicationID: "source-app", EnvironmentID: "source-env", Environment: "prod", Purpose: "settlement_dashboard_read", Client: application.OAuthClientView{ID: "registered-oauth-id", ClientID: "data_analysis-prod-settlement-dashboard", TenantID: "tenant", ApplicationID: "source-app", EnvironmentID: "source-env", Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: []string{"dashboard.settlement.read"}}}}}
}

// TestSettlementDashboardReceiverBindings 验证启用依赖公开身份且两种ID不混用。
// 参数：t为测试上下文。
// 返回值：无，缺失、越权或秘密外发时失败。
func TestSettlementDashboardReceiverBindings(t *testing.T) {
	input := dashboardReceiverFixture()
	for source, want := range map[string]string{
		"settlement_dashboard_enabled":                                            "true",
		"related_service.data_analysis.settlement_dashboard_read.client_id":       "data_analysis-prod-settlement-dashboard",
		"related_service.data_analysis.settlement_dashboard_read.oauth_client_id": "registered-oauth-id",
	} {
		got, err := resolveProductionBinding(input, source)
		if err != nil || got != want {
			t.Fatalf("%s: got=%q err=%v", source, got, err)
		}
	}
	if _, err := resolveProductionBinding(input, settlementDashboardRelatedPrefix+"client_secret"); err == nil {
		t.Fatal("source secret accessible")
	}
	input.RelatedServiceClients = nil
	values := map[string]string{"SETTLEMENT_DASHBOARD_MACHINE_CLIENT_ID": "stale"}
	if err := settlementDashboardRuntimeValues(input, values); err != nil {
		t.Fatal(err)
	}
	if values["SETTLEMENT_DASHBOARD_MACHINE_ENABLED"] != "false" || values["SETTLEMENT_DASHBOARD_MACHINE_CLIENT_ID"] != "" || values["SETTLEMENT_DASHBOARD_MACHINE_UNAVAILABLE_REASON"] == "" {
		t.Fatalf("unsafe disabled values: %+v", values)
	}
}

// TestSettlementDashboardReceiverDrift 验证任何身份漂移均拒绝发布。
// 参数：t为测试上下文。
// 返回值：无，身份被误接受时失败。
func TestSettlementDashboardReceiverDrift(t *testing.T) {
	mutations := []func(*application.SubsystemProvisioningInput){
		func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Client.ID = "" },
		func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Client.TenantID = "other" },
		func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Environment = "dev" },
		func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients[0].Client.Scopes = []string{"dashboard.settlement.read", "audit.ingest"}
		},
		func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients = append(i.RelatedServiceClients, i.RelatedServiceClients[0])
		},
	}
	for index, mutate := range mutations {
		input := dashboardReceiverFixture()
		mutate(&input)
		if _, err := resolveProductionBinding(input, "settlement_dashboard_enabled"); err == nil {
			t.Fatalf("accepted drift %d", index)
		}
	}
}

// TestSettlementDashboardSenderSecretInheritance 验证只写密钥仅能在同身份下继承。
// 参数：t为隔离元数据测试上下文。
// 返回值：无，缺密钥、漂移或复用被错误接受时失败。
func TestSettlementDashboardSenderSecretInheritance(t *testing.T) {
	source := dashboardReceiverFixture().RelatedServiceClients[0]
	input := application.SubsystemProvisioningInput{ApplicationCode: "data_analysis", TenantID: "tenant", ApplicationID: "source-app", EnvironmentID: "source-env", Environment: "prod", ServiceCredentials: []application.SubsystemServiceCredential{{Purpose: "settlement_dashboard_read", OAuthClient: source.Client}}}
	current := map[string]string{"SETTLEMENT_MACHINE_CLIENT_ID": source.Client.ClientID, "SETTLEMENT_MACHINE_CLIENT_SECRET": "only-existing-secret", "SETTLEMENT_FINANCE_URL": "", "PROJECT_REPORT_URL": "http://selected.example/reports"}
	values := map[string]string{}
	if err := dataAnalysisSettlementRuntimeValues(input, current, values); err != nil {
		t.Fatal(err)
	}
	if _, replaced := values["SETTLEMENT_MACHINE_CLIENT_SECRET"]; replaced || values["SETTLEMENT_FINANCE_URL"] != "" || values["PROJECT_REPORT_URL"] != current["PROJECT_REPORT_URL"] {
		t.Fatal("overwrote existing secret or explicit URL")
	}
	current["SETTLEMENT_MACHINE_CLIENT_ID"] = "other"
	if err := dataAnalysisSettlementRuntimeValues(input, current, map[string]string{}); err == nil {
		t.Fatal("accepted secret from another client")
	}
	input.ServiceCredentials[0].PlaintextSecret = "same-secret"
	if err := dataAnalysisSettlementRuntimeValues(input, nil, map[string]string{"PROJECT_MACHINE_CLIENT_SECRET": "same-secret"}); err == nil {
		t.Fatal("accepted shared secret")
	}
}

// TestSettlementDashboardProductionURLPreservation 验证受控更新保持独立模式及显式来源。
// 参数：t为测试上下文。
// 返回值：无，已选URL被默认覆盖时失败。
func TestSettlementDashboardProductionURLPreservation(t *testing.T) {
	current := map[string]string{"SETTLEMENT_FINANCE_URL": "", "PROJECT_REPORT_URL": "http://selected.example/reports", "DASHBOARD_INTEGRATION_SYNC_ENABLED": "false", "DASHBOARD_FINANCE_SOURCE": "standard"}
	values := map[string]string{"SETTLEMENT_FINANCE_URL": "http://default/finance", "PROJECT_REPORT_URL": "http://default/reports"}
	preserveDataAnalysisDashboardSources("data_analysis", current, values)
	if values["SETTLEMENT_FINANCE_URL"] != "" || values["PROJECT_REPORT_URL"] != current["PROJECT_REPORT_URL"] {
		t.Fatal("overwrote explicit sources")
	}
}

// TestSettlementDashboardProductionRuntimeLifecycle 验证正式清单实际生成、更新及禁用接收端配置。
// 参数：t为仅使用临时文件和合成身份的测试上下文。
// 返回值：无，来源ID混用、静默继承或来源消失未关闭时失败。
func TestSettlementDashboardProductionRuntimeLifecycle(t *testing.T) {
	target, input := settlementDashboardProductionFixture(t, "settlement")
	related := dashboardReceiverFixture().RelatedServiceClients[0]
	related.Client.TenantID = input.TenantID
	input.RelatedServiceClients = append(input.RelatedServiceClients, related)
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/settlement.env")
	values, err := readEnvironmentValues(path)
	if err != nil || values["SETTLEMENT_DASHBOARD_MACHINE_ENABLED"] != "true" || values["SETTLEMENT_DASHBOARD_MACHINE_OAUTH_CLIENT_ID"] != "registered-oauth-id" || values["SETTLEMENT_DASHBOARD_MACHINE_CLIENT_ID"] != "data_analysis-prod-settlement-dashboard" {
		t.Fatalf("invalid delivered receiver: %v", err)
	}
	input.RelatedServiceClients = input.RelatedServiceClients[:len(input.RelatedServiceClients)-1]
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	values, err = readEnvironmentValues(path)
	if err != nil || values["SETTLEMENT_DASHBOARD_MACHINE_ENABLED"] != "false" || values["SETTLEMENT_DASHBOARD_MACHINE_CLIENT_ID"] != "" || values["SETTLEMENT_DASHBOARD_MACHINE_OAUTH_CLIENT_ID"] != "" {
		t.Fatalf("stale receiver after source removal: %v", err)
	}
}

// settlementDashboardProductionFixture 创建与生产清单同构的隔离配置生成目标。
// 参数：t为测试上下文，code为目标应用编码。
// 返回值：临时部署目标及合成登记身份，绝不读取实际运行配置。
func settlementDashboardProductionFixture(t *testing.T, code string) (*productionComposeTarget, application.SubsystemProvisioningInput) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(platformModuleRoot(t), "deploy", "production")
	profiles, _, err := loadProductionSubsystemProfiles(source, filepath.Join(source, "subsystems.d"))
	if err != nil {
		t.Fatal(err)
	}
	var profile productionSubsystemProfile
	for _, candidate := range profiles {
		if candidate.Manifest.Application.Code == code {
			profile = candidate
		}
	}
	if profile.Manifest.Application.Code != code {
		t.Fatal("profile missing")
	}
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range profile.Manifest.Runtime.Files {
		content, err := os.ReadFile(filepath.Join(source, file.TemplatePath))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file.Path), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{ApplicationCode: code, Environment: "prod", AuthenticationProvider: "platform", Issuer: "https://identity.example", PublicURL: "https://identity.example" + profile.Manifest.Application.PathPrefix + "/", PathPrefix: profile.Manifest.Application.PathPrefix, ClientID: "browser-client", ClientSecret: "browser-secret", RedirectURI: "https://identity.example" + profile.Manifest.Application.PathPrefix + "/auth/callback"})
	input.ServiceCredentials = nil
	for _, requirement := range application.SubsystemUpdateServiceCredentialRequirements(code) {
		input.ServiceCredentials = append(input.ServiceCredentials, application.SubsystemServiceCredential{Purpose: requirement.Purpose, PlaintextSecret: "fixture-" + requirement.Purpose, OAuthClient: application.OAuthClientView{ID: "registered-" + requirement.Purpose, TenantID: input.TenantID, ApplicationID: input.ApplicationID, EnvironmentID: input.EnvironmentID, ClientID: code + "-prod-" + requirement.Suffix, Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: application.SubsystemUpdateServiceCredentialScopes(requirement)}})
	}
	return &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}}, input
}

// TestSettlementDashboardSenderCanonicalIdentity 验证生产发送端不能消费错误应用或标签。
// 参数：t为测试上下文。
// 返回值：无，来源应用或规范客户端标签未绑定时失败。
func TestSettlementDashboardSenderCanonicalIdentity(t *testing.T) {
	source := dashboardReceiverFixture().RelatedServiceClients[0]
	for _, code := range []string{"data_analysis", "settlement"} {
		client := source.Client
		if code == "data_analysis" {
			client.ClientID = "reused-project-client"
		}
		input := application.SubsystemProvisioningInput{ApplicationCode: code, TenantID: "tenant", ApplicationID: "source-app", EnvironmentID: "source-env", Environment: "prod", ServiceCredentials: []application.SubsystemServiceCredential{{Purpose: "settlement_dashboard_read", OAuthClient: client, PlaintextSecret: "isolated"}}}
		if _, err := resolveProductionBinding(input, "service.settlement_dashboard_read.client_secret"); err == nil {
			t.Fatalf("accepted sender mismatch %s", code)
		}
	}
}

// TestSettlementDashboardProductionSenderLifecycle 验证发送端只写密钥更新保护及完整独立交付。
// 参数：t为临时配置与合成登记身份测试上下文。
// 返回值：无，密钥未交付或错误身份继承旧密钥时失败。
func TestSettlementDashboardProductionSenderLifecycle(t *testing.T) {
	target, input := settlementDashboardProductionFixture(t, "data_analysis")
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/data-analysis.env")
	values, err := readEnvironmentValues(path)
	if err != nil || values["SETTLEMENT_MACHINE_CLIENT_ID"] != "data_analysis-prod-settlement-dashboard" || values["SETTLEMENT_MACHINE_CLIENT_SECRET"] != "fixture-settlement_dashboard_read" || values["SETTLEMENT_MACHINE_TOKEN_SCOPE"] != "dashboard.settlement.read" {
		t.Fatalf("sender delivery failed: %v", err)
	}
	for index := range input.ServiceCredentials {
		if input.ServiceCredentials[index].Purpose == "settlement_dashboard_read" {
			input.ServiceCredentials[index].PlaintextSecret = ""
		}
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	if err := updateProductionSubsystemEnvironment(path, map[string]string{"SETTLEMENT_MACHINE_CLIENT_ID": "foreign", "SETTLEMENT_FINANCE_URL": "", "DASHBOARD_FINANCE_SOURCE": "standard", "DASHBOARD_INTEGRATION_SYNC_ENABLED": "false"}); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeFixedValues(input); err == nil {
		t.Fatal("inherited a foreign client's secret")
	}
}
