package application

import (
	"slices"
	"testing"
	"time"
)

// TestCRMImportProvisionPurposesMatchCRMMachinePermission 验证历史导入开立 CRM
// 客户/商机两个受控用途的固定元数据：scope 必须与 CRM 内部导入通道的机器入口
// 白名单（customer.import_provision / opportunity.import_provision）完全一致，
// 后缀必须能被部署 Agent 按 applicationCode-environment-suffix 稳定推导。
// 参数：t为测试上下文。
// 返回值：无；用途未登记或 scope/后缀漂移时测试失败。
func TestCRMImportProvisionPurposesMatchCRMMachinePermission(t *testing.T) {
	if ServiceCredentialCRMCustomerImportProvision != "crm_customer_import_provision" {
		t.Fatalf("purpose constant = %q", ServiceCredentialCRMCustomerImportProvision)
	}
	if ServiceCredentialCRMOpportunityImportProvision != "crm_opportunity_import_provision" {
		t.Fatalf("purpose constant = %q", ServiceCredentialCRMOpportunityImportProvision)
	}
	for _, want := range []integratedServicePurposeDefinition{
		{purpose: ServiceCredentialCRMCustomerImportProvision, suffix: "crm-customer-import-provision", name: "CRM Customer Import Provisioner", scope: "customer.import_provision"},
		{purpose: ServiceCredentialCRMOpportunityImportProvision, suffix: "crm-opportunity-import-provision", name: "CRM Opportunity Import Provisioner", scope: "opportunity.import_provision"},
	} {
		definition, registered := integratedServicePurposeRegistry[want.purpose]
		if !registered {
			t.Fatalf("%s is not registered as a platform service purpose", want.purpose)
		}
		if definition != want {
			t.Fatalf("unexpected purpose definition: %#v, want %#v", definition, want)
		}
	}
}

// TestContractDefaultsProvisionCRMImportProvisionClients 验证合同默认用途包含导入
// 开立客户/商机，且按平台默认接入时生成两个单 scope 客户端：一个用途一个凭据，
// 不允许把客户与商机权限合并成通用客户端。
// 参数：t为测试上下文，使用既有 onboarding 仓库桩。
// 返回值：无；默认用途缺失、客户端命名不可推导或 scope 过宽时测试失败。
func TestContractDefaultsProvisionCRMImportProvisionClients(t *testing.T) {
	for _, purpose := range []string{ServiceCredentialCRMCustomerImportProvision, ServiceCredentialCRMOpportunityImportProvision} {
		if !slices.Contains(hardcodedIntegratedServicePurposes(integratedContractApplicationCode), purpose) {
			t.Fatalf("contract default purposes do not include %s", purpose)
		}
	}

	service, err := NewSubsystemOnboardingService(&subsystemOnboardingRepositoryStub{}, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Now()}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	writes, secrets, err := service.buildIntegratedServiceClients(SubsystemOnboardingInput{TenantID: "tenant", OperatorID: "operator", ApplicationCode: integratedContractApplicationCode, ApplicationName: "合同管理系统", Environment: "prod", AllowedServiceBindings: nil}, "application", "environment", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		ServiceCredentialCRMCustomerImportProvision:    "customer.import_provision",
		ServiceCredentialCRMOpportunityImportProvision: "opportunity.import_provision",
	}
	seen := map[string]bool{}
	for index := range writes {
		client := writes[index]
		scope, expected := want[client.Purpose]
		if !expected {
			continue
		}
		seen[client.Purpose] = true
		if client.OAuthClient.ClientID != integratedContractApplicationCode+"-prod-"+integratedServicePurposeRegistry[client.Purpose].suffix {
			t.Fatalf("client ID = %q for purpose %q", client.OAuthClient.ClientID, client.Purpose)
		}
		if len(client.OAuthClient.Scopes) != 1 || client.OAuthClient.Scopes[0] != scope {
			t.Fatalf("purpose %q scopes = %#v, want the single %s scope", client.Purpose, client.OAuthClient.Scopes, scope)
		}
		if secrets[client.Purpose] == "" {
			t.Fatalf("purpose %q client secret missing", client.Purpose)
		}
	}
	for purpose := range want {
		if !seen[purpose] {
			t.Fatalf("contract onboarding did not create the %s client", purpose)
		}
	}
}

// TestContractUpdateRequirementsCarryCRMImportProvisionScopes 验证受控更新/重试必须
// 重新交付导入开立客户/商机的凭据，且 scope 与 CRM 白名单一致、按同类别用途轮换。
// 参数：t为测试上下文。
// 返回值：无；缺需求、scope 漂移或轮换策略漂移时测试失败。
func TestContractUpdateRequirementsCarryCRMImportProvisionScopes(t *testing.T) {
	want := map[string]string{
		ServiceCredentialCRMCustomerImportProvision:    "crm-customer-import-provision",
		ServiceCredentialCRMOpportunityImportProvision: "crm-opportunity-import-provision",
	}
	found := map[string]bool{}
	for _, requirement := range SubsystemUpdateServiceCredentialRequirements(integratedContractApplicationCode) {
		suffix, expected := want[requirement.Purpose]
		if !expected {
			continue
		}
		found[requirement.Purpose] = true
		if requirement.Suffix != suffix {
			t.Fatalf("purpose %q suffix = %q, want %q", requirement.Purpose, requirement.Suffix, suffix)
		}
		if requirement.PrimaryScope() != integratedServicePurposeRegistry[requirement.Purpose].scope {
			t.Fatalf("purpose %q scope = %q, want the CRM machine whitelist scope", requirement.Purpose, requirement.PrimaryScope())
		}
		if !requirement.Rotate {
			t.Fatalf("purpose %q must rotate on controlled update like other CRM machine credentials", requirement.Purpose)
		}
	}
	for purpose := range want {
		if !found[purpose] {
			t.Fatalf("contract update requirements missing %s", purpose)
		}
	}
}
