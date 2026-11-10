package bootstrap

import "testing"

// TestReimbursementInitialRoleIsConfigurationOnly 验证接入初始角色不回退通用admin。
// 参数：t 为测试上下文。
// 返回值：无；初始角色与报销目录不符时失败。
func TestReimbursementInitialRoleIsConfigurationOnly(t *testing.T) {
	roles := hardcodedInitialSubsystemAdministratorRoles("reimbursement")
	if len(roles) != 1 || roles[0] != "reimbursement_admin" {
		t.Fatal(roles)
	}
}
