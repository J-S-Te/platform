package application

import (
	"slices"
	"testing"
)

// TestCRMProjectHistoryCredentialLifecycle 验证首次接入和已有安装更新共用一个精确只读用途。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCRMProjectHistoryCredentialLifecycle(t *testing.T) {
	purpose := ServiceCredentialCRMProjectHistoryRead
	value, ok := integratedServicePurposeRegistry[purpose]
	if !ok || value.suffix != "crm-project-history" || value.scope != "crm.project_history.read" {
		t.Fatalf("invalid project history credential definition: %+v", value)
	}
	for _, app := range []string{integratedCustomerApplicationCode, integratedPortalApplicationCode, integratedProjectApplicationCode} {
		if got := slices.Contains(hardcodedIntegratedServicePurposes(app), purpose); got != (app == integratedCustomerApplicationCode) {
			t.Fatalf("unexpected default project history permission for %s: %v", app, got)
		}
	}
	for _, requirement := range SubsystemUpdateServiceCredentialRequirements(integratedCustomerApplicationCode) {
		if requirement.Purpose == purpose {
			if requirement.Suffix != value.suffix || !slices.Equal(requirement.Scopes, []string{value.scope}) ||
				!requirement.Rotate || !RequiresStrictSubsystemUpdateServiceClientValidation(integratedCustomerApplicationCode, purpose) {
				t.Fatalf("invalid project history update requirement: %+v", requirement)
			}
			return
		}
	}
	t.Fatal("CRM update does not deliver the project history credential")
}
