// Package positiongrant 为组织岗位实现可复用的授权模板。它有意物化为 TEMPLATE 来源的
// POSITION 绑定，而不是把授权复制到用户：用户只在任职有效期内动态获得这些绑定。
package positiongrant

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/delegation"
	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	activeStatus        = "ACTIVE"
	disabledStatus      = "DISABLED"
	templateStatusDraft = "DRAFT"
	originTemplate      = "TEMPLATE"
	subjectPosition     = "POSITION"
	scopeTenant         = "TENANT"
	scopeEnvironment    = "ENVIRONMENT"
	roleTypeApplication = "APPLICATION"
	roleTypePlatform    = "PLATFORM"
	catalogStatusSynced = "SYNCED"
)

var (
	// ErrNotFound 表示租户内不存在该岗位授权模板或关联记录。
	ErrNotFound = errors.New("position authorization template not found")
	// ErrValidation 表示输入不满足模板/分配的输入契约。
	ErrValidation = errors.New("position authorization template validation failed")
	// ErrConflict 表示乐观锁版本冲突或状态转换冲突。
	ErrConflict = errors.New("position authorization template conflict")
	// ErrForbidden 表示操作者试图通过模板委派本人并不拥有的权限。
	ErrForbidden = errors.New("position authorization template delegation forbidden")
)

// IdentifierGenerator 为模板/分配/物化绑定提供可排序 ULID。
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock 使授权时间戳在测试中可确定。
type Clock interface{ Now() time.Time }

// SystemClock 是生产环境的 UTC 时钟。
type SystemClock struct{}

// Now 返回当前 UTC 时间。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Service 提供岗位授权模板的租户范围内用例：模板 CRUD、岗位分配对账、
// 有效期交叉预览，以及把分配物化为可追溯的 POSITION 角色绑定。
type Service struct {
	db    *gorm.DB
	ids   IdentifierGenerator
	clock Clock
}

// NewService 校验依赖并构造岗位授权模板服务；任一依赖为 nil 即拒绝启动。
func NewService(db *gorm.DB, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if db == nil || ids == nil || clock == nil {
		return nil, errors.New("position authorization template service dependencies must not be nil")
	}
	return &Service{db: db, ids: ids, clock: clock}, nil
}

// TemplateRoleInput 是模板内单个角色条目的输入；作用域只允许 TENANT 或 ENVIRONMENT。
type TemplateRoleInput struct {
	ApplicationID string     `json:"application_id"`
	RoleID        string     `json:"role_id"`
	ScopeType     string     `json:"scope_type"`
	ScopeID       string     `json:"scope_id,omitempty"`
	ValidFrom     *time.Time `json:"valid_from,omitempty"`
	ValidUntil    *time.Time `json:"valid_until,omitempty"`
	Status        string     `json:"status,omitempty"`
}

// Legacy role-inheritance DTOs 仅为迁移兼容保留在内部；对应 HTTP 功能已下线。
type RoleInheritanceInput struct {
	SourceRoleID        string     `json:"source_role_id"`
	TargetApplicationID string     `json:"target_application_id"`
	TargetRoleID        string     `json:"target_role_id"`
	ScopeType           string     `json:"scope_type,omitempty"`
	ScopeID             string     `json:"scope_id,omitempty"`
	ValidFrom           *time.Time `json:"valid_from,omitempty"`
	ValidUntil          *time.Time `json:"valid_until,omitempty"`
	Status              string     `json:"status,omitempty"`
}

// RoleInheritanceView 是平台角色到子系统角色继承映射的展示形态。
type RoleInheritanceView struct {
	MappingID             string     `json:"mapping_id"`
	SourceApplicationID   string     `json:"source_application_id"`
	SourceRoleID          string     `json:"source_role_id"`
	SourceRoleCode        string     `json:"source_role_code"`
	SourceRoleName        string     `json:"source_role_name"`
	TargetApplicationID   string     `json:"target_application_id"`
	TargetApplicationCode string     `json:"target_application_code"`
	TargetApplicationName string     `json:"target_application_name"`
	TargetRoleID          string     `json:"target_role_id"`
	TargetRoleCode        string     `json:"target_role_code"`
	TargetRoleName        string     `json:"target_role_name"`
	ScopeType             string     `json:"scope_type"`
	ScopeID               string     `json:"scope_id,omitempty"`
	ValidFrom             *time.Time `json:"valid_from,omitempty"`
	ValidUntil            *time.Time `json:"valid_until,omitempty"`
	Status                string     `json:"status"`
}

// RoleInheritanceReplaceInput 以完整集合替换某平台角色的继承映射。
type RoleInheritanceReplaceInput struct {
	SourceRoleID string                 `json:"source_role_id"`
	Mappings     []RoleInheritanceInput `json:"mappings"`
}

// TemplateInput 有意不接受 code：模板编码是不可变的系统标识，
// 由创建时从模板 ULID 派生生成。
type TemplateInput struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Status      string              `json:"status"`
	ValidFrom   *time.Time          `json:"valid_from,omitempty"`
	ValidUntil  *time.Time          `json:"valid_until,omitempty"`
	Version     uint64              `json:"version,omitempty"`
	Roles       []TemplateRoleInput `json:"roles"`
}

// AssignmentInput 描述把一个模板分配给岗位的输入；有效期受模板与条目有效期交叉约束。
type AssignmentInput struct {
	TemplateID string     `json:"template_id"`
	Status     string     `json:"status,omitempty"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
}

// ReplaceAssignmentsInput 以完整集合替换一个岗位的模板分配。
type ReplaceAssignmentsInput struct {
	Assignments      []AssignmentInput `json:"assignments"`
	ExpectedRevision string            `json:"expected_revision"`
}

// PreviewInput 是岗位授权预览的输入；EffectiveFrom/EffectiveTo 可模拟未来任职窗口。
type PreviewInput struct {
	UserID               string     `json:"user_id,omitempty"`
	OrgUnitID            string     `json:"org_unit_id,omitempty"`
	PositionID           string     `json:"position_id"`
	EffectiveFrom        *time.Time `json:"effective_from,omitempty"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	InheritAuthorization *bool      `json:"inherit_authorization,omitempty"`
}

// TemplateRoleView 是模板角色条目的展示形态，附带应用与角色的可读信息。
type TemplateRoleView struct {
	ItemID          string     `json:"item_id"`
	ApplicationID   string     `json:"application_id"`
	ApplicationCode string     `json:"application_code"`
	ApplicationName string     `json:"application_name"`
	RoleID          string     `json:"role_id"`
	RoleCode        string     `json:"role_code"`
	RoleName        string     `json:"role_name"`
	ScopeType       string     `json:"scope_type"`
	ScopeID         string     `json:"scope_id,omitempty"`
	ValidFrom       *time.Time `json:"valid_from,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
	Status          string     `json:"status"`
}

// TemplateView 是岗位授权模板的完整展示形态，包含角色集合与影响面计数。
type TemplateView struct {
	TemplateID      string             `json:"template_id"`
	Code            string             `json:"code"`
	Name            string             `json:"name"`
	Description     string             `json:"description,omitempty"`
	Status          string             `json:"status"`
	ValidFrom       *time.Time         `json:"valid_from,omitempty"`
	ValidUntil      *time.Time         `json:"valid_until,omitempty"`
	Version         uint64             `json:"version"`
	Roles           []TemplateRoleView `json:"roles"`
	AssignmentCount int64              `json:"assignment_count"`
	AffectedUsers   int64              `json:"affected_users"`
}

// AssignmentView 是模板到岗位分配的展示形态。
type AssignmentView struct {
	AssignmentID string       `json:"assignment_id"`
	PositionID   string       `json:"position_id"`
	Template     TemplateView `json:"template"`
	Status       string       `json:"status"`
	ValidFrom    *time.Time   `json:"valid_from,omitempty"`
	ValidUntil   *time.Time   `json:"valid_until,omitempty"`
	Version      uint64       `json:"version"`
}

// PreviewRole 是预览中单条将在有效期交集内生效的岗位角色。
type PreviewRole struct {
	ApplicationID   string     `json:"application_id"`
	ApplicationCode string     `json:"application_code"`
	ApplicationName string     `json:"application_name"`
	RoleID          string     `json:"role_id"`
	RoleCode        string     `json:"role_code"`
	RoleName        string     `json:"role_name"`
	TemplateID      string     `json:"template_id"`
	TemplateName    string     `json:"template_name"`
	ValidFrom       *time.Time `json:"valid_from,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
}

// Preview 是岗位授权预览结果：生效角色集合、继承开关以及超出应用角色上限的冲突说明。
type Preview struct {
	PositionID           string        `json:"position_id"`
	InheritAuthorization bool          `json:"inherit_authorization"`
	Roles                []PreviewRole `json:"roles"`
	Conflicts            []string      `json:"conflicts"`
}

// AuthorizationTargetRoleView 是模板可选角色条目及其可分配性标记。
type AuthorizationTargetRoleView struct {
	RoleID     string `json:"role_id"`
	RoleCode   string `json:"role_code"`
	RoleName   string `json:"role_name"`
	RoleType   string `json:"role_type"`
	RoleStatus string `json:"status"`
	Assignable bool   `json:"assignable"`
}

// AuthorizationPositionView 聚合在册岗位及其在册组织上下文，用于分组展示岗位授权
// 专用目录。有意比组织/岗位管理资源窄，不泄露管理元数据。
type AuthorizationPositionView struct {
	PositionID       string `json:"position_id"`
	PositionCode     string `json:"position_code"`
	PositionName     string `json:"position_name"`
	OrgUnitID        string `json:"org_unit_id"`
	OrgUnitCode      string `json:"org_unit_code"`
	OrgUnitName      string `json:"org_unit_name"`
	OrgUnitPath      string `json:"org_unit_path"`
	OrgUnitDepth     uint   `json:"org_unit_depth"`
	OrgUnitSortOrder int    `json:"org_unit_sort_order"`
}

