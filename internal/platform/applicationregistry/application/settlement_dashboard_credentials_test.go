package application

import (
	"context"
	"testing"
)

// TestSettlementDashboardPurpose 验证财务用途独立、默认授予且更新补发。
// 参数：t为测试上下文。
// 返回值：无，违反最小用途契约时失败。
func TestSettlementDashboardPurpose(t *testing.T) {
	const purpose = "settlement_dashboard_read"
	spec, ok := integratedServicePurposeRegistry[purpose]
	if !ok || spec.scope != "dashboard.settlement.read" {
		t.Fatalf("missing isolated purpose: %+v", spec)
	}
	found := false
	for _, p := range hardcodedIntegratedServicePurposes("data_analysis") {
		if p == purpose {
			found = true
		}
	}
	if !found {
		t.Fatal("missing default purpose")
	}
	found = false
	for _, r := range SubsystemUpdateServiceCredentialRequirements("data_analysis") {
		if r.Purpose == purpose {
			found = r.Rotate && r.Suffix == "settlement-dashboard" && sameStringSetValues(r.Scopes, []string{"dashboard.settlement.read"})
		}
	}
	if !found || !RequiresStrictSubsystemUpdateServiceClientValidation("data_analysis", purpose) {
		t.Fatal("missing strict update delivery")
	}
}

// TestSettlementDashboardRelatedIdentity 验证接收端只解析同环境公开身份。
// 参数：t为测试上下文。
// 返回值：无，身份缺失或漂移未正确处理时失败。
func TestSettlementDashboardRelatedIdentity(t *testing.T) {
	client := canonicalRelatedClient()
	client.ClientID = "data_analysis-dev-settlement-dashboard"
	client.Scopes = []string{"dashboard.settlement.read"}
	resolver := relatedResolverFor(t, &relatedRegistryStub{applicationID: client.ApplicationID, environmentID: client.EnvironmentID}, relatedClientListerStub{clients: []OAuthClientView{client}})
	got, err := resolver.resolveSettlementDashboardSource(context.Background(), "tenant-1", "dev")
	if err != nil || len(got) != 1 || got[0].Client.ID != client.ID {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	client.Scopes = append(client.Scopes, "audit.ingest")
	resolver = relatedResolverFor(t, &relatedRegistryStub{applicationID: client.ApplicationID, environmentID: client.EnvironmentID}, relatedClientListerStub{clients: []OAuthClientView{client}})
	if _, err = resolver.resolveSettlementDashboardSource(context.Background(), "tenant-1", "dev"); err == nil {
		t.Fatal("accepted scope drift")
	}
}
