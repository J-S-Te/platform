package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/authorization/domain"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
	"gorm.io/gorm"
)

const (
	platformApplicationCode = "platform"

	// Protected role assignments control the platform control plane. Only an effective tenant-scoped
	// super administrator can create, modify, or disable bindings involving these roles.
	platformSuperAdminRoleCode     = "platform-super-admin"
	platformEmergencyAdminRoleCode = "platform-emergency-admin"
	platformBreakGlassRolePrefix   = "platform-break-glass-"
)

// GORMRepository persists RBAC aggregates in tables owned by SQL migrations. It deliberately does
// not call AutoMigrate so schema history remains explicit and reviewable.
type GORMRepository struct{ database *gorm.DB }

func NewGORMRepository(database *gorm.DB) (*GORMRepository, error) {
	if database == nil {
		return nil, errors.New("authorization GORM database must not be nil")
	}
	return &GORMRepository{database: database}, nil
}

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

func (r *GORMRepository) ListRoles(ctx context.Context, tenantID string, page application.PageRequest) (application.PageResult[domain.Role], error) {
	query := r.database.WithContext(ctx).Model(&roleModel{}).Where("tenant_id = ?", tenantID)
	if page.Keyword != "" {
		query = query.Where("code LIKE ? OR name LIKE ?", like(page.Keyword), like(page.Keyword))
	}
	if page.Status != "" {
		query = query.Where("status = ?", page.Status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return application.PageResult[domain.Role]{}, fmt.Errorf("count roles: %w", err)
	}
	var rows []roleModel
	if err := query.Order("created_at DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Find(&rows).Error; err != nil {
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
func (r *GORMRepository) GetRole(ctx context.Context, tenantID, roleID string) (domain.Role, error) {
	var model roleModel
	result := r.database.WithContext(ctx).Where("id = ? AND tenant_id = ?", roleID, tenantID).First(&model)
	if result.Error != nil {
		return domain.Role{}, translateNotFound(result.Error, "role")
	}
	return r.roleWithPermissions(ctx, model)
}
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

// ensureRoleEditable preserves the ownership boundary for roles. Built-in platform roles
// and application-defined roles are not mutable through the generic role-management API.
// Application roles must be changed by the owning subsystem through its catalog sync flow.
func ensureRoleEditable(role roleModel) error {
	if role.BuiltIn || strings.EqualFold(strings.TrimSpace(role.RoleType), "APPLICATION") {
		return application.ErrConflict
	}
	return nil
}

// ensureApplicationRoleBindingManaged keeps application-owned catalog roles on
// the application-access path, where grant provenance and effective-role limits
// are enforced. Generic RBAC binding APIs remain available for platform roles.
func ensureApplicationRoleBindingManaged(role roleModel) error {
	if strings.EqualFold(strings.TrimSpace(role.RoleType), "APPLICATION") {
		return application.ErrConflict
	}
	return nil
}

// catalogMirrorReadOnly treats a successfully synchronized catalog as application
// owned. A retained version/hash also keeps it read-only after a later failed
// resync; a sync failure must not reopen the directory for console mutations.
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

func (r *GORMRepository) ListRoleBindings(ctx context.Context, tenantID string, page application.PageRequest) (application.PageResult[domain.RoleBinding], error) {
	query := r.database.WithContext(ctx).Table("authz_role_binding AS binding").Joins("JOIN authz_role AS role ON role.id = binding.role_id").Where("binding.tenant_id = ?", tenantID)
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
	result := query.Select("binding.id, binding.role_id, role.code AS role_code, role.name AS role_name, binding.subject_type, binding.subject_id, binding.scope_type, binding.scope_id, binding.valid_until, binding.status, binding.version").Order("binding.created_at DESC").Offset((page.Page - 1) * page.PageSize).Limit(page.PageSize).Scan(&rows)
	if result.Error != nil {
		return application.PageResult[domain.RoleBinding]{}, fmt.Errorf("list role bindings: %w", result.Error)
	}
	items := make([]domain.RoleBinding, 0, len(rows))
	for _, row := range rows {
		items = append(items, bindingProjectionToDomain(row))
	}
	return application.PageResult[domain.RoleBinding]{Items: items, Page: page.Page, PageSize: page.PageSize, Total: total}, nil
}
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
		model := roleBindingModel{ID: binding.ID, TenantID: tenantID, ApplicationID: role.ApplicationID, RoleID: role.ID, SubjectType: binding.SubjectType, SubjectID: binding.Subject.ID, ScopeType: binding.ScopeType, ScopeID: scopeID, ValidUntil: binding.ExpiresAt, Status: domain.StatusActive, Version: 1, CreatedAt: now, CreatedBy: &operator, UpdatedAt: now, UpdatedBy: &operator}
		if err := tx.Create(&model).Error; err != nil {
			return translateError(err, "create role binding")
		}
		if err := r.bumpRevision(tx, tenantID, role.ApplicationID, now, "role binding created"); err != nil {
			return err
		}
		output = bindingProjectionToDomain(bindingProjection{ID: model.ID, RoleID: role.ID, RoleCode: role.Code, RoleName: role.Name, SubjectType: model.SubjectType, SubjectID: model.SubjectID, ScopeType: model.ScopeType, ScopeID: model.ScopeID, ValidUntil: model.ValidUntil, Status: model.Status, Version: model.Version})
		return nil
	})
	return output, err
}
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
		output = bindingProjectionToDomain(bindingProjection{ID: binding.ID, RoleID: role.ID, RoleCode: role.Code, RoleName: role.Name, SubjectType: binding.SubjectType, SubjectID: binding.Subject.ID, ScopeType: binding.ScopeType, ScopeID: scopeID, ValidUntil: binding.ExpiresAt, Status: binding.Status, Version: binding.Version + 1})
		return nil
	})
	return output, err
}