type authorizationPositionRow struct {
	PositionID       string `gorm:"column:position_id"`
	PositionCode     string `gorm:"column:position_code"`
	PositionName     string `gorm:"column:position_name"`
	OrgUnitID        string `gorm:"column:org_unit_id"`
	OrgUnitCode      string `gorm:"column:org_unit_code"`
	OrgUnitName      string `gorm:"column:org_unit_name"`
	OrgUnitPath      string `gorm:"column:org_unit_path"`
	OrgUnitDepth     uint   `gorm:"column:org_unit_depth"`
	OrgUnitSortOrder int    `gorm:"column:org_unit_sort_order"`
}

// AuthorizationTargetView 有意只包含构建岗位授权模板所需的最小信息：
// 绝不把应用管理配置或完整应用授权目录暴露给角色绑定操作者。
type AuthorizationTargetView struct {
	ApplicationID    string `json:"application_id"`
	ApplicationCode  string `json:"application_code"`
	ApplicationName  string `json:"application_name"`
	CatalogVersion   string `json:"catalog_version,omitempty"`
	CatalogSyncState string `json:"catalog_sync_status"`
	// MaxEffectiveRoles 是应用自有目录策略的单用户角色上限；0 表示不限制。只读展示：
	// 让模板操作者在保存映射前即可看到每用户角色预算，避免被后端拒绝。
	MaxEffectiveRoles int                           `json:"max_effective_roles,omitempty"`
	Roles             []AuthorizationTargetRoleView `json:"roles"`
}

type authorizationTargetRow struct {
	ApplicationID     string `gorm:"column:application_id"`
	ApplicationCode   string `gorm:"column:application_code"`
	ApplicationName   string `gorm:"column:application_name"`
	CatalogVersion    string `gorm:"column:catalog_version"`
	CatalogSyncState  string `gorm:"column:catalog_sync_status"`
	MaxEffectiveRoles int    `gorm:"column:max_effective_roles"`
	RoleID            string `gorm:"column:role_id"`
	RoleCode          string `gorm:"column:role_code"`
	RoleName          string `gorm:"column:role_name"`
	RoleType          string `gorm:"column:role_type"`
	RoleStatus        string `gorm:"column:role_status"`
}

type templateModel struct {
	ID, TenantID, Code, Name, Description, Status string
	ValidFrom, ValidUntil                         *time.Time
	Version                                       uint64
	CreatedAt, UpdatedAt                          time.Time
	CreatedBy, UpdatedBy                          *string
}

func (templateModel) TableName() string { return "authz_position_grant_template" }

type templateRoleModel struct {
	ID, TenantID, TemplateID, ApplicationID, RoleID, ScopeType, ScopeID, Status string
	ValidFrom, ValidUntil                                                       *time.Time
	Version                                                                     uint64
	CreatedAt, UpdatedAt                                                        time.Time
	CreatedBy, UpdatedBy                                                        *string
}
type roleInheritanceModel struct {
	ID, TenantID, SourceApplicationID, SourceRoleID, TargetApplicationID, TargetRoleID, ScopeType, ScopeID, Status string
	ValidFrom, ValidUntil                                                                                          *time.Time
	Version                                                                                                        uint64
	CreatedAt, UpdatedAt                                                                                           time.Time
	CreatedBy, UpdatedBy                                                                                           *string
}

func (roleInheritanceModel) TableName() string { return "authz_role_inheritance_mapping" }

func (templateRoleModel) TableName() string { return "authz_position_grant_template_role" }

type assignmentModel struct {
	ID, TenantID, PositionID, TemplateID, Status string
	ValidFrom, ValidUntil                        *time.Time
	Version                                      uint64
	CreatedAt, UpdatedAt                         time.Time
	CreatedBy, UpdatedBy                         *string
}

func (assignmentModel) TableName() string { return "authz_position_grant_template_assignment" }

type roleProjection struct {
	templateRoleModel
	ApplicationCode string `gorm:"column:application_code"`
	ApplicationName string `gorm:"column:application_name"`
	RoleCode        string `gorm:"column:role_code"`
	RoleName        string `gorm:"column:role_name"`
}
type bindingProjection struct {
	ID, ApplicationID, RoleID, SubjectType, SubjectID, ScopeType, ScopeID, Status, OriginItemID string
	ValidFrom, ValidUntil                                                                       *time.Time
	Version                                                                                     uint64
}

// ListAuthorizationTargets 只返回岗位模板可选的角色。平台原生角色以平台库为权威，
// 无需应用自有目录；子系统角色必须在其目录 SYNCED 之后才暴露。把目录状态并入这个
// 专用响应，也避免仅为了构建模板就要求更宽的 platform:application:read 权限。
func (s *Service) ListAuthorizationTargets(ctx context.Context, tenantID string) ([]AuthorizationTargetView, error) {
	var rows []authorizationTargetRow
	err := s.db.WithContext(ctx).
		Table("platform_application AS application").
		Select("application.id AS application_id, application.code AS application_code, application.name AS application_name, COALESCE(catalog.catalog_version, '') AS catalog_version, COALESCE(catalog.sync_status, '') AS catalog_sync_status, COALESCE(policy.max_effective_roles, 0) AS max_effective_roles, role.id AS role_id, role.code AS role_code, role.name AS role_name, role.role_type AS role_type, role.status AS role_status").
		Joins("JOIN authz_role AS role ON role.tenant_id=application.tenant_id AND role.application_id=application.id AND role.status=?", activeStatus).
		Joins("LEFT JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id=application.tenant_id AND catalog.application_id=application.id").
		Joins("LEFT JOIN authz_application_authorization_policy AS policy ON policy.tenant_id=application.tenant_id AND policy.application_id=application.id").
		Where("application.tenant_id=? AND application.status=?", tenantID, activeStatus).
		Where("role.role_type=? OR (role.role_type=? AND catalog.sync_status=?)", roleTypePlatform, roleTypeApplication, catalogStatusSynced).
		Order("application.code ASC, role.code ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list position authorization targets: %w", err)
	}
	return authorizationTargetViews(rows), nil
}

func authorizationTargetViews(rows []authorizationTargetRow) []AuthorizationTargetView {
	targets := make([]AuthorizationTargetView, 0)
	indexes := make(map[string]int)
	for _, row := range rows {
		// 受保护控制面角色不出现在岗位模板的可选目标中。
		if row.RoleType == roleTypePlatform && domain.IsProtectedRoleCode(row.RoleCode) {
			continue
		}
		index, ok := indexes[row.ApplicationID]
		if !ok {
			index = len(targets)
			indexes[row.ApplicationID] = index
			catalogVersion, catalogSyncState := row.CatalogVersion, row.CatalogSyncState
			if row.RoleType == roleTypePlatform {
				catalogVersion, catalogSyncState = "built-in", catalogStatusSynced
			}
			targets = append(targets, AuthorizationTargetView{
				ApplicationID: row.ApplicationID, ApplicationCode: row.ApplicationCode, ApplicationName: row.ApplicationName,
				CatalogVersion: catalogVersion, CatalogSyncState: catalogSyncState,
				MaxEffectiveRoles: row.MaxEffectiveRoles,
				Roles:             make([]AuthorizationTargetRoleView, 0),
			})
		}
		if row.RoleType == roleTypePlatform {
			targets[index].CatalogVersion, targets[index].CatalogSyncState = "built-in", catalogStatusSynced
		}
		targets[index].Roles = append(targets[index].Roles, AuthorizationTargetRoleView{
			RoleID: row.RoleID, RoleCode: row.RoleCode, RoleName: row.RoleName,
			RoleType: row.RoleType, RoleStatus: row.RoleStatus, Assignable: true,
		})
	}
	return targets
}

// ListRoleInheritances 返回持久化的平台角色到子系统角色继承映射。
func (s *Service) ListRoleInheritances(ctx context.Context, tenantID string) ([]RoleInheritanceView, error) {
	var rows []struct {
		roleInheritanceModel
		SourceRoleCode        string `gorm:"column:source_role_code"`
		SourceRoleName        string `gorm:"column:source_role_name"`
		TargetApplicationCode string `gorm:"column:target_application_code"`
		TargetApplicationName string `gorm:"column:target_application_name"`
		TargetRoleCode        string `gorm:"column:target_role_code"`
		TargetRoleName        string `gorm:"column:target_role_name"`
	}
	err := s.db.WithContext(ctx).Table("authz_role_inheritance_mapping AS mapping").Select("mapping.*, source_role.code AS source_role_code, source_role.name AS source_role_name, target_application.code AS target_application_code, target_application.name AS target_application_name, target_role.code AS target_role_code, target_role.name AS target_role_name").
		Joins("JOIN authz_role AS source_role ON source_role.tenant_id=mapping.tenant_id AND source_role.application_id=mapping.source_application_id AND source_role.id=mapping.source_role_id").
		Joins("JOIN platform_application AS target_application ON target_application.tenant_id=mapping.tenant_id AND target_application.id=mapping.target_application_id").
		Joins("JOIN authz_role AS target_role ON target_role.tenant_id=mapping.tenant_id AND target_role.application_id=mapping.target_application_id AND target_role.id=mapping.target_role_id").
		Where("mapping.tenant_id=?", tenantID).Order("source_role.code, target_application.code, target_role.code, mapping.id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list role inheritance mappings: %w", err)
	}
	items := make([]RoleInheritanceView, 0, len(rows))
	for _, row := range rows {
		items = append(items, RoleInheritanceView{MappingID: row.ID, SourceApplicationID: row.SourceApplicationID, SourceRoleID: row.SourceRoleID, SourceRoleCode: row.SourceRoleCode, SourceRoleName: row.SourceRoleName, TargetApplicationID: row.TargetApplicationID, TargetApplicationCode: row.TargetApplicationCode, TargetApplicationName: row.TargetApplicationName, TargetRoleID: row.TargetRoleID, TargetRoleCode: row.TargetRoleCode, TargetRoleName: row.TargetRoleName, ScopeType: row.ScopeType, ScopeID: row.ScopeID, ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil, Status: row.Status})
	}
	return items, nil
}

