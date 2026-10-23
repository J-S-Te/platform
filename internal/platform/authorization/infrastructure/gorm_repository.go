package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/delegation"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"gorm.io/gorm"
)

const (
	platformApplicationCode = "platform"
)

// GORMRepository 将 RBAC 聚合持久化到 SQL 迁移拥有的表中。有意不调用 AutoMigrate，
// 使模式演进历史保持显式、可审查。
type GORMRepository struct{ database *gorm.DB }

// NewGORMRepository 校验数据库依赖非空后构造授权仓储，缺失依赖时失败关闭。
func NewGORMRepository(database *gorm.DB) (*GORMRepository, error) {
	if database == nil {
		return nil, errors.New("authorization GORM database must not be nil")
	}
	return &GORMRepository{database: database}, nil
}

// ListResources 分页返回租户内资源，支持关键字与状态过滤。
func (r *GORMRepository) ListResources(ctx context.Context, tenantID string, page application.PageRequest) (application.PageResult[domain.Resource], error) {
	query := r.database.WithContext(ctx).Table("authz_resource AS resource").Joins("JOIN platform_application AS application ON application.id = resource.application_id").Where("resource.tenant_id = ?", tenantID)
	if page.Keyword != "" {
		query = query.Where("resource.code LIKE ? OR resource.name LIKE ?", like(page.Keyword), like(page.Keyword))
	}
	if page.Status != "" {
		query = query.Where("resource.status = ?", page.Status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return application.PageResult[domain.Resource]{}, fmt.Errorf("count resources: %w", err)
	}
	var rows []resourceProjection
	result := query.Select("resource.id, application.code AS application_code, resource.code, resource.name, resource.resource_type, resource.version").Order("resource.created_at DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Scan(&rows)
	if result.Error != nil {
		return application.PageResult[domain.Resource]{}, fmt.Errorf("list resources: %w", result.Error)
	}
	items := make([]domain.Resource, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.Resource{ID: row.ID, ApplicationCode: row.ApplicationCode, Code: row.Code, Name: row.Name, ResourceType: row.ResourceType, Version: row.Version})
	}
	return application.PageResult[domain.Resource]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}

// CreateResource 校验应用有效且目录可写后创建资源。
func (r *GORMRepository) CreateResource(ctx context.Context, tenantID, operatorID string, resource domain.Resource) (domain.Resource, error) {
	app, err := r.activeApplication(ctx, tenantID, resource.ApplicationCode)
	if err != nil {
		return domain.Resource{}, err
	}
	if err := r.ensureApplicationCatalogWritable(ctx, tenantID, app.ID); err != nil {
		return domain.Resource{}, err
	}
	now := time.Now().UTC()
	operator := operatorID
	model := resourceModel{ID: resource.ID, TenantID: tenantID, ApplicationID: app.ID, Code: resource.Code, Name: resource.Name, ResourceType: resource.ResourceType, Status: domain.StatusActive, Version: 1, CreatedAt: now, CreatedBy: &operator, UpdatedAt: now, UpdatedBy: &operator}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.Resource{}, translateError(err, "create resource")
	}
	resource.Version = model.Version
	return resource, nil
}

// ListPermissions 分页返回租户内权限点，附带所属资源信息。
func (r *GORMRepository) ListPermissions(ctx context.Context, tenantID string, page application.PageRequest) (application.PageResult[domain.Permission], error) {
	query := r.database.WithContext(ctx).Table("authz_permission AS permission").Joins("JOIN authz_resource AS resource ON resource.id = permission.resource_id").Where("permission.tenant_id = ?", tenantID)
	if page.Keyword != "" {
		query = query.Where("permission.code LIKE ? OR permission.name LIKE ?", like(page.Keyword), like(page.Keyword))
	}
	if page.Status != "" {
		query = query.Where("permission.status = ?", page.Status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return application.PageResult[domain.Permission]{}, fmt.Errorf("count permissions: %w", err)
	}
	var rows []permissionProjection
	result := query.Select("permission.id, permission.code, permission.name, permission.action, permission.version, resource.id AS resource_id, resource.code AS resource_code, resource.name AS resource_name").Order("permission.created_at DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Scan(&rows)
	if result.Error != nil {
		return application.PageResult[domain.Permission]{}, fmt.Errorf("list permissions: %w", result.Error)
	}
	items := make([]domain.Permission, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.Permission{ID: row.ID, Code: row.Code, Name: row.Name, Action: row.Action, Version: row.Version, Resource: domain.Reference{ID: row.ResourceID, Code: row.ResourceCode, Name: row.ResourceName}})
	}
	return application.PageResult[domain.Permission]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}

// CreatePermission 要求资源有效且应用目录可写后才创建权限点。
func (r *GORMRepository) CreatePermission(ctx context.Context, tenantID, operatorID string, permission domain.Permission) (domain.Permission, error) {
	var resource resourceModel
	result := r.database.WithContext(ctx).Where("id = ? AND tenant_id = ? AND status = ?", permission.Resource.ID, tenantID, domain.StatusActive).First(&resource)
	if result.Error != nil {
		return domain.Permission{}, translateNotFound(result.Error, "resource")
	}
	if err := r.ensureApplicationCatalogWritable(ctx, tenantID, resource.ApplicationID); err != nil {
		return domain.Permission{}, err
	}
	now := time.Now().UTC()
	operator := operatorID
	model := permissionModel{ID: permission.ID, TenantID: tenantID, ApplicationID: resource.ApplicationID, ResourceID: resource.ID, Code: permission.Code, Action: permission.Action, Name: permission.Name, Status: domain.StatusActive, Version: 1, CreatedAt: now, CreatedBy: &operator, UpdatedAt: now, UpdatedBy: &operator}
	if err := r.database.WithContext(ctx).Create(&model).Error; err != nil {
		return domain.Permission{}, translateError(err, "create permission")
	}
	permission.Version = 1
	permission.Resource = domain.Reference{ID: resource.ID, Code: resource.Code, Name: resource.Name}
	return permission, nil
}

// roleListQuery 构造平台角色列表查询（供列表与定向回归共用）。
//
// 关键字、状态和排序必须带 authz_role 限定：本查询 JOIN 了 platform_application，
// 两张表都有 code/name/status/created_at，未限定列名会被 MySQL 判为 ambiguous。
func roleListQuery(database *gorm.DB, tenantID string, page application.PageRequest) *gorm.DB {
	query := database.Model(&roleModel{}).
		Joins("JOIN platform_application AS application ON application.id = authz_role.application_id AND application.tenant_id = authz_role.tenant_id").
		Where("authz_role.tenant_id = ? AND application.code = ?", tenantID, platformApplicationCode)
	if page.Keyword != "" {
		query = query.Where("authz_role.code LIKE ? OR authz_role.name LIKE ?", like(page.Keyword), like(page.Keyword))
	}
	if page.Status != "" {
		query = query.Where("authz_role.status = ?", page.Status)
	}
	return query
}

func (r *GORMRepository) ListRoles(ctx context.Context, tenantID string, page application.PageRequest) (application.PageResult[domain.Role], error) {
	query := roleListQuery(r.database.WithContext(ctx), tenantID, page)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return application.PageResult[domain.Role]{}, fmt.Errorf("count roles: %w", err)
	}
	var rows []roleModel
	if err := query.Order("authz_role.created_at DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Find(&rows).Error; err != nil {
		return application.PageResult[domain.Role]{}, fmt.Errorf("list roles: %w", err)
	}
	items := make([]domain.Role, 0, len(rows))
	for _, row := range rows {
		role, err := r.roleWithPermissions(ctx, row)
		if err != nil {
			return application.PageResult[domain.Role]{}, err
		}
		items = append(items, role)
	}
	return application.PageResult[domain.Role]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}

