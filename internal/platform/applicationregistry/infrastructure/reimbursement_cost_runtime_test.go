package infrastructure

import (
	"path/filepath"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// costReceiverFixture 构造报销接收端的隔离来源身份。
// 参数：无。
// 返回值：不含真实凭据的结算公开注册元数据。
func costReceiverFixture() application.SubsystemProvisioningInput {
	input := dashboardReceiverFixture()
	input.ApplicationCode = "reimbursement"
	source := &input.RelatedServiceClients[0]
	source.ApplicationCode = "settlement"
	source.Purpose = "settlement_reimbursement_cost_read"
	source.Client.ClientID = "settlement-prod-reimbursement-cost"
	source.Client.Scopes = []string{"reimbursement.cost_candidate.read"}
	return input
}

// costSenderFixtureCredential 构造本次新增用途的完整独立测试凭据。
// 参数：input为测试目标登记身份。
// 返回值：用途隔离的合成元数据与测试secret，不读取运行环境。
func costSenderFixtureCredential(input application.SubsystemProvisioningInput) application.SubsystemServiceCredential {
	return application.SubsystemServiceCredential{
		Purpose:         application.ServiceCredentialSettlementReimbursementCostRead,
		PlaintextSecret: "fixture-reimbursement-cost-secret",
		OAuthClient: application.OAuthClientView{ID: "fixture-reimbursement-cost-id",
			TenantID: input.TenantID, ApplicationID: input.ApplicationID,
			EnvironmentID: input.EnvironmentID,
			ClientID:      "settlement-" + input.Environment + "-reimbursement-cost",
			Status:        "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic",
			GrantTypes: []string{"client_credentials"}, Scopes: []string{"reimbursement.cost_candidate.read"}},
	}
}

// TestReimbursementCostReceiverBindings 验证注册ID与OAuth标签独立且缺失时关闭。
// 参数：t为测试上下文。
// 返回值：无；错误身份、秘密泄漏或默认放行时测试失败。
func TestReimbursementCostReceiverBindings(t *testing.T) {
	input := costReceiverFixture()
	prefix := "related_service.settlement.settlement_reimbursement_cost_read."
	for key, want := range map[string]string{
		"reimbursement_cost_enabled": "true",
		prefix + "client_id":         "settlement-prod-reimbursement-cost",
		prefix + "oauth_client_id":   "registered-oauth-id",
		prefix + "application_id":    "source-app",
		prefix + "environment_id":    "source-env",
	} {
		got, err := resolveProductionBinding(input, key)
		if err != nil || got != want {
			t.Errorf("%s: got=%q err=%v", key, got, err)
		}
		if !validProductionBindingSource(key) {
			t.Errorf("unrecognized binding %s", key)
		}
	}
	if _, err := resolveProductionBinding(input, prefix+"client_secret"); err == nil {
		t.Fatal("source secret exposed")
	}
	input.RelatedServiceClients = nil
	got, err := resolveProductionBinding(input, "reimbursement_cost_enabled")
	if err != nil || got != "false" {
		t.Fatalf("missing source must disable: %q %v", got, err)
	}
}

// TestReimbursementCostReceiverDrift 验证来源及接收端的所有绑定不可漂移。
// 参数：t为测试上下文。
// 返回值：无；任何不匹配被接受时失败。
func TestReimbursementCostReceiverDrift(t *testing.T) {
	for index, mutate := range []func(*application.SubsystemProvisioningInput){
		func(i *application.SubsystemProvisioningInput) { i.ApplicationCode = "project_management" },
		func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Client.ID = "" },
		func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Client.TenantID = "other" },
		func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients[0].Client.ApplicationID = "other"
		},
		func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients[0].Client.EnvironmentID = "other"
		},
		func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Environment = "test" },
		func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients[0].Client.ClientID = "settlement-prod-project-reference"
		},
		func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients[0].Client.Scopes = []string{"reimbursement.cost_candidate.read", "audit.ingest"}
		},
		func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients = append(i.RelatedServiceClients, i.RelatedServiceClients[0])
		},
	} {
		input := costReceiverFixture()
		mutate(&input)
		if _, err := resolveProductionBinding(input, "reimbursement_cost_enabled"); err == nil {
			t.Errorf("accepted drift %d", index)
		}
	}
}

// TestReimbursementCostSenderScope 验证专用用途只接受唯一scope及结算来源。
// 参数：t为测试上下文。
// 返回值：无；权限或调用方漂移被接受时失败。
func TestReimbursementCostSenderScope(t *testing.T) {
	source := costReceiverFixture().RelatedServiceClients[0]
	input := application.SubsystemProvisioningInput{ApplicationCode: "settlement",
		TenantID: "tenant", ApplicationID: "source-app", EnvironmentID: "source-env", Environment: "prod"}
	if err := validateP1ServiceCredential(input, source.Purpose, source.Client); err != nil {
		t.Fatal(err)
	}
	input.ApplicationCode = "data_analysis"
	if err := validateP1ServiceCredential(input, source.Purpose, source.Client); err == nil {
		t.Fatal("accepted wrong sender")
	}
}