// ReplaceRoleInheritances 以对账方式整体替换平台角色的继承映射；来源必须是有效的
// 平台角色，所有写操作在租户级模板写事务内完成。
func (s *Service) ReplaceRoleInheritances(ctx context.Context, tenantID, operatorID string, input RoleInheritanceReplaceInput) ([]RoleInheritanceView, error) {
	input.SourceRoleID = strings.TrimSpace(input.SourceRoleID)
	if input.SourceRoleID == "" || len(input.Mappings) > 200 {
		return nil, ErrValidation
	}
	now := s.clock.Now().UTC()
	err := s.templateWriteTransaction(ctx, tenantID, func(tx *gorm.DB) error {
		var source struct{ ID, ApplicationID, RoleType, Status string }
		if err := tx.Table("authz_role").Where("tenant_id=? AND id=?", tenantID, input.SourceRoleID).Take(&source).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrValidation
			}
			return err
		}
		var sourceApp struct{ Code, Status string }
		if err := tx.Table("platform_application").Where("tenant_id=? AND id=?", tenantID, source.ApplicationID).Take(&sourceApp).Error; err != nil {
			return err
		}
		if source.Status != activeStatus || source.RoleType != roleTypePlatform || sourceApp.Code != "platform" || sourceApp.Status != activeStatus {
			return validation("source role must be an active platform role")
		}
		desired := map[string]RoleInheritanceInput{}
		for _, mapping := range input.Mappings {
			mapping.SourceRoleID = input.SourceRoleID
			mapping.TargetApplicationID = strings.TrimSpace(mapping.TargetApplicationID)
			mapping.TargetRoleID = strings.TrimSpace(mapping.TargetRoleID)
			mapping.ScopeType = strings.ToUpper(strings.TrimSpace(mapping.ScopeType))
			mapping.ScopeID = strings.TrimSpace(mapping.ScopeID)
			mapping.Status = strings.ToUpper(strings.TrimSpace(mapping.Status))
			if mapping.ScopeType == "" {
				mapping.ScopeType = scopeTenant
			}
			if mapping.Status == "" {
				mapping.Status = activeStatus
			}
			if mapping.TargetApplicationID == "" || mapping.TargetRoleID == "" || mapping.Status != activeStatus || (mapping.ScopeType != scopeTenant && mapping.ScopeType != scopeEnvironment) || (mapping.ScopeType == scopeEnvironment && mapping.ScopeID == "") {
				return ErrValidation
			}
			var count int64
			if err := tx.Table("authz_role AS role").Joins("JOIN platform_application AS app ON app.tenant_id=role.tenant_id AND app.id=role.application_id AND app.status=?", activeStatus).Joins("LEFT JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id=role.tenant_id AND catalog.application_id=role.application_id").Where("role.tenant_id=? AND role.id=? AND role.application_id=? AND role.status=? AND role.role_type=? AND catalog.sync_status=?", tenantID, mapping.TargetRoleID, mapping.TargetApplicationID, activeStatus, roleTypeApplication, catalogStatusSynced).Count(&count).Error; err != nil {
				return err
			}
			if count != 1 {
				return validation("target role must be an active synchronized subsystem role")
			}
			key := mapping.TargetApplicationID + "\x00" + mapping.TargetRoleID + "\x00" + mapping.ScopeType + "\x00" + mapping.ScopeID
			if _, ok := desired[key]; ok {
				return validation("role inheritance mapping must not repeat the same target role")
			}
			desired[key] = mapping
		}
		if err := validateRoleInheritanceLimits(ctx, tx, tenantID, desired); err != nil {
			return err
		}
		var existing []roleInheritanceModel
		if err := tx.Where("tenant_id=? AND source_role_id=?", tenantID, input.SourceRoleID).Find(&existing).Error; err != nil {
			return err
		}
		byKey := map[string]roleInheritanceModel{}
		for _, row := range existing {
			byKey[row.TargetApplicationID+"\x00"+row.TargetRoleID+"\x00"+row.ScopeType+"\x00"+row.ScopeID] = row
		}
		for _, old := range existing {
			if _, ok := desired[old.TargetApplicationID+"\x00"+old.TargetRoleID+"\x00"+old.ScopeType+"\x00"+old.ScopeID]; !ok && old.Status == activeStatus {
				if err := tx.Model(&roleInheritanceModel{}).Where("id=?", old.ID).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return err
				}
			}
		}
		for key, mapping := range desired {
			if old, ok := byKey[key]; ok {
				if err := tx.Model(&roleInheritanceModel{}).Where("id=?", old.ID).Updates(map[string]any{"status": activeStatus, "valid_from": mapping.ValidFrom, "valid_until": mapping.ValidUntil, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return err
				}
				continue
			}
			id, err := s.ids.New(now)
			if err != nil {
				return err
			}
			row := roleInheritanceModel{ID: id, TenantID: tenantID, SourceApplicationID: source.ApplicationID, SourceRoleID: input.SourceRoleID, TargetApplicationID: mapping.TargetApplicationID, TargetRoleID: mapping.TargetRoleID, ScopeType: mapping.ScopeType, ScopeID: mapping.ScopeID, Status: activeStatus, ValidFrom: mapping.ValidFrom, ValidUntil: mapping.ValidUntil, Version: 1, CreatedAt: now, UpdatedAt: now, CreatedBy: &operatorID, UpdatedBy: &operatorID}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		// 映射变更必须立即作用于已分配模板：否则既有岗位会继续持有过期的物化子系统
		// 绑定，直到下一次无关的模板或分配变更发生。
		var assignments []assignmentModel
		if err := tx.Where("tenant_id=? AND status=?", tenantID, activeStatus).Find(&assignments).Error; err != nil {
			return err
		}
		changedApps := map[string]struct{}{}
		changedPositions := map[string]map[string]struct{}{}
		for _, assignment := range assignments {
			if err := s.syncAssignment(ctx, tx, tenantID, assignment.ID, operatorID, now, changedApps, changedPositions); err != nil {
				return err
			}
		}
		if err := bumpChanged(tx, tenantID, changedApps, now, "role inheritance mapping synchronized"); err != nil {
			return err
		}
		return s.enqueueAffectedUsers(tx, tenantID, changedApps, changedPositions, now)
	})
	if err != nil {
		return nil, err
	}
	return s.ListRoleInheritances(ctx, tenantID)
}

func validateRoleInheritanceLimits(ctx context.Context, tx *gorm.DB, tenantID string, mappings map[string]RoleInheritanceInput) error {
	rolesByApplication := make(map[string]map[string]struct{})
	for _, mapping := range mappings {
		if rolesByApplication[mapping.TargetApplicationID] == nil {
			rolesByApplication[mapping.TargetApplicationID] = make(map[string]struct{})
		}
		rolesByApplication[mapping.TargetApplicationID][mapping.TargetRoleID] = struct{}{}
	}
	limits, err := applicationRoleLimits(ctx, tx, tenantID, mapKeys(rolesByApplication))
	if err != nil {
		return err
	}
	for applicationID, roles := range rolesByApplication {
		if limit := limits[applicationID]; limit > 0 && len(roles) > limit {
			return validation(fmt.Sprintf("application %s allows at most %d inherited roles", applicationID, limit))
		}
	}
	return nil
}

// ListAuthorizationPositions 返回在册岗位，权限边界与模板分配共用的角色绑定权限一致。
// 若调用通用岗位管理端点，会错误地要求角色绑定操作者持有 platform:position:read。
func (s *Service) ListAuthorizationPositions(ctx context.Context, tenantID string) ([]AuthorizationPositionView, error) {
	var rows []authorizationPositionRow
	if err := s.db.WithContext(ctx).
		Table("iam_position AS position").
		Select("position.id AS position_id, position.code AS position_code, position.name AS position_name, organization.id AS org_unit_id, organization.code AS org_unit_code, organization.name AS org_unit_name, organization.path AS org_unit_path, organization.depth AS org_unit_depth, organization.sort_order AS org_unit_sort_order").
		Joins("JOIN iam_org_unit AS organization ON organization.tenant_id=position.tenant_id AND organization.id=position.org_unit_id AND organization.status=?", activeStatus).
		Where("position.tenant_id=? AND position.status=?", tenantID, activeStatus).
		Order("organization.path ASC, organization.sort_order ASC, organization.code ASC, position.name ASC, position.code ASC, position.id ASC").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list position authorization positions: %w", err)
	}
	return authorizationPositionViews(rows), nil
}

func authorizationPositionViews(rows []authorizationPositionRow) []AuthorizationPositionView {
	items := make([]AuthorizationPositionView, 0, len(rows))
	for _, row := range rows {
		items = append(items, AuthorizationPositionView{
			PositionID: row.PositionID, PositionCode: row.PositionCode, PositionName: row.PositionName,
			OrgUnitID: row.OrgUnitID, OrgUnitCode: row.OrgUnitCode, OrgUnitName: row.OrgUnitName,
			OrgUnitPath: row.OrgUnitPath, OrgUnitDepth: row.OrgUnitDepth, OrgUnitSortOrder: row.OrgUnitSortOrder,
		})
	}
	return items
}

