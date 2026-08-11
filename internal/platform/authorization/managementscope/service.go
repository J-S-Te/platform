// Package managementscope resolves effective organization and resource-scoped
// platform permissions for IAM management requests.
package managementscope

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	ScopeTenant   = "TENANT"
	ScopeOrgUnit  = "ORG_UNIT"
	ScopeResource = "RESOURCE"

	ResourceOrgUnit    = "ORG_UNIT"
	ResourcePosition   = "POSITION"
	ResourceMembership = "MEMBERSHIP"
)

var ErrResourceNotFound = errors.New("management authorization resource not found")

// Subject contains only server-authenticated identifiers. Callers must never
// populate these fields from request headers or request bodies.
type Subject struct {
	TenantID  string
	UserID    string
	AccountID string
}

// Scope is the effective scope for one permission. Organization IDs include
// descendants of explicitly granted organization roots.
type Scope struct {
	Unrestricted bool
	OrgUnitIDs   []string
	ResourceIDs  []string
}

func (scope Scope) Empty() bool {
	return !scope.Unrestricted && len(scope.OrgUnitIDs) == 0 && len(scope.ResourceIDs) == 0
}

func (scope Scope) Allows(orgUnitID, resourceID string) bool {
	if scope.Unrestricted {
		return true
	}
	if contains(scope.OrgUnitIDs, orgUnitID) || contains(scope.ResourceIDs, resourceID) {
		return true
	}
	return false
}

// ResourceContext is loaded from the database so authorization never trusts a
// client-supplied organization for an existing resource.
type ResourceContext struct {
	OrgUnitID  string
	ResourceID string
}

// Authorizer is consumed by the identity HTTP adapter and can be replaced by a
// deterministic fake in handler tests.
type Authorizer interface {
	Resolve(context.Context, Subject, string) (Scope, error)
	ResourceContext(context.Context, string, string, string) (ResourceContext, error)
}

// Service 直接从数据库解析当前绑定，不复用会话中的扁平权限码。组织/资源范围决策因此
// 能同时校验权限与作用域，撤销角色或任职后下一次管理请求即失效。
type Service struct{ database *gorm.DB }

func New(database *gorm.DB) (*Service, error) {
	if database == nil {
		return nil, errors.New("management scope database must not be nil")
	}
	return &Service{database: database}, nil
}

type grantRow struct {
	ScopeType string `gorm:"column:scope_type"`
	ScopeID   string `gorm:"column:scope_id"`
}

type organizationRow struct {
	ID   string `gorm:"column:id"`
	Path string `gorm:"column:path"`
}

func (service *Service) Resolve(ctx context.Context, subject Subject, permissionCode string) (Scope, error) {
	subject.TenantID = strings.TrimSpace(subject.TenantID)
	subject.UserID = strings.TrimSpace(subject.UserID)
	subject.AccountID = strings.TrimSpace(subject.AccountID)
	permissionCode = strings.TrimSpace(permissionCode)
	if subject.TenantID == "" || subject.UserID == "" || permissionCode == "" {
		return Scope{}, errors.New("management authorization subject and permission are required")
	}

	now := time.Now().UTC()
	// USER/ACCOUNT 绑定直接匹配主体；ORG_UNIT/POSITION 绑定只有在用户存在当前有效、
	// 允许继承且组织岗位仍活动的任职时才成为候选。
	var grants []grantRow
	query := service.database.WithContext(ctx).
		Table("authz_role_binding AS binding").
		Select("DISTINCT binding.scope_type, binding.scope_id").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id").
		Joins("JOIN authz_role_permission AS role_permission ON role_permission.role_id = role.id AND role_permission.effect = 'ALLOW'").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.tenant_id = binding.tenant_id AND permission.application_id = binding.application_id").
		Joins("JOIN platform_application AS application ON application.id = binding.application_id AND application.tenant_id = binding.tenant_id").
		Where("binding.tenant_id = ? AND application.code = ? AND application.status = ?", subject.TenantID, "platform", "ACTIVE").
		Where("binding.status = ? AND role.status = ? AND permission.status = ? AND permission.code = ?", "ACTIVE", "ACTIVE", "ACTIVE", permissionCode).
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now).
		Where(managementSubjectFilter(), managementSubjectArguments(subject.UserID, subject.AccountID, now)...)
	if err := query.Find(&grants).Error; err != nil {
		return Scope{}, fmt.Errorf("resolve management authorization scope: %w", err)
	}

	scope := Scope{}
	organizationRoots := make([]string, 0)
	for _, grant := range grants {
		switch strings.ToUpper(strings.TrimSpace(grant.ScopeType)) {
		case ScopeTenant:
			if strings.TrimSpace(grant.ScopeID) == "" {
				return Scope{Unrestricted: true}, nil
			}
		case ScopeOrgUnit:
			if id := strings.TrimSpace(grant.ScopeID); id != "" {
				organizationRoots = appendUnique(organizationRoots, id)
			}
		case ScopeResource:
			if id := strings.TrimSpace(grant.ScopeID); id != "" {
				scope.ResourceIDs = appendUnique(scope.ResourceIDs, id)
			}
		}
	}
	if len(organizationRoots) == 0 {
		return scope, nil
	}

	var roots []organizationRow
	if err := service.database.WithContext(ctx).Table("iam_org_unit").
		Select("id, path").
		Where("tenant_id = ? AND id IN ?", subject.TenantID, organizationRoots).
		Find(&roots).Error; err != nil {
		return Scope{}, fmt.Errorf("load granted organization roots: %w", err)
	}
	if len(roots) == 0 {
		return scope, nil
	}

	pathClauses := make([]string, 0, len(roots))
	pathArguments := make([]any, 0, len(roots))
	for _, root := range roots {
		// 组织授权根按物化路径展开到全部后代；资源直绑保持精确 ID，不随组织树扩张。
		pathClauses = append(pathClauses, "path LIKE ?")
		pathArguments = append(pathArguments, strings.TrimSpace(root.Path)+"%")
	}
	descendants := service.database.WithContext(ctx).Table("iam_org_unit").
		Select("id").
		Where("tenant_id = ?", subject.TenantID).
		Where("("+strings.Join(pathClauses, " OR ")+")", pathArguments...)
	var organizationIDs []string
	if err := descendants.Pluck("id", &organizationIDs).Error; err != nil {
		return Scope{}, fmt.Errorf("expand granted organization scopes: %w", err)
	}
	for _, id := range organizationIDs {
		scope.OrgUnitIDs = appendUnique(scope.OrgUnitIDs, id)
	}
	return scope, nil
}