// CreateRole 在单事务内校验权限存在性与操作者可委派集合后创建自定义角色，
// 并推进应用 revision 触发下游投影刷新。
func (r *GORMRepository) CreateRole(ctx context.Context, tenantID, operatorID, operatorAccountID string, role domain.Role, permissionIDs []string) (domain.Role, error) {
	returnRole := domain.Role{}
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		app, err := r.activeApplicationTx(tx, tenantID, platformApplicationCode)
		if err != nil {
			return err
		}
		if err := r.verifyPermissions(tx, tenantID, app.ID, permissionIDs); err != nil {
			return err
		}
		// “拥有角色管理权限”不等于“可以授予平台全部权限”。在创建角色和权限关系前，
		// 以操作者当前有效绑定重新计算可委派集合，整个检查与写入共享同一事务快照。
		if err := r.verifyDelegablePermissions(tx, tenantID, operatorID, operatorAccountID, app.ID, permissionIDs); err != nil {
			return err
		}
		now := time.Now().UTC()
		operator := operatorID
		model := roleModel{ID: role.ID, TenantID: tenantID, ApplicationID: app.ID, Code: role.Code, Name: role.Name, RoleType: "CUSTOM", Description: role.Description, Status: role.Status, Version: 1, CreatedAt: now, CreatedBy: &operator, UpdatedAt: now, UpdatedBy: &operator}
		if err := tx.Create(&model).Error; err != nil {
			return translateError(err, "create role")
		}
		if err := replacePermissions(tx, role.ID, operatorID, now, permissionIDs); err != nil {
			return err
		}
		if err := r.bumpRevision(tx, tenantID, app.ID, now, "role created"); err != nil {
			return err
		}
		resolvedRole, err := r.roleWithPermissionsTx(tx, model)
		if err != nil {
			return err
		}
		returnRole = resolvedRole
		return nil
	})
	return returnRole, err
}
// GetRole 返回角色及其权限集合；不存在时返回未找到。
func (r *GORMRepository) GetRole(ctx context.Context, tenantID, roleID string) (domain.Role, error) {
	var model roleModel
	result := r.database.WithContext(ctx).Where("id = ? AND tenant_id = ?", roleID, tenantID).First(&model)
	if result.Error != nil {
		return domain.Role{}, translateNotFound(result.Error, "role")
	}
	return r.roleWithPermissions(ctx, model)
}