// List 返回租户内全部岗位授权模板（含各模板的角色集合与影响面计数）。
func (s *Service) List(ctx context.Context, tenantID string) ([]TemplateView, error) {
	var rows []templateModel
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list position authorization templates: %w", err)
	}
	views := make([]TemplateView, 0, len(rows))
	for _, row := range rows {
		view, err := s.get(ctx, tenantID, row.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// Get 返回单个岗位授权模板的完整展示形态。
func (s *Service) Get(ctx context.Context, tenantID, templateID string) (TemplateView, error) {
	return s.get(ctx, tenantID, templateID)
}
func (s *Service) get(ctx context.Context, tenantID, templateID string) (TemplateView, error) {
	var model templateModel
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, templateID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return TemplateView{}, ErrNotFound
	}
	if err != nil {
		return TemplateView{}, fmt.Errorf("get position authorization template: %w", err)
	}
	roles, err := s.templateRoles(ctx, tenantID, templateID)
	if err != nil {
		return TemplateView{}, err
	}
	var assignments, users int64
	if err := s.db.WithContext(ctx).Table("authz_position_grant_template_assignment").Where("tenant_id = ? AND template_id = ? AND status = ?", tenantID, templateID, activeStatus).Count(&assignments).Error; err != nil {
		return TemplateView{}, fmt.Errorf("count template assignments: %w", err)
	}
	now := s.clock.Now().UTC()
	// 影响人数代表受当前有效模板来源影响的用户；最终跨来源冲突另由权限解析判定。
	// 同时检查来源窗口与物化绑定，避免历史漂移、未生效或已过期的授权被计入。
	if err := s.db.WithContext(ctx).Table("iam_membership AS m").Distinct("m.user_id").
		Joins("JOIN iam_user AS user ON user.tenant_id=m.tenant_id AND user.id=m.user_id AND user.status=? AND user.deleted_at IS NULL AND (user.valid_until IS NULL OR user.valid_until > ?)", activeStatus, now).
		Joins("JOIN iam_org_unit AS organization ON organization.tenant_id=m.tenant_id AND organization.id=m.org_unit_id AND organization.status=?", activeStatus).
		Joins("JOIN iam_position AS position ON position.tenant_id=m.tenant_id AND position.id=m.position_id AND position.org_unit_id=m.org_unit_id AND position.status=?", activeStatus).
		Joins("JOIN authz_position_grant_template_assignment AS a ON a.tenant_id=m.tenant_id AND a.position_id=m.position_id AND a.template_id=? AND a.status=?", templateID, activeStatus).
		Joins("JOIN authz_position_grant_template AS template ON template.tenant_id=a.tenant_id AND template.id=a.template_id AND template.status=?", activeStatus).
		Joins("JOIN authz_role_binding AS binding ON binding.tenant_id=a.tenant_id AND binding.subject_type=? AND binding.subject_id=a.position_id AND binding.grant_origin=? AND binding.origin_id=a.id AND binding.status=?", subjectPosition, originTemplate, activeStatus).
		Joins("JOIN authz_position_grant_template_role AS item ON item.tenant_id=template.tenant_id AND item.template_id=template.id AND item.id=binding.origin_item_id AND item.application_id=binding.application_id AND item.role_id=binding.role_id AND item.status=?", activeStatus).
		Joins("JOIN platform_application AS application ON application.tenant_id=binding.tenant_id AND application.id=binding.application_id AND application.status=?", activeStatus).
		Joins("JOIN authz_role AS role ON role.tenant_id=binding.tenant_id AND role.application_id=binding.application_id AND role.id=binding.role_id AND role.status=?", activeStatus).
		Where("m.tenant_id=? AND m.status=? AND m.inherit_authorization=?", tenantID, activeStatus, true).
		Where("(m.valid_from IS NULL OR m.valid_from <= ?) AND (m.valid_until IS NULL OR m.valid_until > ?)", now, now).
		Where("(a.valid_from IS NULL OR a.valid_from <= ?) AND (a.valid_until IS NULL OR a.valid_until > ?)", now, now).
		Where("(template.valid_from IS NULL OR template.valid_from <= ?) AND (template.valid_until IS NULL OR template.valid_until > ?)", now, now).
		Where("(item.valid_from IS NULL OR item.valid_from <= ?) AND (item.valid_until IS NULL OR item.valid_until > ?)", now, now).
		Where("(binding.valid_from IS NULL OR binding.valid_from <= ?) AND (binding.valid_until IS NULL OR binding.valid_until > ?)", now, now).
		Count(&users).Error; err != nil {
		return TemplateView{}, fmt.Errorf("count template affected users: %w", err)
	}
	return TemplateView{TemplateID: model.ID, Code: model.Code, Name: model.Name, Description: model.Description, Status: model.Status, ValidFrom: model.ValidFrom, ValidUntil: model.ValidUntil, Version: model.Version, Roles: roles, AssignmentCount: assignments, AffectedUsers: users}, nil
}
func (s *Service) templateRoles(ctx context.Context, tenantID, templateID string) ([]TemplateRoleView, error) {
	var rows []roleProjection
	err := s.db.WithContext(ctx).Table("authz_position_grant_template_role AS item").Select("item.*, a.code AS application_code, a.name AS application_name, r.code AS role_code, r.name AS role_name").Joins("JOIN platform_application AS a ON a.id=item.application_id AND a.tenant_id=item.tenant_id").Joins("JOIN authz_role AS r ON r.id=item.role_id AND r.tenant_id=item.tenant_id AND r.application_id=item.application_id").Where("item.tenant_id=? AND item.template_id=? AND item.status=?", tenantID, templateID, activeStatus).Order("a.code, r.code, item.id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list position authorization template roles: %w", err)
	}
	views := make([]TemplateRoleView, 0, len(rows))
	for _, item := range rows {
		views = append(views, TemplateRoleView{ItemID: item.ID, ApplicationID: item.ApplicationID, ApplicationCode: item.ApplicationCode, ApplicationName: item.ApplicationName, RoleID: item.RoleID, RoleCode: item.RoleCode, RoleName: item.RoleName, ScopeType: item.ScopeType, ScopeID: item.ScopeID, ValidFrom: item.ValidFrom, ValidUntil: item.ValidUntil, Status: item.Status})
	}
	return views, nil
}

// Create 创建模板及角色条目；模板编码由 ULID 派生，创建即不可变。
func (s *Service) Create(ctx context.Context, tenantID, operatorID string, input TemplateInput) (TemplateView, error) {
	if err := normalizeTemplateInput(&input); err != nil {
		return TemplateView{}, err
	}
	now := s.clock.Now().UTC()
	id, err := s.ids.New(now)
	if err != nil {
		return TemplateView{}, fmt.Errorf("generate template ID: %w", err)
	}
	err = s.templateWriteTransaction(ctx, tenantID, func(tx *gorm.DB) error {
		row := templateModel{ID: id, TenantID: tenantID, Code: generatedTemplateCode(id), Name: input.Name, Description: input.Description, Status: input.Status, ValidFrom: input.ValidFrom, ValidUntil: input.ValidUntil, Version: 1, CreatedAt: now, UpdatedAt: now, CreatedBy: &operatorID, UpdatedBy: &operatorID}
		if err := tx.Create(&row).Error; err != nil {
			return writeError(err, "create position authorization template")
		}
		return s.saveTemplateItems(ctx, tx, tenantID, id, operatorID, input.Roles, now)
	})
	if err != nil {
		return TemplateView{}, err
	}
	return s.get(ctx, tenantID, id)
}

// Update 在乐观锁下更新模板展示信息、有效期与角色集合，并同步重算已分配岗位的
// 物化绑定；模板编码不变。
func (s *Service) Update(ctx context.Context, tenantID, operatorID, templateID string, input TemplateInput) (TemplateView, error) {
	if err := normalizeTemplateInput(&input); err != nil {
		return TemplateView{}, err
	}
	now := s.clock.Now().UTC()
	changedApps := map[string]struct{}{}
	changedPositions := map[string]map[string]struct{}{}
	err := s.templateWriteTransaction(ctx, tenantID, func(tx *gorm.DB) error {
		var existing templateModel
		// 先加锁再做一致性读取，避免分配物化用到其他分配写入提交之前的快照。
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenantID, templateID).Take(&existing)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if result.Error != nil {
			return fmt.Errorf("get template before update: %w", result.Error)
		}
		if input.Version == 0 || existing.Version != input.Version {
			return ErrConflict
		}
		// 模板编码由系统生成且不可变；更新只能调整展示信息、有效期和角色集合，避免外部
		// 引用因重命名或编辑模板内容而失效。
		if err := updateTemplateRecord(tx, tenantID, templateID, input, operatorID, now); err != nil {
			return err
		}
		if err := s.saveTemplateItems(ctx, tx, tenantID, templateID, operatorID, input.Roles, now); err != nil {
			return err
		}
		return s.syncTemplateAssignments(ctx, tx, tenantID, templateID, operatorID, now, changedApps, changedPositions)
	})
	if err != nil {
		return TemplateView{}, err
	}
	return s.get(ctx, tenantID, templateID)
}

// Delete 以乐观锁停用模板并同步撤销其物化绑定；不做物理删除，保留审计链。
func (s *Service) Delete(ctx context.Context, tenantID, operatorID, templateID string, version uint64) error {
	if version == 0 {
		return validation("version is required")
	}
	now := s.clock.Now().UTC()
	changedApps := map[string]struct{}{}
	changedPositions := map[string]map[string]struct{}{}
	return s.templateWriteTransaction(ctx, tenantID, func(tx *gorm.DB) error {
		result := tx.Model(&templateModel{}).Where("tenant_id=? AND id=? AND version=?", tenantID, templateID, version).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return writeError(result.Error, "disable template")
		}
		if result.RowsAffected == 0 {
			return ErrConflict
		}
		// 模板停用后其岗位分配不能再以 ACTIVE 留在编辑器中。否则下一次保存岗位
		// 映射会把已停用模板一并提交，被 ensureTemplate 以 422 拒绝，表现为
		// “校验未通过”。这里先把分配停用，再统一同步物化绑定。
		if err := tx.Model(&assignmentModel{}).
			Where("tenant_id=? AND template_id=? AND status=?", tenantID, templateID, activeStatus).
			Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return writeError(err, "disable template assignments")
		}
		return s.syncTemplateAssignments(ctx, tx, tenantID, templateID, operatorID, now, changedApps, changedPositions)
	})
}

// ListPositionAssignments 返回岗位的全部模板分配。
func (s *Service) ListPositionAssignments(ctx context.Context, tenantID, positionID string) (AssignmentSetView, error) {
	if err := s.ensurePosition(ctx, tenantID, positionID); err != nil {
		return AssignmentSetView{}, err
	}
	var result AssignmentSetView
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = s.readAssignmentSet(ctx, tx, tenantID, positionID)
		return err
	})
	return result, err
}

