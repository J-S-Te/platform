package domain

import "strings"

const (
	// SuperAdminRoleCode / EmergencyAdminRoleCode / BreakGlassRolePrefix 是控制面受保护角色码：
	// 只能由租户级超级管理员直接绑定，不得经由岗位授权模板、角色继承或普通角色绑定路径委派。
	SuperAdminRoleCode     = "platform-super-admin"
	EmergencyAdminRoleCode = "platform-emergency-admin"
	BreakGlassRolePrefix   = "platform-break-glass-"
)

// IsProtectedRoleCode 判断一个角色码是否属于控制面受保护角色。比较统一小写去空白，
// 与授权仓储的历史判定口径一致。
func IsProtectedRoleCode(code string) bool {
	normalized := strings.ToLower(strings.TrimSpace(code))
	return normalized == SuperAdminRoleCode || normalized == EmergencyAdminRoleCode || strings.HasPrefix(normalized, BreakGlassRolePrefix)
}
