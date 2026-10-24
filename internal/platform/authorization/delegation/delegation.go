// Package delegation 提炼"操作者只能委派本人当前已有权限"这一条授权边界。
//
// 角色绑定、自定义角色和岗位授权模板都必须走同一份实现：允许管理绑定或模板不等于
// 允许把本人并不拥有的权限授予他人（或授予自己继承得到的岗位）。
package delegation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/gorm"
)

const (
	activeStatus    = "ACTIVE"
	tenantScopeType = "TENANT"
)

// delegationBypass 定义哪些已认证控制面角色可以委派本人并未逐项持有的权限。
// 零值不放行任何角色；不同调用入口必须使用各自明确选择的策略，不能用“超级管理员
// 例外”顺手覆盖安全管理员。
type delegationBypass struct {
	superAdmin    bool
	securityAdmin bool
}

var (
	// standardDelegationBypass 用于普通角色绑定、应用授权等入口：只保留超级管理员例外。
	standardDelegationBypass = delegationBypass{superAdmin: true}
	// positionTemplateDelegationBypass 额外允许内置平台安全管理员管理岗位授权模板及其
	// 岗位映射。该策略只用于 positiongrant；不得复用到普通角色绑定或应用授权入口。
	positionTemplateDelegationBypass = delegationBypass{superAdmin: true, securityAdmin: true}
)

// OperatorAccountIDFromContext 只使用认证中间件装入的主体。没有已认证主体的直接调用
// 只能按用户与继承的组织/岗位授予计算，绝不信任调用方自报的账号标识。
func OperatorAccountIDFromContext(ctx context.Context, tenantID, operatorID string) string {
	principal, ok := authctx.PrincipalFromContext(ctx)
	if !ok || principal.Tenant.ID != tenantID || principal.User.ID != operatorID {
		return ""
	}
	return strings.TrimSpace(principal.Account.ID)
}

// VerifyRolePermissions 把可委派边界应用到一个角色包含的全部权限。管理绑定或模板
// 本身不等于有权授予既有角色恰好包含的全部权限。该入口保留标准策略：只有平台超级
// 管理员例外，安全管理员仍需逐项持有目标角色权限。
func VerifyRolePermissions(ctx context.Context, tx *gorm.DB, tenantID, operatorID, applicationID, roleID string) error {
	return verifyRolePermissions(ctx, tx, tenantID, operatorID, applicationID, roleID, standardDelegationBypass)
}

// VerifyRolePermissionsForPositionTemplates 是岗位授权模板及其岗位映射专用的委派边界。
// 平台安全管理员按产品规则与超级管理员一样可以管理岗位模板，因此允许其委派本人未
// 逐项持有的子系统角色；普通角色绑定、应用授权和自定义角色路径不得使用本入口。
func VerifyRolePermissionsForPositionTemplates(ctx context.Context, tx *gorm.DB, tenantID, operatorID, applicationID, roleID string) error {
	return verifyRolePermissions(ctx, tx, tenantID, operatorID, applicationID, roleID, positionTemplateDelegationBypass)
}

func verifyRolePermissions(ctx context.Context, tx *gorm.DB, tenantID, operatorID, applicationID, roleID string, bypass delegationBypass) error {
	var permissionIDs []string
	result := tx.WithContext(ctx).Table("authz_role_permission AS role_permission").
		Distinct("role_permission.permission_id").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.tenant_id = ? AND permission.application_id = ? AND permission.status = ?", tenantID, applicationID, activeStatus).
		Where("role_permission.role_id = ? AND role_permission.effect = ?", roleID, "ALLOW").
		Pluck("role_permission.permission_id", &permissionIDs)
	if result.Error != nil {
		return fmt.Errorf("load role permissions for delegation: %w", result.Error)
	}
	if len(permissionIDs) == 0 {
		return nil
	}
	// 平台超级管理员是租户控制面最高权限身份。其有效应用角色由
	// applicationaccess 以只读投影展开，并不会逐个应用写入普通绑定；如果这里仍按
	// 实际绑定判断，会把“超管看得到全部应用，却无法配置岗位标准模板”变成永久缺口。
	// 平台安全管理员只是在岗位模板专用策略中享有同样的委派来源豁免；受保护平台角色
	// 仍由各调用方的目标角色检查拦截，本函数只放宽委派来源判定。
	if bypass.allows(ctx, tenantID, operatorID) {
		return nil
	}
	return VerifyPermissions(ctx, tx, tenantID, operatorID, OperatorAccountIDFromContext(ctx, tenantID, operatorID), applicationID, permissionIDs)
}

func (bypass delegationBypass) allows(ctx context.Context, tenantID, operatorID string) bool {
	if bypass.superAdmin && IsPlatformSuperAdmin(ctx, tenantID, operatorID) {
		return true
	}
	return bypass.securityAdmin && IsPlatformSecurityAdmin(ctx, tenantID, operatorID)
}

