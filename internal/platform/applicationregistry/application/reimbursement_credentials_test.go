package application

import (
	"slices"
	"testing"
	"time"
)

// TestReimbursementPurposeLifecycle 验证首次与更新重试均交付独立项目用途及精确文件范围。
// 参数：t 为测试上下文。
// 返回值：无；任一用途丢失、复用或权限过宽时失败。
func TestReimbursementPurposeLifecycle(t *testing.T) {
	service, err := NewSubsystemOnboardingService(&subsystemOnboardingRepositoryStub{}, &sequentialManagementIDs{}, fixedSubsystemClock{now: time.Now()}, RedirectURIValidationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	writes, secrets, err := service.buildIntegratedServiceClients(SubsystemOnboardingInput{TenantID: "tenant", OperatorID: "operator", ApplicationCode: "reimbursement", ApplicationName: "报销", Environment: "prod"}, "app", "env", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string][]string{"reimbursement_project_reference_read": {"reimbursement.project_reference.read"}, "project_reimbursement_cost_write": {"project.reimbursement_cost.write"}, "file_gateway_write": {"platform:file:upload", "platform:file:bind", "platform:file:download"}, "owner_directory_read": {"owner_directory.read"}, "notification_ingest": {"notification.ingest"}, "audit_ingest": {"audit.ingest"}}
	for purpose, scopes := range expected {
		found := false
		for _, w := range writes {
			if w.Purpose == purpose {
				found = true
				if !sameStringSetValues(w.OAuthClient.Scopes, scopes) || secrets[purpose] == "" {
					t.Fatal(purpose, w.OAuthClient.Scopes)
				}
			}
		}
		if !found {
			t.Fatalf("missing initial purpose %s", purpose)
		}
	}
	requirements := SubsystemUpdateServiceCredentialRequirements("reimbursement")
	for purpose := range expected {
		found := false
		for _, r := range requirements {
			if r.Purpose == purpose {
				found = true
				if !RequiresStrictSubsystemUpdateServiceClientValidation("reimbursement", purpose) || !sameStringSetValues(SubsystemUpdateServiceCredentialScopes(r), expected[purpose]) {
					t.Fatal("update drift", purpose)
				}
			}
		}
		if !found {
			t.Fatal("missing update", purpose)
		}
	}
	if !slices.Contains(hardcodedIntegratedServicePurposes("reimbursement"), "reimbursement_project_reference_read") {
		t.Fatal("missing default")
	}
}