// UpdateRole 在乐观锁与可编辑性校验后整体替换角色属性与权限集合；
// 扩权风险通过按本次提交集合重算可委派校验来阻断。
func (r *GORMRepository) UpdateRole(ctx context.Context, tenantID, operatorID, operatorAccountID string, role domain.Role, permissionIDs []string) (domain.Role, error) {
	var output domain.Role
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var model roleModel
		result := tx.Where("id = ? AND tenant_id = ?", role.ID, tenantID).First(&model)
		if result.Error != nil {
			return translateNotFound(result.Error, "role")
		}
		if err := ensureRoleEditable(model); err != nil {
			return err
		}
		if model.Version != role.Version {
			return application.ErrVersionConflict
		}
		// 更新既有角色同样可能扩权，因此不能复用旧角色权限或只验证权限存在；必须按
		// 本次提交的完整权限集合重新执行可委派校验。
		if err := r.verifyPermissions(tx, tenantID, model.ApplicationID, permissionIDs); err != nil {
			return err
		}
		if err := r.verifyDelegablePermissions(tx, tenantID, operatorID, operatorAccountID, model.ApplicationID, permissionIDs); err != nil {
			return err
		}
		now := time.Now().UTC()
		result = tx.Model(&roleModel{}).Where("id = ? AND version = ?", role.ID, role.Version).Updates(map[string]any{"name": role.Name, "description": role.Description, "status": role.Status, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": operatorID})
		if result.Error != nil {
			return fmt.Errorf("update role: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}
		if err := replacePermissions(tx, role.ID, operatorID, now, permissionIDs); err != nil {
			return err
		}
		if err := r.bumpRevision(tx, tenantID, model.ApplicationID, now, "role updated"); err != nil {
			return err
		}
		var updated roleModel
		if err := tx.Where("id = ?", role.ID).First(&updated).Error; err != nil {
			return fmt.Errorf("reload role: %w", err)
		}
		resolvedRole, err := r.roleWithPermissionsTx(tx, updated)
		if err != nil {
			return err
		}
		output = resolvedRole
		return nil
	})
	return output, err
}

// ensureRoleEditable 维持角色的归属边界：内置平台角色与应用自定义角色都不能经
// 通用角色管理 API 修改；应用角色必须由归属子系统通过目录同步流程变更。
func ensureRoleEditable(role roleModel) error {
	if role.BuiltIn || strings.EqualFold(strings.TrimSpace(role.RoleType), "APPLICATION") {
		return application.ErrConflict
	}
	return nil
}

// ensureApplicationRoleBindingManaged 使应用目录角色只能走应用授权入口——那里强制
// 授予来源与有效角色上限。通用 RBAC 绑定入口仅保留给平台角色。
func ensureApplicationRoleBindingManaged(role roleModel) error {
	if strings.EqualFold(strings.TrimSpace(role.RoleType), "APPLICATION") {
		return application.ErrConflict
	}
	return nil
}

// catalogMirrorReadOnly 把一次成功同步过的目录视为应用所有。保留的 version/hash
// 也会让后续一次失败的重新同步之后目录仍保持只读：同步失败绝不能重新打开目录
// 给控制台改写。
func catalogMirrorReadOnly(syncStatus, catalogVersion, catalogHash string) bool {
	return strings.EqualFold(strings.TrimSpace(syncStatus), "SYNCED") ||
		strings.TrimSpace(catalogVersion) != "" || strings.TrimSpace(catalogHash) != ""
}

func (r *GORMRepository) ensureApplicationCatalogWritable(ctx context.Context, tenantID, applicationID string) error {
	var metadata struct {
		SyncStatus     string `gorm:"column:sync_status"`
		CatalogVersion string `gorm:"column:catalog_version"`
		CatalogHash    string `gorm:"column:catalog_hash"`
	}
	err := r.database.WithContext(ctx).Table("authz_authorization_catalog").
		Select("sync_status, catalog_version, catalog_hash").
		Where("tenant_id = ? AND application_id = ?", tenantID, applicationID).Take(&metadata).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load authorization catalog ownership: %w", err)
	}
	if catalogMirrorReadOnly(metadata.SyncStatus, metadata.CatalogVersion, metadata.CatalogHash) {
		return application.ErrConflict
	}
	return nil
}

// ListRoleBindings 分页返回租户内平台角色绑定，支持关键字与状态过滤。
func (r *GORMRepository) ListRoleBindings(ctx context.Context, tenantID string, page application.PageRequest) (application.PageResult[domain.RoleBinding], error) {
	query := r.database.WithContext(ctx).Table("authz_role_binding AS binding").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id").
		Joins("JOIN platform_application AS application ON application.id = role.application_id AND application.tenant_id = role.tenant_id").
		Where("binding.tenant_id = ? AND application.code = ?", tenantID, platformApplicationCode)
	if page.Keyword != "" {
		query = query.Where("binding.subject_id LIKE ? OR role.name LIKE ?", like(page.Keyword), like(page.Keyword))
	}
	if page.Status != "" {
		query = query.Where("binding.status = ?", page.Status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return application.PageResult[domain.RoleBinding]{}, fmt.Errorf("count role bindings: %w", err)
	}
	var rows []bindingProjection
	result := query.Select("binding.id, binding.role_id, role.code AS role_code, role.name AS role_name, binding.grant_origin, binding.subject_type, binding.subject_id, binding.scope_type, binding.scope_id, binding.valid_until, binding.status, binding.version").Order("binding.created_at DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Scan(&rows)
	if result.Error != nil {
		return application.PageResult[domain.RoleBinding]{}, fmt.Errorf("list role bindings: %w", result.Error)
	}
	items := make([]domain.RoleBinding, 0, len(rows))
	for _, row := range rows {
		items = append(items, bindingProjectionToDomain(row))
	}
	return application.PageResult[domain.RoleBinding]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}
// CreateRoleBinding 在单事务内完成受保护角色操作者校验、可委派权限校验与主体/作用域
// 引用校验后创建 MANUAL 来源绑定，并推进应用 revision。
func (r *GORMRepository) CreateRoleBinding(ctx context.Context, tenantID, operatorID string, binding domain.RoleBinding) (domain.RoleBinding, error) {
	var output domain.RoleBinding
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var role roleModel
		result := tx.Where("id = ? AND tenant_id = ? AND status = ?", binding.Role.ID, tenantID, domain.StatusActive).First(&role)
		if result.Error != nil {
			return translateNotFound(result.Error, "role")
		}
		if err := ensureApplicationRoleBindingManaged(role); err != nil {
			return err
		}
		// 应用目录角色只能通过应用授权入口管理；通用绑定入口仅处理平台角色，并在
		// 这里同时防护受保护角色和“角色内权限不可委派”两条提权路径。
		if err := r.ensureProtectedRoleBindingOperator(ctx, tx, tenantID, operatorID, role); err != nil {
			return err
		}
		if err := r.verifyDelegableRolePermissions(ctx, tx, tenantID, operatorID, role); err != nil {
			return err
		}
		if err := validateRoleBindingReferences(tx, tenantID, binding); err != nil {
			return err
		}
		now := time.Now().UTC()
		operator := operatorID
		scopeID := ""
		if binding.ScopeID != nil {
			scopeID = *binding.ScopeID
		}
		model := roleBindingModel{ID: binding.ID, TenantID: tenantID, ApplicationID: role.ApplicationID, RoleID: role.ID, SubjectType: binding.SubjectType, SubjectID: binding.Subject.ID, ScopeType: binding.ScopeType, ScopeID: scopeID, GrantOrigin: "MANUAL", ValidUntil: binding.ExpiresAt, Status: domain.StatusActive, Version: 1, CreatedAt: now, CreatedBy: &operator, UpdatedAt: now, UpdatedBy: &operator}
		if err := tx.Create(&model).Error; err != nil {
			return translateError(err, "create role binding")
		}
		if err := r.bumpRevision(tx, tenantID, role.ApplicationID, now, "role binding created"); err != nil {
			return err
		}
		output = bindingProjectionToDomain(bindingProjection{ID: model.ID, RoleID: role.ID, RoleCode: role.Code, RoleName: role.Name, GrantOrigin: model.GrantOrigin, SubjectType: model.SubjectType, SubjectID: model.SubjectID, ScopeType: model.ScopeType, ScopeID: model.ScopeID, ValidUntil: model.ValidUntil, Status: model.Status, Version: model.Version})
		return nil
	})
	return output, err
}
// UpdateRoleBinding 在乐观锁保护下更新 MANUAL 绑定；模板/系统来源只能由所属流程
// 维护，替换角色时新旧角色都要通过受保护角色与可委派校验。
func (r *GORMRepository) UpdateRoleBinding(ctx context.Context, tenantID, operatorID string, binding domain.RoleBinding) (domain.RoleBinding, error) {
	var output domain.RoleBinding
	err := r.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing roleBindingModel
		result := tx.Where("id = ? AND tenant_id = ?", binding.ID, tenantID).First(&existing)
		if result.Error != nil {
			return translateNotFound(result.Error, "role binding")
		}
		if existing.Version != binding.Version {
			return application.ErrVersionConflict
		}
		// 来源由持久化记录决定，模板/系统绑定只能由所属流程维护；超管也不能
		// 经通用入口改写其主体、角色或有效范围。空来源沿用历史人工绑定兼容约定。
		origin := strings.ToUpper(strings.TrimSpace(existing.GrantOrigin))
		if origin != "" && origin != "MANUAL" {
			return application.ErrConflict
		}
		var role roleModel
		result = tx.Where("id = ? AND tenant_id = ? AND application_id = ?", binding.Role.ID, tenantID, existing.ApplicationID).First(&role)
		if result.Error != nil {
			return translateNotFound(result.Error, "role")
		}
		if err := ensureApplicationRoleBindingManaged(role); err != nil {
			return err
		}
		if err := r.ensureProtectedRoleBindingOperator(ctx, tx, tenantID, operatorID, role); err != nil {
			return err
		}
		if err := r.verifyDelegableRolePermissions(ctx, tx, tenantID, operatorID, role); err != nil {
			return err
		}
		if existing.RoleID != role.ID {
			// 替换角色时，旧角色也要经过受保护角色检查。否则普通管理员可通过“把高权
			// 绑定改成低权绑定”的更新动作间接停用紧急/超级管理员访问。
			var existingRole roleModel
			if err := tx.Where("id = ? AND tenant_id = ? AND application_id = ?", existing.RoleID, tenantID, existing.ApplicationID).First(&existingRole).Error; err != nil {
				return translateNotFound(err, "role")
			}
			if err := ensureApplicationRoleBindingManaged(existingRole); err != nil {
				return err
			}
			if err := r.ensureProtectedRoleBindingOperator(ctx, tx, tenantID, operatorID, existingRole); err != nil {
				return err
			}
			if err := r.verifyDelegableRolePermissions(ctx, tx, tenantID, operatorID, existingRole); err != nil {
				return err
			}
		}
		if err := validateRoleBindingReferences(tx, tenantID, binding); err != nil {
			return err
		}
		// 部署初始化的超级管理员绑定即使调用者就是该管理员本身也不可更新本行：
		// 不能撤销、替换、收窄或使其过期。该检查放在常规授权与可委派门禁之后、
		// 第一次写入之前。
		identity, initialized, err := bootstrapadmin.Resolve(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if initialized && existing.SubjectType == "USER" && existing.SubjectID == identity.UserID {
			bootstrap, err := isBootstrapSuperAdminRole(tx, tenantID, existing.RoleID)
			if err != nil {
				return err
			}
			if bootstrap {
				return bootstrapadmin.ErrProtected
			}
		}
		now := time.Now().UTC()
		scopeID := ""
		if binding.ScopeID != nil {
			scopeID = *binding.ScopeID
		}
		result = tx.Model(&roleBindingModel{}).Where("id = ? AND version = ?", binding.ID, binding.Version).Updates(map[string]any{"role_id": role.ID, "subject_type": binding.SubjectType, "subject_id": binding.Subject.ID, "scope_type": binding.ScopeType, "scope_id": scopeID, "valid_until": binding.ExpiresAt, "status": binding.Status, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": operatorID})
		if result.Error != nil {
			return fmt.Errorf("update role binding: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return application.ErrVersionConflict
		}
		if err := r.bumpRevision(tx, tenantID, existing.ApplicationID, now, "role binding updated"); err != nil {
			return err
		}
		output = bindingProjectionToDomain(bindingProjection{ID: binding.ID, RoleID: role.ID, RoleCode: role.Code, RoleName: role.Name, GrantOrigin: existing.GrantOrigin, SubjectType: binding.SubjectType, SubjectID: binding.Subject.ID, ScopeType: binding.ScopeType, ScopeID: scopeID, ValidUntil: binding.ExpiresAt, Status: binding.Status, Version: binding.Version + 1})
		return nil
	})
	return output, err
}

// ensureProtectedRoleBindingOperator 把控制面角色限定给租户级超级管理员。角色绑定
// 属安全关键操作：缺少该检查时，安全管理员可以把超级管理员角色绑给自己或其他
// 普通账号。
func (r *GORMRepository) ensureProtectedRoleBindingOperator(ctx context.Context, tx *gorm.DB, tenantID, operatorID string, role roleModel) error {
	if !isProtectedRoleCode(role.Code) {
		return nil
	}

	now := time.Now().UTC()
	subjectSQL, subjectArgs := roleBindingSubjectFilter(operatorID, delegation.OperatorAccountIDFromContext(ctx, tenantID, operatorID), now)
	var total int64
	result := tx.Table("authz_role_binding AS binding").
		Joins("JOIN authz_role AS operator_role ON operator_role.id = binding.role_id").
		Where("binding.tenant_id = ? AND binding.application_id = ?", tenantID, role.ApplicationID).
		Where("binding.status = ? AND (binding.valid_until IS NULL OR binding.valid_until > ?)", domain.StatusActive, now).
		Where("binding.scope_type = ? AND binding.scope_id = ?", "TENANT", "").
		Where("operator_role.code = ? AND operator_role.status = ?", domain.SuperAdminRoleCode, domain.StatusActive).
		Where(subjectSQL, subjectArgs...).
		Count(&total)
	if result.Error != nil {
		return fmt.Errorf("check protected role-binding operator: %w", result.Error)
	}
	if total == 0 {
		return application.ErrForbidden
	}
	return nil
}

func isProtectedRoleCode(code string) bool {
	return domain.IsProtectedRoleCode(code)
}

// isBootstrapSuperAdminRole 判断该角色是否为内置平台超级管理员角色。只有 platform
// 应用可以定义它；其他应用的同名角色绝不能让普通绑定伪装成初始化权威。
func isBootstrapSuperAdminRole(tx *gorm.DB, tenantID, roleID string) (bool, error) {
	var count int64
	err := tx.Table("authz_role AS role").
		Joins("JOIN platform_application AS application ON application.id = role.application_id AND application.tenant_id = role.tenant_id").
		Where("role.id = ? AND role.tenant_id = ? AND role.code = ? AND application.code = ?", roleID, tenantID, domain.SuperAdminRoleCode, platformApplicationCode).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("check bootstrap super-administrator role: %w", err)
	}
	return count > 0, nil
}

// validateRoleBindingReferences 在写入绑定前解析租户所属的主体与作用域目标。
// 作用域 ID 来自请求：接受不存在或跨租户的 ID 会让后续授权判定出现歧义，
// 在数据修复或复用时可能放大访问。
func validateRoleBindingReferences(tx *gorm.DB, tenantID string, binding domain.RoleBinding) error {
	var subjectTable string
	switch binding.SubjectType {
	case "USER":
		subjectTable = "iam_user"
	case "ACCOUNT":
		subjectTable = "iam_account"
	case "ORG_UNIT":
		subjectTable = "iam_org_unit"
	case "POSITION":
		subjectTable = "iam_position"
	default:
		return application.ErrValidation
	}
	if err := requireTenantResource(tx, subjectTable, tenantID, binding.Subject.ID); err != nil {
		return err
	}

	scopeID := ""
	if binding.ScopeID != nil {
		scopeID = strings.TrimSpace(*binding.ScopeID)
	}
	switch binding.ScopeType {
	case "TENANT":
		return nil
	case "ORG_UNIT":
		return requireTenantResource(tx, "iam_org_unit", tenantID, scopeID)
	case "RESOURCE":
		// 平台 IAM 资源作用域目前指向具体的组织、岗位或任职 ID。服务端逐一解析，
		// 未知 ID 一律失败关闭。
		for _, table := range []string{"iam_org_unit", "iam_position", "iam_membership"} {
			var count int64
			if err := tx.Table(table).Where("tenant_id = ? AND id = ?", tenantID, scopeID).Count(&count).Error; err != nil {
				return fmt.Errorf("validate role binding resource scope: %w", err)
			}
			if count == 1 {
				return nil
			}
		}
		return application.ErrNotFound
	default:
		return application.ErrValidation
	}
}

func requireTenantResource(tx *gorm.DB, table, tenantID, resourceID string) error {
	if strings.TrimSpace(resourceID) == "" {
		return application.ErrValidation
	}
	var count int64
	if err := tx.Table(table).Where("tenant_id = ? AND id = ?", tenantID, resourceID).Count(&count).Error; err != nil {
		return fmt.Errorf("validate role binding reference: %w", err)
	}
	if count != 1 {
		return application.ErrNotFound
	}
	return nil
}

// Check 基于租户当前策略 revision 判定权限；上下文只消费上层从可信资源归属
// 解析出的值，未匹配时默认拒绝（DENY_NO_MATCH）。
func (r *GORMRepository) Check(ctx context.Context, input application.CheckInput) (domain.Decision, error) {
	var app applicationModel
	result := r.database.WithContext(ctx).Where("tenant_id = ? AND code = ? AND status = ?", input.TenantID, platformApplicationCode, domain.StatusActive).First(&app)
	if result.Error != nil {
		return domain.Decision{}, fmt.Errorf("load platform application: %w", result.Error)
	}
	var revision policyRevisionModel
	result = r.database.WithContext(ctx).Where("tenant_id = ? AND application_id = ?", input.TenantID, app.ID).First(&revision)
	if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return domain.Decision{}, fmt.Errorf("load authorization revision: %w", result.Error)
	}
	decision := domain.Decision{Allowed: false, PermissionCode: input.PermissionCode, PolicyVersion: revision.Revision, ReasonCode: "DENY_NO_MATCH"}
	now := time.Now().UTC()
	// scopeFilter 只消费上层从可信资源归属解析出的上下文；HTTP 请求体中的组织 ID
	// 不能直接构造 CheckInput，否则会把数据定位参数误当作授权事实。
	scopeSQL, scopeArgs := scopeFilter(input)
	subjectSQL, subjectArgs := roleBindingSubjectFilter(input.UserID, input.AccountID, now)
	query := r.database.WithContext(ctx).
		Table("authz_role_binding AS binding").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id").
		Joins("JOIN authz_role_permission AS role_permission ON role_permission.role_id = role.id AND role_permission.effect = 'ALLOW'").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id").
		Where("binding.tenant_id = ? AND binding.application_id = ? AND binding.status = ? AND role.status = ? AND permission.status = ? AND permission.code = ?", input.TenantID, app.ID, domain.StatusActive, domain.StatusActive, domain.StatusActive, input.PermissionCode).
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now).
		Where(subjectSQL, subjectArgs...).
		Where(scopeSQL, scopeArgs...)

	var matches int64
	if err := query.Count(&matches).Error; err != nil {
		return domain.Decision{}, fmt.Errorf("evaluate authorization policy: %w", err)
	}
	if matches > 0 {
		decision.Allowed = true
		decision.ReasonCode = "ALLOW_ROLE_BINDING"
	}
	return decision, nil
}

