package application

import (
	"context"
	"testing"
)

// TestReimbursementCostPurpose 验证财务用途独立、默认授予且更新补发。
// 参数：t为测试上下文。
// 返回值：无，违反最小用途契约时失败。
func TestReimbursementCostPurpose(t *testing.T) {
	const purpose = "settlement_reimbursement_cost_read"
	spec, ok := integratedServicePurposeRegistry[purpose]
	if !ok || spec.scope != "reimbursement.cost_candidate.read" {
		t.Fatalf("missing isolated purpose: %+v", spec)
	}
	found := false
	for _, p := range hardcodedIntegratedServicePurposes("settlement") {
		if p == purpose {
			found = true
		}
	}
	if !found {
		t.Fatal("missing default purpose")
	}
	found = false
	for _, r := range SubsystemUpdateServiceCredentialRequirements("settlement") {
		if r.Purpose == purpose {
			found = r.Rotate && r.Suffix == "reimbursement-cost" && sameStringSetValues(r.Scopes, []string{"reimbursement.cost_candidate.read"})
		}
	}
	if !found || !RequiresStrictSubsystemUpdateServiceClientValidation("settlement", purpose) {
		t.Fatal("missing strict update delivery")
	}
}

// TestReimbursementCostRelatedIdentity 验证接收端只解析同环境公开身份。
// 参数：t为测试上下文。
// 返回值：无，身份缺失或漂移未正确处理时失败。
func TestReimbursementCostRelatedIdentity(t *testing.T) {
	client := canonicalRelatedClient()
	client.ClientID = "settlement-dev-reimbursement-cost"
	client.Scopes = []string{"reimbursement.cost_candidate.read"}
	resolver := relatedResolverFor(t, &relatedRegistryStub{applicationID: client.ApplicationID, environmentID: client.EnvironmentID}, relatedClientListerStub{clients: []OAuthClientView{client}})
	got, err := resolver.resolveReimbursementCostSource(context.Background(), "tenant-1", "dev")
	if err != nil || len(got) != 1 || got[0].Client.ID != client.ID {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	client.Scopes = append(client.Scopes, "audit.ingest")
	resolver = relatedResolverFor(t, &relatedRegistryStub{applicationID: client.ApplicationID, environmentID: client.EnvironmentID}, relatedClientListerStub{clients: []OAuthClientView{client}})
	if _, err = resolver.resolveReimbursementCostSource(context.Background(), "tenant-1", "dev"); err == nil {
		t.Fatal("accepted scope drift")
	}
}