// ReplacePositionAssignments 以模板 ID 集合对账岗位分配，并同步物化或撤销
// TEMPLATE 来源的 POSITION 角色绑定。
func (s *Service) ReplacePositionAssignments(ctx context.Context, tenantID, operatorID, positionID string, input ReplaceAssignmentsInput) (AssignmentSetView, error) {
	if len(input.ExpectedRevision) != 67 || !strings.HasPrefix(input.ExpectedRevision, "v1:") {
		return AssignmentSetView{}, validation("expected_revision from the current assignment snapshot is required")
	}
	if err := s.ensurePosition(ctx, tenantID, positionID); err != nil {
		return AssignmentSetView{}, err
	}
	now := s.clock.Now().UTC()
	normalized, err := normalizeAssignments(input.Assignments)
	if err != nil {
		return AssignmentSetView{}, err
	}
	changedApps := map[string]struct{}{}
	changedPositions := map[string]map[string]struct{}{}
	var result AssignmentSetView
	err = s.templateWriteTransaction(ctx, tenantID, func(tx *gorm.DB) error {
		// 以“模板 ID 集合”对账而非物理删除：移除项逻辑停用，已有项复用并递增版本，
		// 新增项创建；每一步同步生成角色绑定，最后统一推进受影响应用的 revision。
		// 即使集合为空也要锁定父岗位；仅靠分配表上的行锁/间隙锁无法在不同隔离级别下
		// 可靠地串行化两个首次写入者。
		var position struct{ ID string }
		if err := tx.Table("iam_position").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("tenant_id=? AND id=? AND status=?", tenantID, positionID, activeStatus).Take(&position).Error; err != nil {
			return writeError(err, "lock assignment position")
		}
		var existing []assignmentModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND position_id=?", tenantID, positionID).Find(&existing).Error; err != nil {
			return fmt.Errorf("load position assignments: %w", err)
		}
		if assignmentSetRevision(tenantID, positionID, existing) != input.ExpectedRevision {
			return ErrConflict
		}
		wanted := map[string]AssignmentInput{}
		for _, a := range normalized {
			wanted[a.TemplateID] = a
		}
		byTemplate := map[string]assignmentModel{}
		for _, a := range existing {
			byTemplate[a.TemplateID] = a
		}
		// 所有新增授权必须在第一次写入前复核，不能先套用一个模板给自己授予权限，
		// 再以该权限通过同批次后续模板的委派校验。
		if err := s.validateAssignmentDelegation(ctx, tx, tenantID, operatorID, positionID, normalized, byTemplate, now); err != nil {
			return err
		}
		for _, a := range existing {
			if _, ok := wanted[a.TemplateID]; ok || a.Status == disabledStatus {
				continue
			}
			if err := tx.Model(&assignmentModel{}).Where("id=?", a.ID).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return writeError(err, "disable position assignment")
			}
			if err := s.syncAssignment(ctx, tx, tenantID, a.ID, operatorID, now, changedApps, changedPositions); err != nil {
				return err
			}
		}
		for _, wantedAssignment := range normalized {
			if old, ok := byTemplate[wantedAssignment.TemplateID]; ok {
				if err := tx.Model(&assignmentModel{}).Where("id=?", old.ID).Updates(map[string]any{"status": wantedAssignment.Status, "valid_from": wantedAssignment.ValidFrom, "valid_until": wantedAssignment.ValidUntil, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return writeError(err, "update position assignment")
				}
				if err := s.syncAssignment(ctx, tx, tenantID, old.ID, operatorID, now, changedApps, changedPositions); err != nil {
					return err
				}
				continue
			}
			id, err := s.ids.New(now)
			if err != nil {
				return fmt.Errorf("generate assignment ID: %w", err)
			}
			row := assignmentModel{ID: id, TenantID: tenantID, PositionID: positionID, TemplateID: wantedAssignment.TemplateID, Status: wantedAssignment.Status, ValidFrom: wantedAssignment.ValidFrom, ValidUntil: wantedAssignment.ValidUntil, Version: 1, CreatedAt: now, UpdatedAt: now, CreatedBy: &operatorID, UpdatedBy: &operatorID}
			if err := tx.Create(&row).Error; err != nil {
				return writeError(err, "create position assignment")
			}
			if err := s.syncAssignment(ctx, tx, tenantID, id, operatorID, now, changedApps, changedPositions); err != nil {
				return err
			}
		}
		if err := bumpChanged(tx, tenantID, changedApps, now, "position authorization assignments synchronized"); err != nil {
			return err
		}
		if err := s.enqueueAffectedUsers(tx, tenantID, changedApps, changedPositions, now); err != nil {
			return err
		}
		var err error
		result, err = s.readAssignmentSet(ctx, tx, tenantID, positionID)
		return err
	})
	if err != nil {
		return AssignmentSetView{}, err
	}
	return result, nil
}

// validateAssignmentDelegation 比较实际物化授权，而非仅比较分配状态。安全的撤销、
// 缩小和不变保存不要求操作者继续持有旧权限；新增、重新激活及扩大的角色必须复核。
func (s *Service) validateAssignmentDelegation(ctx context.Context, tx *gorm.DB, tenantID, operatorID, positionID string, wanted []AssignmentInput, existing map[string]assignmentModel, now time.Time) error {
	// 在任何普通快照读取之前锁定全部目标模板，保持校验与后续物化读取同一版本；
	// 固定锁顺序也避免两个岗位同时套用相反顺序的模板导致交叉等待。
	ordered := append([]AssignmentInput(nil), wanted...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TemplateID < ordered[j].TemplateID })
	templates := make(map[string]templateModel, len(ordered))
	for _, assignment := range ordered {
		var template templateModel
		if err := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenantID, assignment.TemplateID).Take(&template).Error; err != nil {
			return writeError(err, "get assignment template")
		}
		templates[assignment.TemplateID] = template
	}
	for _, assignment := range wanted {
		template := templates[assignment.TemplateID]
		if assignment.Status != activeStatus {
			continue
		}
		if template.Status != activeStatus {
			return fmt.Errorf("%w: 模板未启用（当前 %s）；请先将模板启用为 ACTIVE 再分配岗位", ErrValidation, template.Status)
		}
		var items []templateRoleModel
		if err := tx.Where("tenant_id=? AND template_id=? AND status=?", tenantID, template.ID, activeStatus).Find(&items).Error; err != nil {
			return fmt.Errorf("load assignment template roles: %w", err)
		}
		oldAssignment, hadAssignment := existing[assignment.TemplateID]
		var bindings []bindingProjection
		if hadAssignment && oldAssignment.Status == activeStatus {
			if err := tx.Table("authz_role_binding").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id,application_id,role_id,scope_type,scope_id,status,origin_item_id,valid_from,valid_until").
				Where("tenant_id=? AND subject_type=? AND subject_id=? AND grant_origin=? AND origin_id=?", tenantID, subjectPosition, positionID, originTemplate, oldAssignment.ID).Find(&bindings).Error; err != nil {
				return fmt.Errorf("load existing assignment bindings: %w", err)
			}
		}
		for _, item := range items {
			from, until, ok := intersect(template.ValidFrom, template.ValidUntil, assignment.ValidFrom, assignment.ValidUntil, item.ValidFrom, item.ValidUntil)
			if !ok || (until != nil && !until.After(now)) {
				continue
			}
			var role struct{ RoleType, Code string }
			if err := tx.Table("authz_role AS role").Select("role.role_type, role.code").Where("role.tenant_id=? AND role.application_id=? AND role.id=?", tenantID, item.ApplicationID, item.RoleID).Take(&role).Error; err != nil {
				return writeError(err, "get assignment role")
			}
			if role.RoleType == roleTypePlatform && domain.IsProtectedRoleCode(role.Code) {
				return validation("platform control-plane roles cannot be delegated through position authorization templates")
			}
			covered := false
			for _, binding := range bindings {
				if binding.Status == activeStatus && binding.OriginItemID == item.ID && binding.ApplicationID == item.ApplicationID && binding.RoleID == item.RoleID && binding.ScopeType == item.ScopeType && binding.ScopeID == item.ScopeID &&
					(binding.ValidFrom == nil || (from != nil && !from.Before(*binding.ValidFrom))) &&
					(binding.ValidUntil == nil || (until != nil && !until.After(*binding.ValidUntil))) {
					covered = true
					break
				}
			}
			if covered {
				continue
			}
			if err := delegation.VerifyRolePermissionsForPositionTemplates(ctx, tx, tenantID, operatorID, item.ApplicationID, item.RoleID); err != nil {
				if errors.Is(err, application.ErrForbidden) {
					return ErrForbidden
				}
				return fmt.Errorf("verify assignment role delegation: %w", err)
			}
		}
	}
	return nil
}