// TestReimbursementCostSenderCannotReuseOtherPurpose 验证新用途不借用既有用途身份或秘密。
// 参数：t为合成登记元数据测试上下文。
// 返回值：无；复用身份或secret被接受时失败。
func TestReimbursementCostSenderCannotReuseOtherPurpose(t *testing.T) {
	input := application.SubsystemProvisioningInput{ApplicationCode: "settlement", Environment: "prod",
		TenantID: "tenant", ApplicationID: "app", EnvironmentID: "env"}
	credential := costSenderFixtureCredential(input)
	other := credential
	other.Purpose = application.ServiceCredentialSettlementProjectReferenceRead
	other.OAuthClient.ID, other.OAuthClient.ClientID = "other-id", "settlement-prod-project-reference"
	other.OAuthClient.Scopes = []string{"settlement.project_reference.read"}
	input.ServiceCredentials = []application.SubsystemServiceCredential{credential, other}
	if err := validateP1ServiceCredential(input, credential.Purpose, credential.OAuthClient); err == nil {
		t.Fatal("shared secret accepted")
	}
	input.ServiceCredentials[1].PlaintextSecret = "different"
	if err := validateP1ServiceCredential(input, credential.Purpose, credential.OAuthClient); err != nil {
		t.Fatal(err)
	}
}

// TestReimbursementCostProductionLifecycle 验证正式清单完整生成及失去来源后关闭。
// 参数：t为仅使用临时目录与合成身份的测试上下文。
// 返回值：无；身份混淆或禁用后继承旧身份时失败。
func TestReimbursementCostProductionLifecycle(t *testing.T) {
	target, input := settlementDashboardProductionFixture(t, "reimbursement")
	input.CatalogPublisherClientID = "fixture-catalog"
	input.CatalogPublisherClientSecret = "fixture-catalog-secret"
	source := costReceiverFixture().RelatedServiceClients[0]
	source.Client.TenantID = input.TenantID
	input.RelatedServiceClients = []application.RelatedSubsystemServiceClient{source}
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/reimbursement.env")
	values, err := readEnvironmentValues(path)
	if err != nil || values["REIMBURSEMENT_COST_MACHINE_ENABLED"] != "true" ||
		values["REIMBURSEMENT_COST_MACHINE_CLIENT_ID"] != "settlement-prod-reimbursement-cost" ||
		values["REIMBURSEMENT_COST_MACHINE_OAUTH_CLIENT_ID"] != "registered-oauth-id" ||
		values["REIMBURSEMENT_COST_MACHINE_APPLICATION_ID"] != "source-app" ||
		values["REIMBURSEMENT_COST_MACHINE_ENVIRONMENT_ID"] != "source-env" {
		t.Fatalf("incomplete delivered trust: %v", err)
	}
	if err := target.validateRuntimeIntegrationConfiguration(); err != nil {
		t.Fatal(err)
	}
	input.RelatedServiceClients = nil
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	values, err = readEnvironmentValues(path)
	if err != nil || values["REIMBURSEMENT_COST_MACHINE_ENABLED"] != "false" ||
		values["REIMBURSEMENT_COST_MACHINE_CLIENT_ID"] != "" {
		t.Fatalf("stale source retained: %v", err)
	}
	if err := target.validateRuntimeIntegrationConfiguration(); err != nil {
		t.Fatal(err)
	}
	if err := updateProductionSubsystemEnvironment(path,
		map[string]string{"REIMBURSEMENT_COST_MACHINE_ENABLED": "true"}); err != nil {
		t.Fatal(err)
	}
	if err := target.validateRuntimeIntegrationConfiguration(); err == nil {
		t.Fatal("enabled receiver with missing identities accepted")
	}
}

// TestReimbursementCostProductionSender 验证结算凭据完整下发且只能原身份继承。
// 参数：t为临时运行配置测试上下文。
// 返回值：无；漏接开关、用途或错误继承时失败。
func TestReimbursementCostProductionSender(t *testing.T) {
	target, input := settlementDashboardProductionFixture(t, "settlement")
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/settlement.env")
	values, err := readEnvironmentValues(path)
	if err != nil || values["SETTLEMENT_REIMBURSEMENT_COST_ENABLED"] != "true" ||
		values["SETTLEMENT_REIMBURSEMENT_COST_CLIENT_ID"] != "settlement-prod-reimbursement-cost" ||
		values["SETTLEMENT_REIMBURSEMENT_COST_CLIENT_SECRET"] != "fixture-settlement_reimbursement_cost_read" ||
		values["SETTLEMENT_REIMBURSEMENT_COST_SCOPE"] != "reimbursement.cost_candidate.read" {
		t.Fatalf("incomplete sender: %v", err)
	}
	for index := range input.ServiceCredentials {
		input.ServiceCredentials[index].PlaintextSecret = ""
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	if err := updateProductionSubsystemEnvironment(path,
		map[string]string{"SETTLEMENT_REIMBURSEMENT_COST_ENABLED": "false"}); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	values, err = readEnvironmentValues(path)
	if err != nil || values["SETTLEMENT_REIMBURSEMENT_COST_ENABLED"] != "false" {
		t.Fatalf("explicit disable overwritten: %v", err)
	}
	if err := updateProductionSubsystemEnvironment(path,
		map[string]string{"SETTLEMENT_REIMBURSEMENT_COST_CLIENT_ID": "foreign"}); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeFixedValues(input); err == nil {
		t.Fatal("inherited foreign credential")
	}
}