// IsPlatformSuperAdmin 只认可认证中间件装入的租户级 platform-super-admin 主体。
// 不按显示名、应用角色名或调用方自报字段猜测，也不额外查询数据库；这与
// applicationaccess 的超管投影保持同一身份来源，但不写入任何业务应用绑定。
func IsPlatformSuperAdmin(ctx context.Context, tenantID, operatorID string) bool {
	principal, ok := authctx.PrincipalFromContext(ctx)
	if !ok || principal.Tenant.ID != tenantID || principal.User.ID != operatorID {
		return false
	}
	for _, role := range principal.Roles {
		if role.Code == domain.SuperAdminRoleCode {
			return true
		}
	}
	return false
}

// IsPlatformSecurityAdmin 只认可认证中间件装入的租户级 platform-security-admin 主体。
// 与 IsPlatformSuperAdmin 使用同一身份来源，不查询数据库或信任请求字段。
func IsPlatformSecurityAdmin(ctx context.Context, tenantID, operatorID string) bool {
	principal, ok := authctx.PrincipalFromContext(ctx)
	if !ok || principal.Tenant.ID != tenantID || principal.User.ID != operatorID {
		return false
	}
	for _, role := range principal.Roles {
		if role.Code == domain.SecurityAdminRoleCode {
			return true
		}
	}
	return false
}

// VerifyPermissions 确保所选权限全部是当前已认证操作者在租户范围内的有效权限。
func VerifyPermissions(ctx context.Context, tx *gorm.DB, tenantID, operatorID, operatorAccountID, applicationID string, permissionIDs []string) error {
	if len(permissionIDs) == 0 {
		return nil
	}

	now := time.Now().UTC()
	subjectSQL, subjectArgs := SubjectFilter(operatorID, operatorAccountID, now)
	var total int64
	result := tx.WithContext(ctx).Table("authz_role_binding AS binding").
		Distinct("permission.id").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ?", activeStatus).
		Joins("JOIN authz_role_permission AS role_permission ON role_permission.role_id = role.id AND role_permission.effect = ?", "ALLOW").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.tenant_id = binding.tenant_id AND permission.application_id = binding.application_id AND permission.status = ?", activeStatus).
		Where("binding.tenant_id = ? AND binding.application_id = ? AND binding.status = ?", tenantID, applicationID, activeStatus).
		Where("binding.scope_type = ? AND binding.scope_id = ?", tenantScopeType, "").
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now).
		Where("permission.id IN ?", permissionIDs).
		Where(subjectSQL, subjectArgs...).
		Count(&total)
	if result.Error != nil {
		return fmt.Errorf("verify delegable permissions: %w", result.Error)
	}
	if total != int64(len(permissionIDs)) {
		return application.ErrForbidden
	}
	return nil
}

// SubjectFilter 解析有效的角色主体。组织与岗位绑定只在请求用户对相应在册组织单元
// 或岗位存在当前有效的任职时才生效。
func SubjectFilter(userID, accountID string, now time.Time) (string, []any) {
	// 组织/岗位角色不会被提前摊平成会话权限：每次决策都要求存在当前有效任职，且
	// 任职明确允许继承授权。停用组织、岗位或任职后，下一次请求立即失去该来源。
	return `(
		(binding.subject_type = ? AND binding.subject_id = ?)
		OR (binding.subject_type = ? AND binding.subject_id = ?)
		OR (
			binding.subject_type IN (?, ?)
			AND EXISTS (
				SELECT 1
				FROM iam_membership AS membership
				JOIN iam_org_unit AS organization
					ON organization.id = membership.org_unit_id
					AND organization.tenant_id = membership.tenant_id
					AND organization.status = ?
				JOIN iam_position AS position
					ON position.id = membership.position_id
					AND position.tenant_id = membership.tenant_id
					AND position.status = ?
				WHERE membership.tenant_id = binding.tenant_id
					AND membership.user_id = ?
					AND membership.status = ?
					AND membership.inherit_authorization = 1
					AND (membership.valid_from IS NULL OR membership.valid_from <= ?)
					AND (membership.valid_until IS NULL OR membership.valid_until > ?)
					AND (
						(binding.subject_type = ? AND membership.org_unit_id = binding.subject_id)
						OR (binding.subject_type = ? AND membership.position_id = binding.subject_id)
					)
			)
		)
	)`, []any{
			"USER", userID,
			"ACCOUNT", accountID,
			"ORG_UNIT", "POSITION",
			"ACTIVE", "ACTIVE",
			userID, "ACTIVE", now, now,
			"ORG_UNIT", "POSITION",
		}
}