// Preview 计算岗位在给定任职窗口内将生效的角色集合；不写库。
// 每条角色取任职、模板、分配与模板条目有效期的交集，并标记超出应用角色上限的冲突。
func (s *Service) Preview(ctx context.Context, tenantID string, input PreviewInput) (Preview, error) {
	input.PositionID = strings.TrimSpace(input.PositionID)
	if input.PositionID == "" {
		return Preview{}, validation("position_id is required")
	}
	if input.EffectiveFrom != nil && input.EffectiveTo != nil && !input.EffectiveTo.After(*input.EffectiveFrom) {
		return Preview{}, validation("effective_to must be later than effective_from")
	}
	if err := s.ensurePosition(ctx, tenantID, input.PositionID); err != nil {
		return Preview{}, err
	}
	inherit := input.InheritAuthorization == nil || *input.InheritAuthorization
	preview := Preview{PositionID: input.PositionID, InheritAuthorization: inherit, Roles: []PreviewRole{}, Conflicts: []string{}}
	if !inherit {
		return preview, nil
	}
	now := s.clock.Now().UTC()
	var rows []struct {
		TemplateID      string     `gorm:"column:template_id"`
		TemplateName    string     `gorm:"column:template_name"`
		ApplicationID   string     `gorm:"column:application_id"`
		ApplicationCode string     `gorm:"column:application_code"`
		ApplicationName string     `gorm:"column:application_name"`
		RoleID          string     `gorm:"column:role_id"`
		RoleCode        string     `gorm:"column:role_code"`
		RoleName        string     `gorm:"column:role_name"`
		TemplateFrom    *time.Time `gorm:"column:template_from"`
		TemplateUntil   *time.Time `gorm:"column:template_until"`
		AssignmentFrom  *time.Time `gorm:"column:assignment_from"`
		AssignmentUntil *time.Time `gorm:"column:assignment_until"`
		ItemFrom        *time.Time `gorm:"column:item_from"`
		ItemUntil       *time.Time `gorm:"column:item_until"`
	}
	err := s.db.WithContext(ctx).Table("authz_position_grant_template_assignment AS assignment").Select("assignment.template_id, template.name AS template_name, item.application_id, application.code AS application_code, application.name AS application_name, item.role_id, role.code AS role_code, role.name AS role_name, template.valid_from AS template_from, template.valid_until AS template_until, assignment.valid_from AS assignment_from, assignment.valid_until AS assignment_until, item.valid_from AS item_from, item.valid_until AS item_until").Joins("JOIN authz_position_grant_template AS template ON template.id=assignment.template_id AND template.tenant_id=assignment.tenant_id AND template.status=?", activeStatus).Joins("JOIN authz_position_grant_template_role AS item ON item.template_id=template.id AND item.tenant_id=template.tenant_id AND item.status=?", activeStatus).Joins("JOIN platform_application AS application ON application.id=item.application_id AND application.tenant_id=item.tenant_id AND application.status=?", activeStatus).Joins("JOIN authz_role AS role ON role.id=item.role_id AND role.application_id=item.application_id AND role.tenant_id=item.tenant_id AND role.status=? AND role.role_type IN ?", activeStatus, []string{roleTypeApplication, roleTypePlatform}).Where("assignment.tenant_id=? AND assignment.position_id=? AND assignment.status=?", tenantID, input.PositionID, activeStatus).Find(&rows).Error
	if err != nil {
		return Preview{}, fmt.Errorf("preview position authorization: %w", err)
	}
	byApp := map[string]map[string]struct{}{}
	applicationLabels := map[string]string{}
	for _, row := range rows {
		// 一个岗位角色只有在预览任职、模板、模板分配和模板条目的有效期存在交集时
		// 才生效；交集边界也会成为最终生成绑定的有效期。
		from, until, ok := intersect(input.EffectiveFrom, input.EffectiveTo, row.TemplateFrom, row.TemplateUntil, row.AssignmentFrom, row.AssignmentUntil, row.ItemFrom, row.ItemUntil)
		if !ok {
			continue
		}
		preview.Roles = append(preview.Roles, PreviewRole{ApplicationID: row.ApplicationID, ApplicationCode: row.ApplicationCode, ApplicationName: row.ApplicationName, RoleID: row.RoleID, RoleCode: row.RoleCode, RoleName: row.RoleName, TemplateID: row.TemplateID, TemplateName: row.TemplateName, ValidFrom: from, ValidUntil: until})
		if byApp[row.ApplicationID] == nil {
			byApp[row.ApplicationID] = map[string]struct{}{}
		}
		byApp[row.ApplicationID][row.RoleID] = struct{}{}
		applicationLabels[row.ApplicationID] = row.ApplicationName
		if strings.TrimSpace(applicationLabels[row.ApplicationID]) == "" {
			applicationLabels[row.ApplicationID] = row.ApplicationCode
		}
	}
	limits, err := applicationRoleLimits(ctx, s.db, tenantID, mapKeys(byApp))
	if err != nil {
		return Preview{}, err
	}
	for applicationID, roles := range byApp {
		if limit := limits[applicationID]; limit > 0 && len(roles) > limit {
			preview.Conflicts = append(preview.Conflicts, fmt.Sprintf("应用「%s」最多允许 %d 个有效角色，当前岗位授权预览为 %d 个；请调整模板或任职关系。", applicationLabels[applicationID], limit, len(roles)))
		}
	}
	sort.Slice(preview.Roles, func(i, j int) bool {
		if preview.Roles[i].ApplicationCode == preview.Roles[j].ApplicationCode {
			return preview.Roles[i].RoleCode < preview.Roles[j].RoleCode
		}
		return preview.Roles[i].ApplicationCode < preview.Roles[j].ApplicationCode
	})
	_ = now
	return preview, nil
}

func (s *Service) saveTemplateItems(ctx context.Context, tx *gorm.DB, tenantID, templateID, operatorID string, inputs []TemplateRoleInput, now time.Time) error {
	if err := s.validateItems(ctx, tx, tenantID, operatorID, inputs); err != nil {
		return err
	}
	return s.upsertTemplateItems(tx, tenantID, templateID, operatorID, inputs, now)
}

func updateTemplateRecord(tx *gorm.DB, tenantID, templateID string, input TemplateInput, operatorID string, now time.Time) error {
	result := tx.Model(&templateModel{}).
		Where("tenant_id=? AND id=? AND version=?", tenantID, templateID, input.Version).
		Updates(map[string]any{
			"name": input.Name, "description": input.Description, "status": input.Status,
			"valid_from": input.ValidFrom, "valid_until": input.ValidUntil,
			"updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return writeError(result.Error, "update template")
	}
	if result.RowsAffected == 0 {
		return ErrConflict
	}
	return nil
}

func (s *Service) upsertTemplateItems(tx *gorm.DB, tenantID, templateID, operatorID string, inputs []TemplateRoleInput, now time.Time) error {
	// 应用、角色和作用域共同构成模板条目的稳定身份。集合对账保留历史 ID 和审计链，
	// 被移除条目仅停用；重新加入同一条目时复用原记录并更新版本。
	var existing []templateRoleModel
	if err := tx.Where("tenant_id=? AND template_id=?", tenantID, templateID).Find(&existing).Error; err != nil {
		return fmt.Errorf("load template roles: %w", err)
	}
	byKey := map[string]templateRoleModel{}
	for _, item := range existing {
		byKey[templateRoleKey(item.ApplicationID, item.RoleID, item.ScopeType, item.ScopeID)] = item
	}
	desired := map[string]TemplateRoleInput{}
	for _, item := range inputs {
		desired[templateRoleKey(item.ApplicationID, item.RoleID, item.ScopeType, item.ScopeID)] = item
	}
	for _, old := range existing {
		if _, ok := desired[templateRoleKey(old.ApplicationID, old.RoleID, old.ScopeType, old.ScopeID)]; ok || old.Status == disabledStatus {
			continue
		}
		if err := tx.Model(&templateRoleModel{}).Where("id=?", old.ID).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return writeError(err, "disable removed template role")
		}
	}
	for _, item := range inputs {
		if old, ok := byKey[templateRoleKey(item.ApplicationID, item.RoleID, item.ScopeType, item.ScopeID)]; ok {
			if err := tx.Model(&templateRoleModel{}).Where("id=?", old.ID).Updates(map[string]any{"status": item.Status, "valid_from": item.ValidFrom, "valid_until": item.ValidUntil, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return writeError(err, "update template role")
			}
			continue
		}
		id, err := s.ids.New(now)
		if err != nil {
			return fmt.Errorf("generate template role ID: %w", err)
		}
		row := templateRoleModel{ID: id, TenantID: tenantID, TemplateID: templateID, ApplicationID: item.ApplicationID, RoleID: item.RoleID, ScopeType: item.ScopeType, ScopeID: item.ScopeID, Status: item.Status, ValidFrom: item.ValidFrom, ValidUntil: item.ValidUntil, Version: 1, CreatedAt: now, UpdatedAt: now, CreatedBy: &operatorID, UpdatedBy: &operatorID}
		if err := tx.Create(&row).Error; err != nil {
			return writeError(err, "create template role")
		}
	}
	return nil
}
func (s *Service) syncTemplateAssignments(ctx context.Context, tx *gorm.DB, tenantID, templateID, operatorID string, now time.Time, changedApps map[string]struct{}, changedPositions map[string]map[string]struct{}) error {
	var assignments []assignmentModel
	if err := tx.Where("tenant_id=? AND template_id=?", tenantID, templateID).Find(&assignments).Error; err != nil {
		return fmt.Errorf("load template assignments: %w", err)
	}
	for _, a := range assignments {
		if err := s.syncAssignment(ctx, tx, tenantID, a.ID, operatorID, now, changedApps, changedPositions); err != nil {
			return err
		}
	}
	if err := bumpChanged(tx, tenantID, changedApps, now, "position authorization template synchronized"); err != nil {
		return err
	}
	return s.enqueueAffectedUsers(tx, tenantID, changedApps, changedPositions, now)
}
func (s *Service) syncAssignment(ctx context.Context, tx *gorm.DB, tenantID, assignmentID, operatorID string, now time.Time, changedApps map[string]struct{}, changedPositions map[string]map[string]struct{}) error {
	// 模板分配不是查询时临时拼接，而是物化为来源可追踪的 POSITION 角色绑定。
	// origin_id/origin_item_id 将每条绑定精确关联回分配和模板项，便于幂等对账与撤销。
	var assignment assignmentModel
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenantID, assignmentID).Take(&assignment).Error
	if err != nil {
		return writeError(err, "get position assignment")
	}
	var template templateModel
	err = tx.Where("tenant_id=? AND id=?", tenantID, assignment.TemplateID).Take(&template).Error
	if err != nil {
		return writeError(err, "get assignment template")
	}
	var existing []bindingProjection
	if err := tx.Table("authz_role_binding").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id,application_id,role_id,subject_type,subject_id,scope_type,scope_id,status,origin_item_id,valid_from,valid_until,version").Where("tenant_id=? AND grant_origin=? AND origin_id=?", tenantID, originTemplate, assignment.ID).Find(&existing).Error; err != nil {
		return fmt.Errorf("load generated bindings: %w", err)
	}
	var items []templateRoleModel
	if assignment.Status == activeStatus && template.Status == activeStatus {
		if err := tx.Where("tenant_id=? AND template_id=? AND status=?", tenantID, template.ID, activeStatus).Find(&items).Error; err != nil {
			return fmt.Errorf("load active template roles: %w", err)
		}
	}
	desired := map[string]templateRoleModel{}
	for _, item := range items {
		if _, _, ok := intersect(template.ValidFrom, template.ValidUntil, assignment.ValidFrom, assignment.ValidUntil, item.ValidFrom, item.ValidUntil); ok {
			desired[item.ID] = item
		}
	}
	byItem := map[string]bindingProjection{}
	for _, b := range existing {
		if item, ok := desired[b.OriginItemID]; ok && generatedBindingMatches(b, item, assignment.PositionID) {
			byItem[b.OriginItemID] = b
		}
	}
	for _, binding := range existing {
		item, wanted := desired[binding.OriginItemID]
		if (wanted && generatedBindingMatches(binding, item, assignment.PositionID)) || binding.Status == disabledStatus {
			continue
		}
		if err := tx.Table("authz_role_binding").Where("id=?", binding.ID).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return writeError(err, "disable generated role binding")
		}
		markChangedPosition(changedApps, changedPositions, binding.ApplicationID, assignment.PositionID)
		if binding.SubjectType != subjectPosition || binding.SubjectID != assignment.PositionID {
			if err := s.enqueueDisplacedSubject(tx, tenantID, binding, now); err != nil {
				return err
			}
		}
	}
	for itemID, item := range desired {
		from, until, _ := intersect(template.ValidFrom, template.ValidUntil, assignment.ValidFrom, assignment.ValidUntil, item.ValidFrom, item.ValidUntil)
		if old, ok := byItem[itemID]; ok {
			if old.Status == activeStatus && sameTime(old.ValidFrom, from) && sameTime(old.ValidUntil, until) {
				continue
			}
			if err := tx.Table("authz_role_binding").Where("id=?", old.ID).Updates(map[string]any{"status": activeStatus, "valid_from": from, "valid_until": until, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return writeError(err, "update generated role binding")
			}
			markChangedPosition(changedApps, changedPositions, item.ApplicationID, assignment.PositionID)
			continue
		}
		id, err := s.ids.New(now)
		if err != nil {
			return fmt.Errorf("generate template role binding ID: %w", err)
		}
		row := map[string]any{"id": id, "tenant_id": tenantID, "application_id": item.ApplicationID, "role_id": item.RoleID, "subject_type": subjectPosition, "subject_id": assignment.PositionID, "scope_type": item.ScopeType, "scope_id": item.ScopeID, "valid_from": from, "valid_until": until, "status": activeStatus, "grant_origin": originTemplate, "origin_id": assignment.ID, "origin_item_id": item.ID, "version": 1, "created_at": now, "created_by": operatorID, "updated_at": now, "updated_by": operatorID}
		if err := tx.Table("authz_role_binding").Create(row).Error; err != nil {
			return writeError(err, "create generated role binding")
		}
		markChangedPosition(changedApps, changedPositions, item.ApplicationID, assignment.PositionID)
	}
	return nil
}

