package application

import "testing"

// 严格校验判定：p1 用途一律严格；合同管理的通知摄取特例严格；其它用途按既有行为不额外严格。
func TestRequiresStrictSubsystemUpdateServiceClientValidation(t *testing.T) {
	cases := []struct {
		applicationCode string
		purpose         string
		want            bool
	}{
		{"settlement", ServiceCredentialSettlementContractIngest, true},
		{"customer_portal", ServiceCredentialPortalReportFileRead, true},
		{"customer_portal", ServiceCredentialPortalReportDownloadIdentityRead, true},
		{"contract_management", ServiceCredentialNotificationIngest, true},
		{"CONTRACT_MANAGEMENT", ServiceCredentialNotificationIngest, true},
		{"settlement", ServiceCredentialNotificationIngest, false},
		{"contract_management", ServiceCredentialSettlementContractIngest, true},
		{"data_analysis", "audit_ingest", false},
		{"", "", false},
	}
	for _, item := range cases {
		if got := RequiresStrictSubsystemUpdateServiceClientValidation(item.applicationCode, item.purpose); got != item.want {
			t.Fatalf("strict(%q, %q) = %v, want %v", item.applicationCode, item.purpose, got, item.want)
		}
	}
}
