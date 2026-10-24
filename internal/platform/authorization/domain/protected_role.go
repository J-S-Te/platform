package domain

import "strings"

const (
	// SuperAdminRoleCode / EmergencyAdminRoleCode / BreakGlassRolePrefix 是控制面受保护角色码：
	// 只能由租户级超级管理员直接绑定，不得经由岗位授权模板、角色继承或普通角色绑定路径委派。
	SuperAdminRoleCode = "platform-super-admin"
	// EmergencyAdminRoleCode 是受保护的应急管理员角色码。
	EmergencyAdminRoleCode = "platform-emergency-admin"
	// BreakGlassRolePrefix 是受保护的紧急接管角色码前缀。
	BreakGlassRolePrefix = "platform-break-glass-"
	// SecurityAdminRoleCode 是内置平台安全管理员。它不是受保护角色，但按产品规则可以
	// 与超级管理员一样管理岗位授权模板；该豁免只用于岗位模板及其岗位映射，
	// 不扩大到普通角色绑定或应用授权入口。
	SecurityAdminRoleCode = "platform-security-admin"
)

// IsProtectedRoleCode 判断一个角色码是否属于控制面受保护角色。比较统一小写去空白，
// 与授权仓储的历史判定口径一致。
func IsProtectedRoleCode(code string) bool {
	normalized := strings.ToLower(strings.TrimSpace(code))
	return normalized == SuperAdminRoleCode || normalized == EmergencyAdminRoleCode || strings.HasPrefix(normalized, BreakGlassRolePrefix)
}