// enqueueAffectedUsers 为每个岗位派生授权发生变化的用户记录一条持久化重算请求。
// 它运行在调用方的授权事务内，保证物化绑定提交与其 Keycloak 投影请求不会分叉。
func (s *Service) enqueueAffectedUsers(tx *gorm.DB, tenantID string, applications map[string]struct{}, positionsByApplication map[string]map[string]struct{}, now time.Time) error {
	if len(applications) == 0 || len(positionsByApplication) == 0 {
		return nil
	}
	for _, applicationID := range stringSetKeys(applications) {
		var environmentIDs []string
		if err := tx.Table("keycloak_application_client_mapping").
			Where("tenant_id=? AND application_id=? AND status=?", tenantID, applicationID, "SYNCED").
			Order("environment_id ASC").Pluck("environment_id", &environmentIDs).Error; err != nil {
			return fmt.Errorf("load Keycloak Client mapping targets: %w", err)
		}
		positionIDs := stringSetKeys(positionsByApplication[applicationID])
		if len(positionIDs) == 0 {
			continue
		}
		var userIDs []string
		if err := tx.Table("iam_membership AS membership").
			Distinct("membership.user_id").
			Joins("JOIN iam_user AS user ON user.tenant_id=membership.tenant_id AND user.id=membership.user_id AND user.status=?", activeStatus).
			Joins("JOIN iam_org_unit AS organization ON organization.tenant_id=membership.tenant_id AND organization.id=membership.org_unit_id AND organization.status=?", activeStatus).
			Joins("JOIN iam_position AS position ON position.tenant_id=membership.tenant_id AND position.id=membership.position_id AND position.org_unit_id=membership.org_unit_id AND position.status=?", activeStatus).
			Where("membership.tenant_id=? AND membership.position_id IN ? AND membership.status=? AND membership.inherit_authorization=?", tenantID, positionIDs, activeStatus, true).
			Where("(membership.valid_from IS NULL OR membership.valid_from <= ?) AND (membership.valid_until IS NULL OR membership.valid_until > ?)", now, now).
			Pluck("membership.user_id", &userIDs).Error; err != nil {
			return fmt.Errorf("load users affected by position authorization: %w", err)
		}
		for _, userID := range userIDs {
			for _, environmentID := range stringSetKeys(mapFromStrings(environmentIDs)) {
				id, err := s.ids.New(now)
				if err != nil {
					return fmt.Errorf("generate Keycloak authorization outbox ID: %w", err)
				}
				if err := tx.Table("keycloak_authorization_outbox").Create(map[string]any{
					"id": id, "tenant_id": tenantID, "identity_id": userID, "application_id": applicationID, "environment_id": environmentID,
					"event_type": "AUTHORIZATION_CHANGED", "authorization_revision": 0, "status": "PENDING", "available_at": now,
					"attempts": 0, "created_at": now,
				}).Error; err != nil {
					return fmt.Errorf("enqueue Keycloak authorization recalculation: %w", err)
				}
			}
		}
	}
	return nil
}

func markChangedPosition(applications map[string]struct{}, positionsByApplication map[string]map[string]struct{}, applicationID, positionID string) {
	applications[applicationID] = struct{}{}
	if positionsByApplication[applicationID] == nil {
		positionsByApplication[applicationID] = map[string]struct{}{}
	}
	positionsByApplication[applicationID][positionID] = struct{}{}
}

// expandInheritedTemplateRoles 为每条生效的继承映射合成一个模板条目。映射 ID 充当
// origin_item_id，使映射变更可对账、可审计，且不覆盖来源平台角色绑定本身。
func (s *Service) expandInheritedTemplateRoles(ctx context.Context, tx *gorm.DB, tenantID string, items []templateRoleModel) ([]templateRoleModel, error) {
	platformRoles := make([]string, 0)
	for _, item := range items {
		var roleType string
		if err := tx.WithContext(ctx).Table("authz_role").Select("role_type").Where("tenant_id=? AND application_id=? AND id=?", tenantID, item.ApplicationID, item.RoleID).Scan(&roleType).Error; err != nil {
			return nil, err
		}
		if roleType == roleTypePlatform {
			platformRoles = append(platformRoles, item.RoleID)
		}
	}
	if len(platformRoles) == 0 {
		return nil, nil
	}
	var mappings []roleInheritanceModel
	if err := tx.WithContext(ctx).Where("tenant_id=? AND source_role_id IN ? AND status=?", tenantID, platformRoles, activeStatus).Find(&mappings).Error; err != nil {
		return nil, fmt.Errorf("load role inheritance mappings: %w", err)
	}
	result := make([]templateRoleModel, 0, len(mappings))
	for _, mapping := range mappings {
		result = append(result, templateRoleModel{ID: mapping.ID, TenantID: tenantID, ApplicationID: mapping.TargetApplicationID, RoleID: mapping.TargetRoleID, ScopeType: mapping.ScopeType, ScopeID: mapping.ScopeID, Status: activeStatus, ValidFrom: mapping.ValidFrom, ValidUntil: mapping.ValidUntil, Version: 1})
	}
	return result, nil
}
func bumpChanged(tx *gorm.DB, tenantID string, applications map[string]struct{}, now time.Time, reason string) error {
	// 一个事务可能改动同一应用的多条生成绑定；按应用去重后只推进一次 revision，
	// 既保证缓存失效，又避免 revision 增量依赖模板条目数量。
	for appID := range applications {
		row := map[string]any{"tenant_id": tenantID, "application_id": appID, "revision": 1, "changed_at": now, "change_reason": reason}
		if err := tx.Table("authz_policy_revision").Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}}, DoUpdates: clause.Assignments(map[string]any{"revision": gorm.Expr("revision + 1"), "changed_at": now, "change_reason": reason})}).Create(row).Error; err != nil {
			return fmt.Errorf("bump application authorization revision: %w", err)
		}
	}
	return nil
}

