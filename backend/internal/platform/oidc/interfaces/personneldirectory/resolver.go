// Package personneldirectory 向 OIDC UserInfo 提供租户内活跃人员的最小投影；
// 登录名、工号、邮箱和手机号均不进入该目录，避免 profile scope 扩大为账号目录读取权。
package personneldirectory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	oidchttp "github.com/J-S-Te/Basic-Platform/backend/internal/platform/oidc/interfaces/http"
	"gorm.io/gorm"
)

type Resolver struct {
	database *gorm.DB
}

func New(database *gorm.DB) (*Resolver, error) {
	if database == nil {
		return nil, errors.New("personnel directory database must not be nil")
	}
	return &Resolver{database: database}, nil
}

type roleRow struct {
	UserID   string `gorm:"column:user_id"`
	RoleCode string `gorm:"column:role_code"`
}

func (resolver *Resolver) ListActivePersonnel(ctx context.Context, tenantID, oauthClientID string) ([]oidchttp.PersonnelDirectoryEntry, error) {
	tenantID, oauthClientID = strings.TrimSpace(tenantID), strings.TrimSpace(oauthClientID)
	if resolver == nil || resolver.database == nil || tenantID == "" || oauthClientID == "" {
		return nil, errors.New("personnel directory resolver is not initialized")
	}
	entries := make([]oidchttp.PersonnelDirectoryEntry, 0)
	if err := resolver.database.WithContext(ctx).
		Table("iam_user").
		Select("id AS user_id", "display_name").
		Where("tenant_id = ? AND status = ? AND display_name <> ''", tenantID, "ACTIVE").
		Order("display_name ASC, id ASC").
		Find(&entries).Error; err != nil {
		return nil, fmt.Errorf("list active tenant personnel: %w", err)
	}
	if len(entries) == 0 {
		return entries, nil
	}

	now := time.Now().UTC()
	// 应用角色必须与 OAuth 客户端所属应用一致，且只承认 TENANT/当前 ENVIRONMENT 范围；
	// 组织和岗位绑定还要求有效任职关系显式开启 inherit_authorization。
	roleRows := make([]roleRow, 0)
	err := resolver.database.WithContext(ctx).
		Table("iam_user AS person").
		Select("DISTINCT person.id AS user_id, role.code AS role_code").
		Joins("JOIN platform_oauth_client AS client ON client.id = ? AND client.tenant_id = person.tenant_id AND client.status = ?", oauthClientID, "ACTIVE").
		Joins("JOIN authz_role_binding AS binding ON binding.tenant_id = person.tenant_id AND binding.application_id = client.application_id AND binding.status = ?", "ACTIVE").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ? AND role.role_type = ?", "ACTIVE", "APPLICATION").
		Where("person.tenant_id = ? AND person.status = ?", tenantID, "ACTIVE").
		Where("(binding.valid_from IS NULL OR binding.valid_from <= ?) AND (binding.valid_until IS NULL OR binding.valid_until > ?)", now, now).
		Where("((binding.scope_type = ? AND binding.scope_id = '') OR (binding.scope_type = ? AND binding.scope_id = client.environment_id))", "TENANT", "ENVIRONMENT").
		Where(`(
			(binding.subject_type = 'USER' AND binding.subject_id = person.id)
			OR (
				binding.subject_type IN ('ORG_UNIT', 'POSITION')
				AND EXISTS (
					SELECT 1
					FROM iam_membership AS membership
					JOIN iam_org_unit AS organization
						ON organization.id = membership.org_unit_id
						AND organization.tenant_id = membership.tenant_id
						AND organization.status = 'ACTIVE'
					JOIN iam_position AS position
						ON position.id = membership.position_id
						AND position.tenant_id = membership.tenant_id
						AND position.org_unit_id = membership.org_unit_id
						AND position.status = 'ACTIVE'
					WHERE membership.tenant_id = binding.tenant_id
						AND membership.user_id = person.id
						AND membership.status = 'ACTIVE'
						AND membership.inherit_authorization = TRUE
						AND (membership.valid_from IS NULL OR membership.valid_from <= ?)
						AND (membership.valid_until IS NULL OR membership.valid_until > ?)
						AND (
							(binding.subject_type = 'ORG_UNIT' AND membership.org_unit_id = binding.subject_id)
							OR (binding.subject_type = 'POSITION' AND membership.position_id = binding.subject_id)
						)
				)
			)
		)`, now, now).
		Order("person.id ASC, role.code ASC").
		Find(&roleRows).Error
	if err != nil {
		return nil, fmt.Errorf("list effective application roles for tenant personnel: %w", err)
	}
	superAdminRows := make([]roleRow, 0)
	// 平台超级管理员映射为目标应用的 admin 角色，而不是把平台角色码直接泄露给子系统；
	// 目标应用必须已登记同名 admin 角色，否则不会产生隐式权限。
	err = resolver.database.WithContext(ctx).
		Table("iam_user AS person").
		Select("DISTINCT person.id AS user_id, target_admin.code AS role_code").
		Joins("JOIN platform_oauth_client AS client ON client.id = ? AND client.tenant_id = person.tenant_id AND client.status = ?", oauthClientID, "ACTIVE").
		Joins("JOIN authz_role AS target_admin ON target_admin.tenant_id = person.tenant_id AND target_admin.application_id = client.application_id AND target_admin.code = ? AND target_admin.status = ?", "admin", "ACTIVE").
		Joins("JOIN platform_application AS platform_app ON platform_app.tenant_id = person.tenant_id AND platform_app.code = ? AND platform_app.status = ?", "platform", "ACTIVE").
		Joins("JOIN authz_role AS platform_role ON platform_role.tenant_id = person.tenant_id AND platform_role.application_id = platform_app.id AND platform_role.code = ? AND platform_role.status = ?", "platform-super-admin", "ACTIVE").
		Joins("JOIN authz_role_binding AS binding ON binding.tenant_id = person.tenant_id AND binding.application_id = platform_app.id AND binding.role_id = platform_role.id AND binding.subject_type = ? AND binding.subject_id = person.id AND binding.scope_type = ? AND binding.scope_id = '' AND binding.status = ?", "USER", "TENANT", "ACTIVE").
		Where("person.tenant_id = ? AND person.status = ?", tenantID, "ACTIVE").
		Where("(binding.valid_from IS NULL OR binding.valid_from <= ?) AND (binding.valid_until IS NULL OR binding.valid_until > ?)", now, now).
		Order("person.id ASC").
		Find(&superAdminRows).Error
	if err != nil {
		return nil, fmt.Errorf("list inherited subsystem administrators: %w", err)
	}
	roleRows = append(roleRows, superAdminRows...)

	attachRoles(entries, roleRows)
	return entries, nil
}

func attachRoles(entries []oidchttp.PersonnelDirectoryEntry, roleRows []roleRow) {
	index := make(map[string]int, len(entries))
	for position := range entries {
		entries[position].Roles = []string{}
		index[entries[position].UserID] = position
	}
	seenRoles := make(map[string]bool, len(roleRows))
	for _, row := range roleRows {
		position, exists := index[row.UserID]
		key := row.UserID + "\x00" + row.RoleCode
		if !exists || row.RoleCode == "" || seenRoles[key] {
			continue
		}
		seenRoles[key] = true
		entries[position].Roles = append(entries[position].Roles, row.RoleCode)
	}
	for position := range entries {
		sort.Strings(entries[position].Roles)
	}
}

var _ oidchttp.PersonnelDirectoryResolver = (*Resolver)(nil)
