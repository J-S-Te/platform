package application

import "testing"

// TestRequiresStrictSubsystemUpdateServiceClientValidation 验证严格用途仅在指定应用生效。
// 参数：t 为测试上下文。
// 返回值：无。
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
		{"settlement", ServiceCredentialNotificationIngest, true},
		{"settlement", ServiceCredentialOwnerDirectoryRead, true},
		{"settlement", ServiceCredentialAuditIngest, true},
		{"project_management", ServiceCredentialOwnerDirectoryRead, false},
		{"customer_and_opportunity", ServiceCredentialNotificationIngest, false},
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