// ensureProtectedRoleBindingOperator restricts control-plane roles to a tenant-scoped super
// administrator. A role binding is security-critical: without this check a security administrator
// could bind the super-admin role to itself or another ordinary account.
func (r *GORMRepository) ensureProtectedRoleBindingOperator(ctx context.Context, tx *gorm.DB, tenantID, operatorID string, role roleModel) error {
	if !isProtectedRoleCode(role.Code) {
		return nil
	}

	now := time.Now().UTC()
	subjectSQL, subjectArgs := roleBindingSubjectFilter(operatorID, operatorAccountIDFromContext(ctx, tenantID, operatorID), now)
	var total int64
	result := tx.Table("authz_role_binding AS binding").
		Joins("JOIN authz_role AS operator_role ON operator_role.id = binding.role_id").
		Where("binding.tenant_id = ? AND binding.application_id = ?", tenantID, role.ApplicationID).
		Where("binding.status = ? AND (binding.valid_until IS NULL OR binding.valid_until > ?)", domain.StatusActive, now).
		Where("binding.scope_type = ? AND binding.scope_id = ?", "TENANT", "").
		Where("operator_role.code = ? AND operator_role.status = ?", platformSuperAdminRoleCode, domain.StatusActive).
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
	normalized := strings.ToLower(strings.TrimSpace(code))
	return normalized == platformSuperAdminRoleCode ||
		normalized == platformEmergencyAdminRoleCode ||
		strings.HasPrefix(normalized, platformBreakGlassRolePrefix)
}

// validateRoleBindingReferences resolves tenant-owned subjects and scope targets before a binding
// is written. Scope IDs come from the request, so accepting a non-existent or cross-tenant ID would
// make later authorization ambiguous and could widen access when data is repaired or reused.
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
		// Platform IAM resource scopes currently target concrete organization, position, or
		// membership identifiers. Resolve them server-side and fail closed for unknown IDs.
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

// verifyDelegableRolePermissions applies the same delegation boundary used when creating or
// updating a custom role to every role-binding mutation. Managing bindings is not itself authority
// to grant all permissions contained by an existing role.
func (r *GORMRepository) verifyDelegableRolePermissions(ctx context.Context, tx *gorm.DB, tenantID, operatorID string, role roleModel) error {
	var permissionIDs []string
	result := tx.Table("authz_role_permission AS role_permission").
		Distinct("role_permission.permission_id").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.tenant_id = ? AND permission.application_id = ? AND permission.status = ?", tenantID, role.ApplicationID, domain.StatusActive).
		Where("role_permission.role_id = ? AND role_permission.effect = ?", role.ID, "ALLOW").
		Pluck("role_permission.permission_id", &permissionIDs)
	if result.Error != nil {
		return fmt.Errorf("load role permissions for delegation: %w", result.Error)
	}
	return r.verifyDelegablePermissions(tx, tenantID, operatorID, operatorAccountIDFromContext(ctx, tenantID, operatorID), role.ApplicationID, permissionIDs)
}