func (s *Service) validateItems(ctx context.Context, tx *gorm.DB, tenantID, operatorID string, items []TemplateRoleInput) error {
	if err := validateTemplateRoleSet(items); err != nil {
		return err
	}
	for _, item := range items {
		var role struct {
			ID       string
			RoleType string
			Code     string
		}
		query := tx.WithContext(ctx).
			Table("authz_role AS role").
			Select("role.id, role.role_type, role.code").
			Joins("JOIN platform_application AS application ON application.id=role.application_id AND application.tenant_id=role.tenant_id AND application.status=?", activeStatus).
			Joins("LEFT JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id=role.tenant_id AND catalog.application_id=role.application_id").
			Where("role.tenant_id=? AND role.id=? AND role.application_id=? AND role.status=?", tenantID, item.RoleID, item.ApplicationID, activeStatus).
			Where("role.role_type=? OR (role.role_type=? AND catalog.sync_status=?)", roleTypePlatform, roleTypeApplication, catalogStatusSynced)
		if err := query.Take(&role).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return validation("application role must exist, be active, assignable and belong to the selected application; subsystem catalogs must be synchronized")
			}
			return fmt.Errorf("validate template role: %w", err)
		}
		// 控制面受保护角色（super-admin/emergency-admin/break-glass）只能由租户级超级管理员
		// 直接绑定，禁止通过岗位授权模板委派；即使平台安全管理员可以管理普通模板角色，
		// 也不能把上述受保护角色塞进模板。
		if role.RoleType == roleTypePlatform && domain.IsProtectedRoleCode(role.Code) {
			return validation("platform control-plane roles cannot be delegated through position authorization templates")
		}
		// 禁止受保护角色并不能覆盖权限扩大：持有 role-binding:update 的人仍然可以把本人
		// 并不拥有的普通角色挂到自己的有效继承岗位上。岗位模板与岗位映射使用专用策略：
		// 超级管理员和平台安全管理员可以豁免逐权限委派判定，其他操作者仍必须持有目标
		// 角色的全部权限。
		if err := delegation.VerifyRolePermissionsForPositionTemplates(ctx, tx, tenantID, operatorID, item.ApplicationID, item.RoleID); err != nil {
			if errors.Is(err, application.ErrForbidden) {
				return ErrForbidden
			}
			return fmt.Errorf("verify template role delegation: %w", err)
		}
		if item.ScopeType == scopeEnvironment {
			var envCount int64
			if err := tx.WithContext(ctx).Table("platform_application_environment").Where("tenant_id=? AND application_id=? AND id=? AND status=?", tenantID, item.ApplicationID, item.ScopeID, activeStatus).Count(&envCount).Error; err != nil {
				return fmt.Errorf("validate template environment: %w", err)
			}
			if envCount != 1 {
				return validation("environment scope must reference an active environment of the application")
			}
		}
	}
	activeRoles := activeRoleIDsByApplication(items)
	limits, err := applicationRoleLimits(ctx, tx, tenantID, mapKeys(activeRoles))
	if err != nil {
		return err
	}
	for applicationID, roleIDs := range activeRoles {
		if limit := limits[applicationID]; limit > 0 && len(roleIDs) > limit {
			return validation(fmt.Sprintf("application %s allows at most %d effective roles", applicationID, limit))
		}
	}
	return nil
}

// validateTemplateRoleSet 禁止同一应用角色重复出现，即使作用域不同：角色即有效角色
// 身份，作用域只是约束而不是第二个可独立分配的角色。以此防止重复的 TEMPLATE 绑定。
func validateTemplateRoleSet(items []TemplateRoleInput) error {
	seen := map[string]struct{}{}
	for _, item := range items {
		key := templateRoleKey(item.ApplicationID, item.RoleID, "", "")
		if _, ok := seen[key]; ok {
			return validation("template roles must not repeat the same application role across scopes")
		}
		seen[key] = struct{}{}
	}
	return nil
}

// templateRoleKey 是模板角色的规范身份。作用域属于持久化条目身份的一部分；
// validateTemplateRoleSet 有意传空作用域，以拒绝跨作用域重复分配同一应用角色。
func templateRoleKey(applicationID, roleID, scopeType, scopeID string) string {
	return applicationID + "\x00" + roleID + "\x00" + scopeType + "\x00" + scopeID
}

func activeRoleIDsByApplication(items []TemplateRoleInput) map[string]map[string]struct{} {
	result := map[string]map[string]struct{}{}
	for _, item := range items {
		if item.Status != activeStatus {
			continue
		}
		if result[item.ApplicationID] == nil {
			result[item.ApplicationID] = map[string]struct{}{}
		}
		result[item.ApplicationID][item.RoleID] = struct{}{}
	}
	return result
}

type applicationRoleLimitRow struct {
	ApplicationID     string `gorm:"column:application_id"`
	MaxEffectiveRoles int    `gorm:"column:max_effective_roles"`
}

func applicationRoleLimits(ctx context.Context, database *gorm.DB, tenantID string, applicationIDs []string) (map[string]int, error) {
	limits := map[string]int{}
	if len(applicationIDs) == 0 {
		return limits, nil
	}
	var rows []applicationRoleLimitRow
	if err := database.WithContext(ctx).Table("authz_application_authorization_policy").Select("application_id, max_effective_roles").Where("tenant_id=? AND application_id IN ?", tenantID, applicationIDs).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load application authorization role limits: %w", err)
	}
	for _, row := range rows {
		limits[row.ApplicationID] = row.MaxEffectiveRoles
	}
	return limits, nil
}

func mapKeys(values map[string]map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func stringSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	return keys
}

func mapFromStrings(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}
func (s *Service) ensurePosition(ctx context.Context, tenantID, positionID string) error {
	var count int64
	err := s.db.WithContext(ctx).Table("iam_position").Where("tenant_id=? AND id=?", tenantID, positionID).Count(&count).Error
	if err != nil {
		return fmt.Errorf("check position: %w", err)
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}
func (s *Service) ensureTemplate(ctx context.Context, tx *gorm.DB, tenantID, templateID string) error {
	var status string
	err := tx.WithContext(ctx).Table("authz_position_grant_template").Select("status").Where("tenant_id=? AND id=?", tenantID, templateID).Take(&status).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("check template: %w", err)
	}
	// DRAFT 模板可保存编辑但物化为零绑定；放行分配会让任职用户永远命中不了模板
	// 授权（表现为“分配成功却不生效”）。在分配入口直接拒绝，迫使其先启用模板。
	if status != activeStatus {
		return fmt.Errorf("%w: 模板未启用（当前 %s）；请先将模板启用为 ACTIVE 再分配岗位", ErrValidation, status)
	}
	return nil
}
func generatedTemplateCode(id string) string {
	return "POSITION-TEMPLATE-" + strings.ToUpper(strings.TrimSpace(id))
}

func normalizeTemplateInput(in *TemplateInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.Status = strings.ToUpper(strings.TrimSpace(in.Status))
	if in.Status == "" {
		in.Status = templateStatusDraft
	}
	if in.Name == "" || len(in.Name) > 128 {
		return validation("name is required")
	}
	if len([]rune(in.Description)) > 1000 {
		return validation("description exceeds the length limit")
	}
	if in.Status != activeStatus && in.Status != disabledStatus && in.Status != templateStatusDraft {
		return validation("status must be DRAFT, ACTIVE or DISABLED")
	}
	if in.ValidFrom != nil && in.ValidUntil != nil && !in.ValidUntil.After(*in.ValidFrom) {
		return validation("valid_until must be later than valid_from")
	}
	for i := range in.Roles {
		role := &in.Roles[i]
		role.ApplicationID = strings.TrimSpace(role.ApplicationID)
		role.RoleID = strings.TrimSpace(role.RoleID)
		role.ScopeType = strings.ToUpper(strings.TrimSpace(role.ScopeType))
		role.ScopeID = strings.TrimSpace(role.ScopeID)
		role.Status = strings.ToUpper(strings.TrimSpace(role.Status))
		if role.ApplicationID == "" || role.RoleID == "" {
			return validation("application_id and role_id are required")
		}
		if len(role.ApplicationID) > 128 || len(role.RoleID) > 128 || len(role.ScopeID) > 128 {
			return validation("role identifiers exceed the length limit")
		}
		if role.ScopeType == "" {
			role.ScopeType = scopeTenant
		}
		if role.ScopeType != "TENANT" && role.ScopeType != scopeEnvironment {
			return validation("scope_type must be TENANT or ENVIRONMENT")
		}
		if role.ScopeType == scopeTenant {
			role.ScopeID = ""
		}
		if role.ScopeType == scopeEnvironment && role.ScopeID == "" {
			return validation("scope_id is required for ENVIRONMENT scope")
		}
		if role.Status == "" {
			role.Status = activeStatus
		}
		if role.Status != activeStatus && role.Status != disabledStatus {
			return validation("role status must be ACTIVE or DISABLED")
		}
		if role.ValidFrom != nil && role.ValidUntil != nil && !role.ValidUntil.After(*role.ValidFrom) {
			return validation("template role valid_until must be later than valid_from")
		}
	}
	return nil
}
func normalizeAssignments(inputs []AssignmentInput) ([]AssignmentInput, error) {
	seen := map[string]struct{}{}
	out := make([]AssignmentInput, 0, len(inputs))
	for _, item := range inputs {
		item.TemplateID = strings.TrimSpace(item.TemplateID)
		item.Status = strings.ToUpper(strings.TrimSpace(item.Status))
		if item.Status == "" {
			item.Status = activeStatus
		}
		if item.TemplateID == "" {
			return nil, validation("template_id is required")
		}
		if item.Status != activeStatus && item.Status != disabledStatus {
			return nil, validation("assignment status must be ACTIVE or DISABLED")
		}
		if item.ValidFrom != nil && item.ValidUntil != nil && !item.ValidUntil.After(*item.ValidFrom) {
			return nil, validation("assignment valid_until must be later than valid_from")
		}
		if _, ok := seen[item.TemplateID]; ok {
			return nil, validation("a position cannot contain the same template twice")
		}
		seen[item.TemplateID] = struct{}{}
		out = append(out, item)
	}
	return out, nil
}
func intersect(values ...*time.Time) (*time.Time, *time.Time, bool) {
	var from, until *time.Time
	for i, v := range values {
		if v == nil {
			continue
		}
		if i%2 == 0 {
			if from == nil || v.After(*from) {
				copy := v.UTC()
				from = &copy
			}
		} else {
			if until == nil || v.Before(*until) {
				copy := v.UTC()
				until = &copy
			}
		}
	}
	if from != nil && until != nil && !until.After(*from) {
		return from, until, false
	}
	return from, until, true
}
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }
func writeError(err error, action string) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return fmt.Errorf("%s: %w", action, err)
}
