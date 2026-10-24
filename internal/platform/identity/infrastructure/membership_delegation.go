package infrastructure

import (
	"context"
	"fmt"
	"time"

	authzapplication "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/delegation"
	authzdomain "github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/domain"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"gorm.io/gorm"
)

// validateMembershipDelegation 在首次任职写入前校验新增或扩大的授权来源，避免用
// 人事管理权限绕过角色委派门禁；保留原任职或缩小其有效窗口不要求重新持有权限。
func validateMembershipDelegation(ctx context.Context, tx *gorm.DB, tenantID, operatorID string, desired membershipModel, existing *membershipModel, now time.Time) error {
	if desired.Status != domain.StatusActive || !desired.InheritAuthorization || (desired.ValidUntil != nil && !desired.ValidUntil.After(now)) {
		return nil
	}
	if existing != nil && existing.Status == domain.StatusActive && existing.InheritAuthorization && existing.UserID == desired.UserID && existing.OrgUnitID == desired.OrgUnitID && existing.PositionID == desired.PositionID && membershipWindowContains(existing.ValidFrom, existing.ValidUntil, desired.ValidFrom, desired.ValidUntil) {
		return nil
	}
	from := now
	if desired.ValidFrom != nil && desired.ValidFrom.After(from) {
		from = *desired.ValidFrom
	}
	var roles []struct{ ID, ApplicationID, Code string }
	query := tx.WithContext(ctx).Table("authz_role_binding AS binding").
		Distinct("role.id", "role.application_id", "role.code").Select("role.id, role.application_id, role.code").
		Joins("JOIN authz_role AS role ON role.id=binding.role_id AND role.tenant_id=binding.tenant_id AND role.application_id=binding.application_id AND role.status=?", domain.StatusActive).
		Joins("JOIN platform_application AS application ON application.id=role.application_id AND application.tenant_id=role.tenant_id AND application.status=?", domain.StatusActive).
		Where("binding.tenant_id=? AND binding.status=?", tenantID, domain.StatusActive).
		Where("((binding.subject_type=? AND binding.subject_id=?) OR (binding.subject_type=? AND binding.subject_id=?))", "ORG_UNIT", desired.OrgUnitID, "POSITION", desired.PositionID).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", from)
	if desired.ValidUntil != nil {
		query = query.Where("binding.valid_from IS NULL OR binding.valid_from < ?", desired.ValidUntil)
	}
	if err := query.Find(&roles).Error; err != nil {
		return fmt.Errorf("load membership inherited roles: %w", err)
	}
	for _, role := range roles {
		if err := verifyIdentityRoleDelegation(ctx, tx, tenantID, operatorID, role.ApplicationID, role.ID, role.Code); err != nil {
			return err
		}
	}
	return nil
}

func membershipWindowContains(oldFrom, oldUntil, newFrom, newUntil *time.Time) bool {
	return (oldFrom == nil || (newFrom != nil && !newFrom.Before(*oldFrom))) && (oldUntil == nil || (newUntil != nil && !newUntil.After(*oldUntil)))
}

// verifyIdentityRoleDelegation 重新解析操作者当前数据库权限；账号来源只从匹配的
// 已认证主体取值，组织/岗位来源要求有效任职。不信任过期角色声明或 worker 系统身份。
func verifyIdentityRoleDelegation(ctx context.Context, tx *gorm.DB, tenantID, operatorID, applicationID, roleID, roleCode string) error {
	accountID := delegation.OperatorAccountIDFromContext(ctx, tenantID, operatorID)
	roles, err := (&GORMRepository{database: tx}).findRoles(ctx, tenantID, operatorID, accountID, time.Now().UTC())
	if err != nil {
		return err
	}
	principal := authctx.Principal{Tenant: authctx.ReferenceName{ID: tenantID}, User: authctx.ReferenceName{ID: operatorID}, Account: authctx.ReferenceName{ID: accountID}}
	for _, role := range roles {
		principal.Roles = append(principal.Roles, authctx.ReferenceName{ID: role.ID, Code: role.Code, Name: role.Name})
	}
	trusted := authctx.WithPrincipal(ctx, principal)
	if authzdomain.IsProtectedRoleCode(roleCode) && !delegation.IsPlatformSuperAdmin(trusted, tenantID, operatorID) {
		return authzapplication.ErrForbidden
	}
	return delegation.VerifyRolePermissions(trusted, tx, tenantID, operatorID, applicationID, roleID)
}

func membershipForDelegation(input application.MembershipCreateInput, status string) membershipModel {
	return membershipModel{UserID: input.UserID, OrgUnitID: input.OrgUnitID, PositionID: input.PositionID, Status: status, InheritAuthorization: input.InheritAuthorization == nil || *input.InheritAuthorization, ValidFrom: input.EffectiveFrom, ValidUntil: input.EffectiveTo}
}
