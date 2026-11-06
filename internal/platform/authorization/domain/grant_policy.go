package domain

import "strings"

const (
	// GrantPolicyDefault 是权限的通用授予策略：按现有角色绑定规则授予，语义与
	// 引入本字段之前完全一致。存量与内置权限全部保持该取值。
	GrantPolicyDefault = "DEFAULT"
	// GrantPolicySuperAdminOnly 表示该权限所属角色只能由租户级平台超级管理员
	// （platform-super-admin）授予。它不仅限制“谁可以绑定角色”，也限制所有统一
	// 委派入口：安全管理员豁免与普通操作者的逐项持有检查都不得绕过它。
	GrantPolicySuperAdminOnly = "SUPER_ADMIN_ONLY"
)

// NormalizeGrantPolicy 把权限授予策略归一为大写去空白形式。
// 空串表示提交方未声明该字段，保持空串原样而**不**回落 DEFAULT：授权目录摘要
// 按逐字节比对，未声明与显式 DEFAULT 属于不同的目录内容；回落只发生在入库时。
func NormalizeGrantPolicy(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

// IsSupportedGrantPolicy 判断（已归一的）授予策略是否为平台认可的取值。
// 空串合法：它表示提交方未声明，入库时按 GrantPolicyDefault 处理。
func IsSupportedGrantPolicy(value string) bool {
	switch NormalizeGrantPolicy(value) {
	case "", GrantPolicyDefault, GrantPolicySuperAdminOnly:
		return true
	default:
		return false
	}
}
