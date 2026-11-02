package application

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCRMCreditPaymentIngestPurposeMatchesCRMMachinePermission 验证平台受控用途
// crm_credit_payment_ingest 的固定元数据（BIZ-XAPI-02 / BIZ-XWIRE-06）：
// scope 必须与 CRM 机器入口权限白名单一致，客户端名与后缀可被部署 Agent 稳定绑定。
// 参数：t为测试上下文。
// 返回值：无；用途定义缺失或 scope 漂移时测试失败。
func TestCRMCreditPaymentIngestPurposeMatchesCRMMachinePermission(t *testing.T) {
	if ServiceCredentialCRMCreditPaymentIngest != "crm_credit_payment_ingest" {
		t.Fatalf("purpose constant = %q", ServiceCredentialCRMCreditPaymentIngest)
	}
	definition, registered := integratedServicePurposeRegistry[ServiceCredentialCRMCreditPaymentIngest]
	if !registered {
		t.Fatal("crm_credit_payment_ingest is not registered as a platform service purpose")
	}
	if definition.purpose != ServiceCredentialCRMCreditPaymentIngest ||
		definition.suffix != "crm-credit-ingest" ||
		definition.name != "CRM Credit Payment Ingester" ||
		definition.scope != "customer.credit.payment.ingest" {
		t.Fatalf("unexpected purpose definition: %#v", definition)
	}
}

// TestSettlementDefaultsProvisionCRMCreditIngestClient 验证结算默认用途包含信用同步，
// 且按平台默认接入时生成单 scope 客户端 settlement-<env>-crm-credit-ingest。
// 参数：t为测试上下文，使用既有 onboarding 仓库桩。
// 返回值：无；默认用途缺失或客户端 scope 过宽时测试失败。
func TestSettlementDefaultsProvisionCRMCreditIngestClient(t *testing.T) {
	for _, applicationCode := range []string{"settlement", "settlement_and_invoicing"} {
		if !slices.Contains(hardcodedIntegratedServicePurposes(applicationCode), ServiceCredentialCRMCreditPaymentIngest) {
			t.Fatalf("%s default purposes do not include %s", applicationCode, ServiceCredentialCRMCreditPaymentIngest)
		}
	}

	service, err := NewSubsystemOnboardingService(&subsystemOnboardingRepositoryStub{}, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Now()}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	writes, secrets, err := service.buildIntegratedServiceClients(SubsystemOnboardingInput{TenantID: "tenant", OperatorID: "operator", ApplicationCode: "settlement", ApplicationName: "结算与开票管理系统", Environment: "prod", AllowedServiceBindings: nil}, "application", "environment", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var creditClient *SubsystemServiceClientWrite
	for index := range writes {
		if writes[index].Purpose == ServiceCredentialCRMCreditPaymentIngest {
			creditClient = &writes[index]
		}
	}
	if creditClient == nil {
		t.Fatal("settlement onboarding did not create the CRM credit ingest client")
	}
	if creditClient.OAuthClient.ClientID != "settlement-prod-crm-credit-ingest" {
		t.Fatalf("client ID = %q", creditClient.OAuthClient.ClientID)
	}
	if len(creditClient.OAuthClient.Scopes) != 1 || creditClient.OAuthClient.Scopes[0] != "customer.credit.payment.ingest" {
		t.Fatalf("scopes = %#v, want the single CRM credit ingest scope", creditClient.OAuthClient.Scopes)
	}
	if !strings.Contains(creditClient.OAuthClient.ClientName, "CRM Credit Payment Ingester") {
		t.Fatalf("client name = %q", creditClient.OAuthClient.ClientName)
	}
	if secrets[ServiceCredentialCRMCreditPaymentIngest] == "" {
		t.Fatal("CRM credit ingest client secret missing")
	}
}