func (r *GORMRepository) activeApplication(ctx context.Context, tenantID, code string) (applicationModel, error) {
	return r.activeApplicationTx(r.database.WithContext(ctx), tenantID, code)
}
func (r *GORMRepository) activeApplicationTx(tx *gorm.DB, tenantID, code string) (applicationModel, error) {
	var app applicationModel
	result := tx.Where("tenant_id = ? AND code = ? AND status = ?", tenantID, code, domain.StatusActive).First(&app)
	if result.Error != nil {
		return applicationModel{}, translateNotFound(result.Error, "application")
	}
	return app, nil
}

// verifyDelegableRolePermissions 把创建/更新自定义角色时的可委派边界同样应用到每次
// 绑定变更：管理绑定本身不等于有权授予既有角色包含的全部权限。
func (r *GORMRepository) verifyDelegableRolePermissions(ctx context.Context, tx *gorm.DB, tenantID, operatorID string, role roleModel) error {
	return delegation.VerifyRolePermissions(ctx, tx, tenantID, operatorID, role.ApplicationID, role.ID)
}

// verifyDelegablePermissions 防止自定义角色沦为提权机制：所选权限必须全部是当前
// 已认证操作者在租户范围内的有效权限；仅有角色管理权限不够。
func (r *GORMRepository) verifyDelegablePermissions(tx *gorm.DB, tenantID, operatorID, operatorAccountID, applicationID string, permissionIDs []string) error {
	return delegation.VerifyPermissions(context.Background(), tx, tenantID, operatorID, operatorAccountID, applicationID, permissionIDs)
}