// operatorAccountIDFromContext uses only the principal installed by authentication middleware.
// Direct repository callers without an authenticated principal remain limited to user and inherited
// organization/position grants rather than trusting a caller-supplied account identifier.
func operatorAccountIDFromContext(ctx context.Context, tenantID, operatorID string) string {
	principal, ok := authctx.PrincipalFromContext(ctx)
	if !ok || principal.Tenant.ID != tenantID || principal.User.ID != operatorID {
		return ""
	}
	return strings.TrimSpace(principal.Account.ID)
}

// verifyDelegablePermissions ensures a custom role cannot become a privilege-escalation
// mechanism. The selected permissions must all be effective for the current authenticated
// operator at tenant scope; having role-management permissions alone is not enough.
func (r *GORMRepository) verifyDelegablePermissions(tx *gorm.DB, tenantID, operatorID, operatorAccountID, applicationID string, permissionIDs []string) error {
	if len(permissionIDs) == 0 {
		return nil
	}

	now := time.Now().UTC()
	subjectSQL, subjectArgs := roleBindingSubjectFilter(operatorID, operatorAccountID, now)
	var total int64
	result := tx.Table("authz_role_binding AS binding").
		Distinct("permission.id").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ?", domain.StatusActive).
		Joins("JOIN authz_role_permission AS role_permission ON role_permission.role_id = role.id AND role_permission.effect = ?", "ALLOW").
		Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id AND permission.tenant_id = binding.tenant_id AND permission.application_id = binding.application_id AND permission.status = ?", domain.StatusActive).
		Where("binding.tenant_id = ? AND binding.application_id = ? AND binding.status = ?", tenantID, applicationID, domain.StatusActive).
		Where("binding.scope_type = ? AND binding.scope_id = ?", "TENANT", "").
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
	var rows []permissionReferenceProjection
	result := tx.Table("authz_role_permission AS role_permission").Select("permission.id, permission.code, permission.name").Joins("JOIN authz_permission AS permission ON permission.id = role_permission.permission_id").Where("role_permission.role_id = ? AND role_permission.effect = 'ALLOW'", role.ID).Order("permission.code").Scan(&rows)
	if result.Error != nil {
		return domain.Role{}, fmt.Errorf("list role permissions: %w", result.Error)
	}
	permissions := make([]domain.Reference, 0, len(rows))
	for _, row := range rows {
		permissions = append(permissions, domain.Reference{ID: row.ID, Code: row.Code, Name: row.Name})
	}
	return domain.Role{ID: role.ID, Code: role.Code, Name: role.Name, Description: role.Description, Status: role.Status, BuiltIn: role.BuiltIn, Permissions: permissions, Version: role.Version}, nil
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

// roleBindingSubjectFilter resolves effective role subjects. Organization and position bindings
// apply only while the requesting user has an active, currently effective membership in the
// corresponding active organization unit or position.
func roleBindingSubjectFilter(userID, accountID string, now time.Time) (string, []any) {
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
			domain.StatusActive, domain.StatusActive,
			userID, domain.StatusActive, now, now,
			"ORG_UNIT", "POSITION",
		}
}

func scopeFilter(input application.CheckInput) (string, []any) {
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
	ID, RoleID, RoleCode, RoleName, SubjectType, SubjectID, ScopeType, ScopeID, Status string
	ValidUntil                                                                         *time.Time
	Version                                                                            uint64
}

func bindingProjectionToDomain(row bindingProjection) domain.RoleBinding {
	var scopeID *string
	if row.ScopeType != "TENANT" {
		copy := row.ScopeID
		scopeID = &copy
	}
	return domain.RoleBinding{ID: row.ID, Role: domain.Reference{ID: row.RoleID, Code: row.RoleCode, Name: row.RoleName}, SubjectType: row.SubjectType, Subject: domain.Reference{ID: row.SubjectID, Name: row.SubjectID}, ScopeType: row.ScopeType, ScopeID: scopeID, Status: row.Status, ExpiresAt: row.ValidUntil, Version: row.Version}
}
