package application

import "testing"

// 凭据需求表护栏：6 个应用都必须有需求（逐行复制的回归护栏），并记录实际条目数。
func TestSubsystemUpdateServiceCredentialRequirementsCoverage(t *testing.T) {
	codes := []string{"contract_management", "customer_and_opportunity", "customer_portal", "project_management", "settlement", "data_analysis"}
	for _, applicationCode := range codes {
		requirements := SubsystemUpdateServiceCredentialRequirements(applicationCode)
		if len(requirements) == 0 {
			t.Fatalf("%s: no requirements（疑似漏转写或该应用未被覆盖）", applicationCode)
		}
		for _, requirement := range requirements {
			if requirement.Purpose == "" || requirement.Suffix == "" || len(requirement.Scopes) == 0 || requirement.ClientName == "" {
				t.Fatalf("%s: incomplete requirement %+v", applicationCode, requirement)
			}
		}
		t.Logf("%s: %d requirements", applicationCode, len(requirements))
	}
	if got := SubsystemUpdateServiceCredentialRequirements("unknown_app"); len(got) != 0 {
		t.Fatalf("unknown application must return no requirements, got %d", len(got))
	}
	purposes := map[string]bool{}
	for _, requirement := range SubsystemUpdateServiceCredentialRequirements("contract_management") {
		purposes[requirement.Purpose] = true
	}
	for _, required := range []string{ServiceCredentialAuditIngest, ServiceCredentialFileGatewayWrite} {
		if !purposes[required] {
			t.Fatalf("contract_management missing %s", required)
		}
	}
}