func (r *GORMRepository) verifyPermissions(tx *gorm.DB, tenantID, applicationID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var count int64
	if err := tx.Model(&permissionModel{}).Where("tenant_id = ? AND application_id = ? AND status = ? AND id IN ?", tenantID, applicationID, domain.StatusActive, ids).Count(&count).Error; err != nil {
		return fmt.Errorf("verify permissions: %w", err)
	}
	if count != int64(len(ids)) {
		return application.ErrNotFound
	}
	return nil
}
func replacePermissions(tx *gorm.DB, roleID, operatorID string, now time.Time, ids []string) error {
	if err := tx.Where("role_id = ?", roleID).Delete(&rolePermissionModel{}).Error; err != nil {
		return fmt.Errorf("delete role permissions: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	rows := make([]rolePermissionModel, 0, len(ids))
	for _, id := range ids {
		operator := operatorID
		rows = append(rows, rolePermissionModel{RoleID: roleID, PermissionID: id, Effect: "ALLOW", CreatedAt: now, CreatedBy: &operator})
	}
	if err := tx.Create(&rows).Error; err != nil {
		return fmt.Errorf("create role permissions: %w", err)
	}
	return nil
}
func (r *GORMRepository) roleWithPermissions(ctx context.Context, role roleModel) (domain.Role, error) {
	return r.roleWithPermissionsTx(r.database.WithContext(ctx), role)
}
func (r *GORMRepository) roleWithPermissionsTx(tx *gorm.DB, role roleModel) (domain.Role, error) {
	permissions, err := rolePermissionReferences(tx, role.ID, false)
	if err != nil {
		return domain.Role{}, err
	}
	return domain.Role{ID: role.ID, ApplicationID: role.ApplicationID, Code: role.Code, Name: role.Name, RoleType: role.RoleType, Description: role.Description, Status: role.Status, BuiltIn: role.BuiltIn, Permissions: permissions, Version: role.Version}, nil
}

// activeRolePermissionReferences 与授权检查保持一致的权限状态要求：影响预览展示的
// 必须恰好是拟议绑定当前实际能授予的权限。
func activeRolePermissionReferences(tx *gorm.DB, roleID string) ([]domain.Reference, error) {
	return rolePermissionReferences(tx, roleID, true)
}

func rolePermissionReferences(tx *gorm.DB, roleID string, activeOnly bool) ([]domain.Reference, error) {
	var rows []permissionReferenceProjection
	query := tx.Table("authz_role_permission AS role_permission").
		Select("permission.id, permission.code, permission.name").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id").
		Where("role_permission.role_id = ? AND role_permission.effect = 'ALLOW'", roleID)
	if activeOnly {
		query = query.Where("permission.status = ?", domain.StatusActive)
	}
	result := query.Order("permission.code").Scan(&rows)
	if result.Error != nil {
		return nil, fmt.Errorf("list role permissions: %w", result.Error)
	}
	permissions := make([]domain.Reference, 0, len(rows))
	for _, row := range rows {
		permissions = append(permissions, domain.Reference{ID: row.ID, Code: row.Code, Name: row.Name})
	}
	return permissions, nil
}
func (r *GORMRepository) bumpRevision(tx *gorm.DB, tenantID, applicationID string, now time.Time, reason string) error {
	result := tx.Model(&policyRevisionModel{}).Where("tenant_id = ? AND application_id = ?", tenantID, applicationID).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "changed_at": now, "change_reason": reason})
	if result.Error != nil {
		return fmt.Errorf("bump authorization revision: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("authorization policy revision is missing: %w", application.ErrNotFound)
	}
	return nil
}

// roleBindingSubjectFilter 通过共享的 delegation 包解析有效绑定主体，
// 使绑定评估与可委派校验永远不会分叉。
func roleBindingSubjectFilter(userID, accountID string, now time.Time) (string, []any) {
	return delegation.SubjectFilter(userID, accountID, now)
}

func scopeFilter(input application.CheckInput) (string, []any) {
	// 租户级绑定始终参与候选；资源和组织级绑定只有在本次服务端资源上下文精确匹配
	// 时才参与，避免“有权限码但作用域不符”仍被放行。
	clauses := []string{"(binding.scope_type = ? AND binding.scope_id = ?)"}
	arguments := []any{"TENANT", ""}
	if input.ResourceID != "" {
		clauses = append(clauses, "(binding.scope_type = ? AND binding.scope_id = ?)")
		arguments = append(arguments, "RESOURCE", input.ResourceID)
	}
	if organizationUnitID, ok := stringValue(input.Context, "org_unit_id"); ok {
		clauses = append(clauses, "(binding.scope_type = ? AND binding.scope_id = ?)")
		arguments = append(arguments, "ORG_UNIT", organizationUnitID)
	}
	return strings.Join(clauses, " OR "), arguments
}

func stringValue(values map[string]any, key string) (string, bool) {
	if values == nil {
		return "", false
	}
	value, ok := values[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}
	return strings.TrimSpace(value), true
}

func like(value string) string { return "%" + strings.ReplaceAll(value, "%", "\\%") + "%" }
func translateNotFound(err error, resource string) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%s: %w", resource, application.ErrNotFound)
	}
	return fmt.Errorf("load %s: %w", resource, err)
}
func translateError(err error, operation string) error {
	if isDuplicate(err) {
		return fmt.Errorf("%s: %w", operation, application.ErrConflict)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
func isDuplicate(err error) bool { return strings.Contains(strings.ToLower(err.Error()), "duplicate") }

type resourceProjection struct {
	ID, ApplicationCode, Code, Name, ResourceType string
	Version                                       uint64
}
type permissionProjection struct {
	ID, Code, Name, Action, ResourceID, ResourceCode, ResourceName string
	Version                                                        uint64
}
type permissionReferenceProjection struct{ ID, Code, Name string }
type bindingProjection struct {
	ID, RoleID, RoleCode, RoleName, GrantOrigin, SubjectType, SubjectID, ScopeType, ScopeID, Status string
	ValidUntil                                                                                      *time.Time
	Version                                                                                         uint64
}

func bindingProjectionToDomain(row bindingProjection) domain.RoleBinding {
	var scopeID *string
	if row.ScopeType != "TENANT" {
		copy := row.ScopeID
		scopeID = &copy
	}
	return domain.RoleBinding{ID: row.ID, Role: domain.Reference{ID: row.RoleID, Code: row.RoleCode, Name: row.RoleName}, GrantOrigin: row.GrantOrigin, SubjectType: row.SubjectType, Subject: domain.Reference{ID: row.SubjectID, Name: row.SubjectID}, ScopeType: row.ScopeType, ScopeID: scopeID, Status: row.Status, ExpiresAt: row.ValidUntil, Version: row.Version}
}

// previewAccountProjection 有意留在 authorization 包内：它只服务于管理端解释 API，
// 身份聚合的所有权仍归 identity 模块。
type previewAccountProjection struct {
	UserID        string     `gorm:"column:user_id"`
	UserName      string     `gorm:"column:user_name"`
	UserStatus    string     `gorm:"column:user_status"`
	AccountID     string     `gorm:"column:account_id"`
	AccountName   string     `gorm:"column:account_name"`
	AccountStatus string     `gorm:"column:account_status"`
	ValidUntil    *time.Time `gorm:"column:valid_until"`
}

type effectiveAccessProjection struct {
	BindingID        string `gorm:"column:binding_id"`
	SubjectType      string `gorm:"column:subject_type"`
	SubjectID        string `gorm:"column:subject_id"`
	SubjectName      string `gorm:"column:subject_name"`
	SubjectCode      string `gorm:"column:subject_code"`
	ScopeType        string `gorm:"column:scope_type"`
	ScopeID          string `gorm:"column:scope_id"`
	RoleID           string `gorm:"column:role_id"`
	RoleCode         string `gorm:"column:role_code"`
	RoleName         string `gorm:"column:role_name"`
	PermissionID     string `gorm:"column:permission_id"`
	PermissionCode   string `gorm:"column:permission_code"`
	PermissionName   string `gorm:"column:permission_name"`
	PermissionAction string `gorm:"column:permission_action"`
	ResourceID       string `gorm:"column:resource_id"`
	ResourceCode     string `gorm:"column:resource_code"`
	ResourceName     string `gorm:"column:resource_name"`
}

type previewReferenceProjection struct {
	ID   string `gorm:"column:id"`
	Name string `gorm:"column:name"`
	Code string `gorm:"column:code"`
}

// PreviewEffectiveAccess 使用与 Check 完全相同的主体谓词列出当前有效的角色绑定路径。
// 有意返回作用域元数据而非布尔授权结论：调用方仍需结合具体资源与组织上下文自行判定。
func (r *GORMRepository) PreviewEffectiveAccess(ctx context.Context, tenantID, userID, accountID string, now time.Time) (domain.EffectiveAccessPreview, error) {
	var account previewAccountProjection
	result := r.database.WithContext(ctx).
		Table("iam_account AS account").
		Select("user.id AS user_id, user.display_name AS user_name, user.status AS user_status, account.id AS account_id, COALESCE(account.username, account.id) AS account_name, account.status AS account_status, account.valid_until").
		Joins("JOIN iam_user AS user ON user.id = account.user_id AND user.tenant_id = account.tenant_id").
		Where("account.tenant_id = ? AND account.id = ? AND account.user_id = ?", tenantID, accountID, userID).
		Scan(&account)
	if result.Error != nil {
		return domain.EffectiveAccessPreview{}, fmt.Errorf("load access preview account: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.EffectiveAccessPreview{}, application.ErrNotFound
	}

	app, err := r.activeApplication(ctx, tenantID, platformApplicationCode)
	if err != nil {
		return domain.EffectiveAccessPreview{}, err
	}
	var revision policyRevisionModel
	result = r.database.WithContext(ctx).Where("tenant_id = ? AND application_id = ?", tenantID, app.ID).First(&revision)
	if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return domain.EffectiveAccessPreview{}, fmt.Errorf("load authorization revision: %w", result.Error)
	}

	subjectSQL, subjectArgs := roleBindingSubjectFilter(userID, accountID, now)
	var rows []effectiveAccessProjection
	result = r.database.WithContext(ctx).
		Table("authz_role_binding AS binding").
		Select(`binding.id AS binding_id, binding.subject_type, binding.subject_id,
			CASE binding.subject_type
				WHEN 'USER' THEN source_user.display_name
				WHEN 'ACCOUNT' THEN COALESCE(source_account.username, source_account.id)
				WHEN 'ORG_UNIT' THEN source_org.name
				WHEN 'POSITION' THEN source_position.name
				ELSE binding.subject_id END AS subject_name,
			CASE binding.subject_type
				WHEN 'ORG_UNIT' THEN source_org.code
				WHEN 'POSITION' THEN source_position.code
				ELSE '' END AS subject_code,
			binding.scope_type, binding.scope_id,
			role.id AS role_id, role.code AS role_code, role.name AS role_name,
			permission.id AS permission_id, permission.code AS permission_code, permission.name AS permission_name, permission.action AS permission_action,
			resource.id AS resource_id, resource.code AS resource_code, resource.name AS resource_name`).
		Joins("JOIN authz_role AS role ON role.id = binding.role_id").
		Joins("LEFT JOIN authz_role_permission AS role_permission ON role_permission.role_id = role.id AND role_permission.effect = 'ALLOW'").
		Joins("LEFT JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.status = ?", domain.StatusActive).
		Joins("LEFT JOIN authz_resource AS resource ON resource.id = permission.resource_id").
		Joins("LEFT JOIN iam_user AS source_user ON binding.subject_type = 'USER' AND source_user.id = binding.subject_id AND source_user.tenant_id = binding.tenant_id").
		Joins("LEFT JOIN iam_account AS source_account ON binding.subject_type = 'ACCOUNT' AND source_account.id = binding.subject_id AND source_account.tenant_id = binding.tenant_id").
		Joins("LEFT JOIN iam_org_unit AS source_org ON binding.subject_type = 'ORG_UNIT' AND source_org.id = binding.subject_id AND source_org.tenant_id = binding.tenant_id").
		Joins("LEFT JOIN iam_position AS source_position ON binding.subject_type = 'POSITION' AND source_position.id = binding.subject_id AND source_position.tenant_id = binding.tenant_id").
		Where("binding.tenant_id = ? AND binding.application_id = ? AND binding.status = ? AND role.status = ?", tenantID, app.ID, domain.StatusActive, domain.StatusActive).
		Where("binding.valid_from IS NULL OR binding.valid_from <= ?", now).
		Where("binding.valid_until IS NULL OR binding.valid_until > ?", now).
		Where(subjectSQL, subjectArgs...).
		Order("role.code, permission.code, binding.id").
		Scan(&rows)
	if result.Error != nil {
		return domain.EffectiveAccessPreview{}, fmt.Errorf("list effective access: %w", result.Error)
	}

	preview := domain.EffectiveAccessPreview{
		User:          domain.Reference{ID: account.UserID, Name: account.UserName},
		Account:       domain.Reference{ID: account.AccountID, Name: account.AccountName},
		LoginEligible: account.UserStatus == domain.StatusActive && account.AccountStatus == domain.StatusActive && (account.ValidUntil == nil || account.ValidUntil.After(now)),
		PolicyVersion: revision.Revision,
		GeneratedAt:   now,
		Roles:         make([]domain.EffectiveRole, 0),
		Permissions:   make([]domain.EffectivePermission, 0),
	}
	roleIndex := make(map[string]int)
	permissionIndex := make(map[string]int)
	for _, row := range rows {
		source := effectiveSource(row)
		index, found := roleIndex[row.RoleID]
		if !found {
			index = len(preview.Roles)
			roleIndex[row.RoleID] = index
			preview.Roles = append(preview.Roles, domain.EffectiveRole{Role: domain.Reference{ID: row.RoleID, Code: row.RoleCode, Name: row.RoleName}})
		}
		preview.Roles[index].Sources = appendUniqueSource(preview.Roles[index].Sources, source)
		if row.PermissionID == "" {
			continue
		}
		index, found = permissionIndex[row.PermissionID]
		if !found {
			index = len(preview.Permissions)
			permissionIndex[row.PermissionID] = index
			preview.Permissions = append(preview.Permissions, domain.EffectivePermission{Permission: domain.Permission{ID: row.PermissionID, Code: row.PermissionCode, Name: row.PermissionName, Action: row.PermissionAction, Resource: domain.Reference{ID: row.ResourceID, Code: row.ResourceCode, Name: row.ResourceName}}})
		}
		preview.Permissions[index].Sources = appendUniqueSource(preview.Permissions[index].Sources, source)
	}

	var providers []previewReferenceProjection
	result = r.database.WithContext(ctx).
		Table("iam_federated_identity_binding AS external_binding").
		Select("DISTINCT provider.id, provider.display_name AS name, provider.provider_code AS code").
		Joins("JOIN iam_federated_identity_provider AS provider ON provider.id = external_binding.provider_id AND provider.tenant_id = external_binding.tenant_id").
		Where("external_binding.tenant_id = ? AND external_binding.user_id = ? AND external_binding.status = ? AND provider.status = ?", tenantID, userID, domain.StatusActive, domain.StatusActive).
		Order("provider.provider_code").
		Scan(&providers)
	if result.Error != nil {
		return domain.EffectiveAccessPreview{}, fmt.Errorf("list external identity providers: %w", result.Error)
	}
	for _, provider := range providers {
		preview.ExternalIdentityProviders = append(preview.ExternalIdentityProviders, domain.Reference{ID: provider.ID, Name: provider.Name, Code: provider.Code})
	}
	return preview, nil
}

// PreviewRoleBindingImpact 报告将立即继承拟议绑定的在册用户。用户列表有意封顶，
// TotalAffectedUsers 保持精确计数，两者配合兼顾响应体大小与影响面准确性。
func (r *GORMRepository) PreviewRoleBindingImpact(ctx context.Context, input application.RoleBindingImpactInput, now time.Time) (domain.RoleBindingImpactPreview, error) {
	app, err := r.activeApplication(ctx, input.TenantID, platformApplicationCode)
	if err != nil {
		return domain.RoleBindingImpactPreview{}, err
	}
	var role roleModel
	result := r.database.WithContext(ctx).Where("id = ? AND tenant_id = ? AND application_id = ? AND status = ?", input.RoleID, input.TenantID, app.ID, domain.StatusActive).First(&role)
	if result.Error != nil {
		return domain.RoleBindingImpactPreview{}, translateNotFound(result.Error, "role")
	}
	activePermissions, err := activeRolePermissionReferences(r.database.WithContext(ctx), role.ID)
	if err != nil {
		return domain.RoleBindingImpactPreview{}, err
	}
	subject, err := r.previewBindingSubject(ctx, input.TenantID, input.SubjectType, input.SubjectID)
	if err != nil {
		return domain.RoleBindingImpactPreview{}, err
	}

	query, err := r.impactUsersQuery(ctx, input.TenantID, input.SubjectType, input.SubjectID, now)
	if err != nil {
		return domain.RoleBindingImpactPreview{}, err
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return domain.RoleBindingImpactPreview{}, fmt.Errorf("count role binding impact users: %w", err)
	}
	const sampleLimit = 100
	var users []previewReferenceProjection
	result = query.Select("DISTINCT user.id, user.display_name AS name, user.employee_no AS code").Order("user.display_name, user.id").Limit(sampleLimit).Scan(&users)
	if result.Error != nil {
		return domain.RoleBindingImpactPreview{}, fmt.Errorf("list role binding impact users: %w", result.Error)
	}
	output := domain.RoleBindingImpactPreview{
		Role: domain.Reference{ID: role.ID, Code: role.Code, Name: role.Name}, Permissions: activePermissions,
		SubjectType: input.SubjectType, Subject: subject, ScopeType: input.ScopeType, ScopeID: trimScopeID(input.ScopeID), ExpiresAt: copyPreviewTime(input.ExpiresAt),
		TotalAffectedUsers: total, Truncated: total > sampleLimit, GeneratedAt: now, Users: make([]domain.Reference, 0, len(users)),
	}
	for _, user := range users {
		output.Users = append(output.Users, domain.Reference{ID: user.ID, Name: user.Name, Code: user.Code})
	}
	return output, nil
}

func effectiveSource(row effectiveAccessProjection) domain.AccessSource {
	var scopeID *string
	if strings.TrimSpace(row.ScopeID) != "" {
		value := row.ScopeID
		scopeID = &value
	}
	return domain.AccessSource{BindingID: row.BindingID, SubjectType: row.SubjectType, Subject: domain.Reference{ID: row.SubjectID, Name: row.SubjectName, Code: row.SubjectCode}, ScopeType: row.ScopeType, ScopeID: scopeID}
}

func appendUniqueSource(values []domain.AccessSource, candidate domain.AccessSource) []domain.AccessSource {
	for _, value := range values {
		if value.BindingID == candidate.BindingID {
			return values
		}
	}
	return append(values, candidate)
}

func trimScopeID(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func copyPreviewTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (r *GORMRepository) previewBindingSubject(ctx context.Context, tenantID, subjectType, subjectID string) (domain.Reference, error) {
	var value previewReferenceProjection
	var query *gorm.DB
	switch subjectType {
	case "USER":
		query = r.database.WithContext(ctx).Table("iam_user").Select("id, display_name AS name, employee_no AS code").Where("tenant_id = ? AND id = ?", tenantID, subjectID)
	case "ACCOUNT":
		query = r.database.WithContext(ctx).Table("iam_account").Select("id, COALESCE(username, id) AS name, '' AS code").Where("tenant_id = ? AND id = ?", tenantID, subjectID)
	case "ORG_UNIT":
		query = r.database.WithContext(ctx).Table("iam_org_unit").Select("id, name, code").Where("tenant_id = ? AND id = ?", tenantID, subjectID)
	case "POSITION":
		query = r.database.WithContext(ctx).Table("iam_position").Select("id, name, code").Where("tenant_id = ? AND id = ?", tenantID, subjectID)
	default:
		return domain.Reference{}, application.ErrValidation
	}
	result := query.Scan(&value)
	if result.Error != nil {
		return domain.Reference{}, fmt.Errorf("load role binding subject: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.Reference{}, application.ErrNotFound
	}
	return domain.Reference{ID: value.ID, Name: value.Name, Code: value.Code}, nil
}

func (r *GORMRepository) impactUsersQuery(ctx context.Context, tenantID, subjectType, subjectID string, now time.Time) (*gorm.DB, error) {
	base := r.database.WithContext(ctx).Table("iam_user AS user").Where("user.tenant_id = ? AND user.status = ?", tenantID, domain.StatusActive)
	switch subjectType {
	case "USER":
		return base.Where("user.id = ?", subjectID), nil
	case "ACCOUNT":
		return base.Joins("JOIN iam_account AS account ON account.user_id = user.id AND account.tenant_id = user.tenant_id").Where("account.id = ? AND account.status = ? AND (account.valid_until IS NULL OR account.valid_until > ?)", subjectID, domain.StatusActive, now), nil
	case "ORG_UNIT":
		return activeImpactMembershipQuery(base, subjectID, "membership.org_unit_id", now), nil
	case "POSITION":
		return activeImpactMembershipQuery(base, subjectID, "membership.position_id", now), nil
	default:
		return nil, application.ErrValidation
	}
}

func activeImpactMembershipQuery(base *gorm.DB, subjectID, subjectColumn string, now time.Time) *gorm.DB {
	return base.
		Joins("JOIN iam_membership AS membership ON membership.user_id = user.id AND membership.tenant_id = user.tenant_id").
		Joins("JOIN iam_org_unit AS organization ON organization.id = membership.org_unit_id AND organization.tenant_id = membership.tenant_id AND organization.status = ?", domain.StatusActive).
		Joins("JOIN iam_position AS position ON position.id = membership.position_id AND position.tenant_id = membership.tenant_id AND position.status = ?", domain.StatusActive).
		Where("membership.status = ? AND (membership.valid_from IS NULL OR membership.valid_from <= ?) AND (membership.valid_until IS NULL OR membership.valid_until > ?)", domain.StatusActive, now, now).
		Where(subjectColumn+" = ?", subjectID)
}
