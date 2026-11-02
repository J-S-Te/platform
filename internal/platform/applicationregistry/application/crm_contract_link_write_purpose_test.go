package application

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCRMContractLinkWritePurposeMatchesCRMMachinePermission 验证平台受控用途
// crm_contract_link_write 的固定元数据（BIZ-XEVT2-02）：scope 必须与 CRM 机器入口
// 白名单 opportunity.signed.write 一致，客户端后缀可被部署 Agent 稳定绑定。
// 参数：t 为测试上下文。
// 返回值：无；用途定义缺失或 scope 漂移时测试失败。
func TestCRMContractLinkWritePurposeMatchesCRMMachinePermission(t *testing.T) {
	if ServiceCredentialCRMContractLinkWrite != "crm_contract_link_write" {
		t.Fatalf("purpose constant = %q", ServiceCredentialCRMContractLinkWrite)
	}
	definition, registered := integratedServicePurposeRegistry[ServiceCredentialCRMContractLinkWrite]
	if !registered {
		t.Fatal("crm_contract_link_write is not registered as a platform service purpose")
	}
	if definition.purpose != ServiceCredentialCRMContractLinkWrite ||
		definition.suffix != "crm-contract-link" ||
		definition.name != "CRM Contract Link Writer" ||
		definition.scope != "opportunity.signed.write" {
		t.Fatalf("unexpected purpose definition: %#v", definition)
	}
}

// TestContractManagementDefaultsProvisionCRMContractLinkClient 验证合同子系统默认用途
// 包含关联回写用途，且按默认接入生成单 scope 客户端
// contract_management-<env>-crm-contract-link。
// 参数：t 为测试上下文，使用既有 onboarding 仓库桩。
// 返回值：无；默认用途缺失或客户端 scope 过宽时测试失败。
func TestContractManagementDefaultsProvisionCRMContractLinkClient(t *testing.T) {
	if !slices.Contains(hardcodedIntegratedServicePurposes(integratedContractApplicationCode), ServiceCredentialCRMContractLinkWrite) {
		t.Fatalf("contract_management default purposes do not include %s", ServiceCredentialCRMContractLinkWrite)
	}

	service, err := NewSubsystemOnboardingService(&subsystemOnboardingRepositoryStub{}, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Now()}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	writes, secrets, err := service.buildIntegratedServiceClients(SubsystemOnboardingInput{TenantID: "tenant", OperatorID: "operator", ApplicationCode: integratedContractApplicationCode, ApplicationName: "合同管理系统", Environment: "prod", AllowedServiceBindings: nil}, "application", "environment", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var linkClient *SubsystemServiceClientWrite
	for index := range writes {
		if writes[index].Purpose == ServiceCredentialCRMContractLinkWrite {
			linkClient = &writes[index]
		}
	}
	if linkClient == nil {
		t.Fatal("contract onboarding did not create the CRM contract link client")
	}
	if linkClient.OAuthClient.ClientID != "contract_management-prod-crm-contract-link" {
		t.Fatalf("client ID = %q", linkClient.OAuthClient.ClientID)
	}
	if len(linkClient.OAuthClient.Scopes) != 1 || linkClient.OAuthClient.Scopes[0] != "opportunity.signed.write" {
		t.Fatalf("scopes = %#v, want the single opportunity.signed.write scope", linkClient.OAuthClient.Scopes)
	}
	if !strings.Contains(linkClient.OAuthClient.ClientName, "CRM Contract Link Writer") {
		t.Fatalf("client name = %q", linkClient.OAuthClient.ClientName)
	}
	if len(linkClient.OAuthClient.GrantTypes) != 1 || linkClient.OAuthClient.GrantTypes[0] != "client_credentials" {
		t.Fatalf("grant types = %#v", linkClient.OAuthClient.GrantTypes)
	}
	if secrets[ServiceCredentialCRMContractLinkWrite] == "" {
		t.Fatal("CRM contract link client secret missing")
	}
}