func managementSubjectFilter() string {
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
				 AND position.org_unit_id = membership.org_unit_id
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
	)`
}

func managementSubjectArguments(userID, accountID string, now time.Time) []any {
	return []any{
		"USER", userID,
		"ACCOUNT", accountID,
		"ORG_UNIT", "POSITION",
		"ACTIVE", "ACTIVE",
		userID, "ACTIVE", now, now,
		"ORG_UNIT", "POSITION",
	}
}

func (service *Service) ResourceContext(ctx context.Context, tenantID, resourceType, resourceID string) (ResourceContext, error) {
	// 资源归属必须由服务端按租户从数据库读取。调用方传入的组织字段只能用于定位，
	// 不能直接参与 Scope.Allows，否则可伪造 org_unit_id 绕过范围限制。
	tenantID = strings.TrimSpace(tenantID)
	resourceID = strings.TrimSpace(resourceID)
	if tenantID == "" || resourceID == "" {
		return ResourceContext{}, ErrResourceNotFound
	}
	switch strings.ToUpper(strings.TrimSpace(resourceType)) {
	case ResourceOrgUnit:
		var row struct {
			ID string `gorm:"column:id"`
		}
		result := service.database.WithContext(ctx).Table("iam_org_unit").Select("id").Where("tenant_id = ? AND id = ?", tenantID, resourceID).Take(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return ResourceContext{}, ErrResourceNotFound
		}
		if result.Error != nil {
			return ResourceContext{}, fmt.Errorf("load organization authorization context: %w", result.Error)
		}
		return ResourceContext{OrgUnitID: row.ID, ResourceID: row.ID}, nil
	case ResourcePosition:
		var row struct {
			ID        string `gorm:"column:id"`
			OrgUnitID string `gorm:"column:org_unit_id"`
		}
		result := service.database.WithContext(ctx).Table("iam_position").Select("id, org_unit_id").Where("tenant_id = ? AND id = ?", tenantID, resourceID).Take(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return ResourceContext{}, ErrResourceNotFound
		}
		if result.Error != nil {
			return ResourceContext{}, fmt.Errorf("load position authorization context: %w", result.Error)
		}
		return ResourceContext{OrgUnitID: row.OrgUnitID, ResourceID: row.ID}, nil
	case ResourceMembership:
		var row struct {
			ID        string `gorm:"column:id"`
			OrgUnitID string `gorm:"column:org_unit_id"`
		}
		result := service.database.WithContext(ctx).Table("iam_membership").Select("id, org_unit_id").Where("tenant_id = ? AND id = ?", tenantID, resourceID).Take(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return ResourceContext{}, ErrResourceNotFound
		}
		if result.Error != nil {
			return ResourceContext{}, fmt.Errorf("load membership authorization context: %w", result.Error)
		}
		return ResourceContext{OrgUnitID: row.OrgUnitID, ResourceID: row.ID}, nil
	default:
		return ResourceContext{}, errors.New("unsupported management authorization resource type")
	}
}

func contains(values []string, expected string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return false
	}
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	if value == "" || contains(values, value) {
		return values
	}
	return append(values, value)
}
