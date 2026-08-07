// Package positiongrant implements reusable authorization templates for organization positions.
// It deliberately materializes TEMPLATE-origin POSITION bindings rather than copying grants to
// users. Users receive those bindings dynamically only through an active appointment.
package positiongrant

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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
	ErrNotFound   = errors.New("position authorization template not found")
	ErrValidation = errors.New("position authorization template validation failed")
	ErrConflict   = errors.New("position authorization template conflict")
)

type IdentifierGenerator interface {
	New(time.Time) (string, error)
}
type Clock interface{ Now() time.Time }
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type Service struct {
	db    *gorm.DB
	ids   IdentifierGenerator
	clock Clock
}

func NewService(db *gorm.DB, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if db == nil || ids == nil || clock == nil {
		return nil, errors.New("position authorization template service dependencies must not be nil")
	}
	return &Service{db: db, ids: ids, clock: clock}, nil
}

type TemplateRoleInput struct {
	ApplicationID string     `json:"application_id"`
	RoleID        string     `json:"role_id"`
	ScopeType     string     `json:"scope_type"`
	ScopeID       string     `json:"scope_id,omitempty"`
	ValidFrom     *time.Time `json:"valid_from,omitempty"`
	ValidUntil    *time.Time `json:"valid_until,omitempty"`
	Status        string     `json:"status,omitempty"`
}

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

type RoleInheritanceReplaceInput struct {
	SourceRoleID string                 `json:"source_role_id"`
	Mappings     []RoleInheritanceInput `json:"mappings"`
}

// TemplateInput deliberately does not accept a code. Template codes are immutable system
// identifiers generated from the template ULID when the template is created.
type TemplateInput struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Status      string              `json:"status"`
	ValidFrom   *time.Time          `json:"valid_from,omitempty"`
	ValidUntil  *time.Time          `json:"valid_until,omitempty"`
	Version     uint64              `json:"version,omitempty"`
	Roles       []TemplateRoleInput `json:"roles"`
}
type AssignmentInput struct {
	TemplateID string     `json:"template_id"`
	Status     string     `json:"status,omitempty"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
}
type ReplaceAssignmentsInput struct {
	Assignments []AssignmentInput `json:"assignments"`
}
type PreviewInput struct {
	UserID               string     `json:"user_id,omitempty"`
	OrgUnitID            string     `json:"org_unit_id,omitempty"`
	PositionID           string     `json:"position_id"`
	EffectiveFrom        *time.Time `json:"effective_from,omitempty"`
	EffectiveTo          *time.Time `json:"effective_to,omitempty"`
	InheritAuthorization *bool      `json:"inherit_authorization,omitempty"`
}

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
type AssignmentView struct {
	AssignmentID string       `json:"assignment_id"`
	PositionID   string       `json:"position_id"`
	Template     TemplateView `json:"template"`
	Status       string       `json:"status"`
	ValidFrom    *time.Time   `json:"valid_from,omitempty"`
	ValidUntil   *time.Time   `json:"valid_until,omitempty"`
	Version      uint64       `json:"version"`
}
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
type Preview struct {
	PositionID           string        `json:"position_id"`
	InheritAuthorization bool          `json:"inherit_authorization"`
	Roles                []PreviewRole `json:"roles"`
	Conflicts            []string      `json:"conflicts"`
}

type AuthorizationTargetRoleView struct {
	RoleID     string `json:"role_id"`
	RoleCode   string `json:"role_code"`
	RoleName   string `json:"role_name"`
	RoleType   string `json:"role_type"`
	RoleStatus string `json:"status"`
	Assignable bool   `json:"assignable"`
}

// AuthorizationPositionView contains the active position and its active organization context
// needed to group the task-specific assignment catalog. It deliberately remains narrower than
// the organization- and position-management resources.
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

// AuthorizationTargetView is intentionally limited to the information needed when building a
// position authorization template. It must not expose application management configuration or a
// full application authorization catalog to a role-binding operator.
type AuthorizationTargetView struct {
	ApplicationID    string                        `json:"application_id"`
	ApplicationCode  string                        `json:"application_code"`
	ApplicationName  string                        `json:"application_name"`
	CatalogVersion   string                        `json:"catalog_version,omitempty"`
	CatalogSyncState string                        `json:"catalog_sync_status"`
	Roles            []AuthorizationTargetRoleView `json:"roles"`
}

type authorizationTargetRow struct {
	ApplicationID    string `gorm:"column:application_id"`
	ApplicationCode  string `gorm:"column:application_code"`
	ApplicationName  string `gorm:"column:application_name"`
	CatalogVersion   string `gorm:"column:catalog_version"`
	CatalogSyncState string `gorm:"column:catalog_sync_status"`
	RoleID           string `gorm:"column:role_id"`
	RoleCode         string `gorm:"column:role_code"`
	RoleName         string `gorm:"column:role_name"`
	RoleType         string `gorm:"column:role_type"`
	RoleStatus       string `gorm:"column:role_status"`
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
	ID, ApplicationID, RoleID, ScopeType, ScopeID, Status, OriginItemID string
	ValidFrom, ValidUntil                                               *time.Time
	Version                                                             uint64
}

// ListAuthorizationTargets returns only roles that can be selected by a position template.
// Platform-native roles are authoritative in the platform database and therefore do not require
// an application-owned catalog. Subsystem roles are exposed only after their catalog is SYNCED.
// Keeping the catalog state in this task-specific response also avoids requiring the broader
// platform:application:read permission merely to build a position template.
func (s *Service) ListAuthorizationTargets(ctx context.Context, tenantID string) ([]AuthorizationTargetView, error) {
	var rows []authorizationTargetRow
	err := s.db.WithContext(ctx).
		Table("platform_application AS application").
		Select("application.id AS application_id, application.code AS application_code, application.name AS application_name, COALESCE(catalog.catalog_version, '') AS catalog_version, COALESCE(catalog.sync_status, '') AS catalog_sync_status, role.id AS role_id, role.code AS role_code, role.name AS role_name, role.role_type AS role_type, role.status AS role_status").
		Joins("JOIN authz_role AS role ON role.tenant_id=application.tenant_id AND role.application_id=application.id AND role.status=?", activeStatus).
		Joins("LEFT JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id=application.tenant_id AND catalog.application_id=application.id").
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
				Roles: make([]AuthorizationTargetRoleView, 0),
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

// ListRoleInheritances returns the durable platform-role to subsystem-role mappings.
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

func (s *Service) ReplaceRoleInheritances(ctx context.Context, tenantID, operatorID string, input RoleInheritanceReplaceInput) ([]RoleInheritanceView, error) {
	input.SourceRoleID = strings.TrimSpace(input.SourceRoleID)
	if input.SourceRoleID == "" || len(input.Mappings) > 200 {
		return nil, ErrValidation
	}
	now := s.clock.Now().UTC()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		// Mapping changes must take effect for already assigned templates immediately;
		// otherwise existing positions would keep stale materialized subsystem bindings
		// until an unrelated template or assignment change occurs.
		var assignments []assignmentModel
		if err := tx.Where("tenant_id=? AND status=?", tenantID, activeStatus).Find(&assignments).Error; err != nil {
			return err
		}
		changedApps := map[string]struct{}{}
		for _, assignment := range assignments {
			if err := s.syncAssignment(ctx, tx, tenantID, assignment.ID, operatorID, now, changedApps); err != nil {
				return err
			}
		}
		if err := bumpChanged(tx, tenantID, changedApps, now, "role inheritance mapping synchronized"); err != nil {
			return err
		}
		return nil
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

// ListAuthorizationPositions returns active positions for the same role-binding
// permission boundary used by template assignment. Calling the general position-management
// endpoint would incorrectly require platform:position:read from a role-binding operator.
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
	if err := s.db.WithContext(ctx).Table("iam_membership AS m").Distinct("m.user_id").Joins("JOIN authz_position_grant_template_assignment AS a ON a.tenant_id=m.tenant_id AND a.position_id=m.position_id AND a.template_id=? AND a.status=?", templateID, activeStatus).Where("m.tenant_id=? AND m.status=?", tenantID, activeStatus).Count(&users).Error; err != nil {
		return TemplateView{}, fmt.Errorf("count template affected users: %w", err)
	}
	return TemplateView{TemplateID: model.ID, Code: model.Code, Name: model.Name, Description: model.Description, Status: model.Status, ValidFrom: model.ValidFrom, ValidUntil: model.ValidUntil, Version: model.Version, Roles: roles, AssignmentCount: assignments, AffectedUsers: users}, nil
}
func (s *Service) templateRoles(ctx context.Context, tenantID, templateID string) ([]TemplateRoleView, error) {
	var rows []roleProjection
	err := s.db.WithContext(ctx).Table("authz_position_grant_template_role AS item").Select("item.*, a.code AS application_code, a.name AS application_name, r.code AS role_code, r.name AS role_name").Joins("JOIN platform_application AS a ON a.id=item.application_id AND a.tenant_id=item.tenant_id").Joins("JOIN authz_role AS r ON r.id=item.role_id AND r.tenant_id=item.tenant_id AND r.application_id=item.application_id").Where("item.tenant_id=? AND item.template_id=?", tenantID, templateID).Order("a.code, r.code, item.id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list position authorization template roles: %w", err)
	}
	views := make([]TemplateRoleView, 0, len(rows))
	for _, item := range rows {
		views = append(views, TemplateRoleView{ItemID: item.ID, ApplicationID: item.ApplicationID, ApplicationCode: item.ApplicationCode, ApplicationName: item.ApplicationName, RoleID: item.RoleID, RoleCode: item.RoleCode, RoleName: item.RoleName, ScopeType: item.ScopeType, ScopeID: item.ScopeID, ValidFrom: item.ValidFrom, ValidUntil: item.ValidUntil, Status: item.Status})
	}
	return views, nil
}
func (s *Service) Create(ctx context.Context, tenantID, operatorID string, input TemplateInput) (TemplateView, error) {
	if err := normalizeTemplateInput(&input); err != nil {
		return TemplateView{}, err
	}
	now := s.clock.Now().UTC()
	id, err := s.ids.New(now)
	if err != nil {
		return TemplateView{}, fmt.Errorf("generate template ID: %w", err)
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
func (s *Service) Update(ctx context.Context, tenantID, operatorID, templateID string, input TemplateInput) (TemplateView, error) {
	if err := normalizeTemplateInput(&input); err != nil {
		return TemplateView{}, err
	}
	now := s.clock.Now().UTC()
	changedApps := map[string]struct{}{}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing templateModel
		result := tx.Where("tenant_id=? AND id=?", tenantID, templateID).Take(&existing)
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
		return s.syncTemplateAssignments(ctx, tx, tenantID, templateID, operatorID, now, changedApps)
	})
	if err != nil {
		return TemplateView{}, err
	}
	return s.get(ctx, tenantID, templateID)
}
func (s *Service) Delete(ctx context.Context, tenantID, operatorID, templateID string, version uint64) error {
	if version == 0 {
		return validation("version is required")
	}
	now := s.clock.Now().UTC()
	changedApps := map[string]struct{}{}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&templateModel{}).Where("tenant_id=? AND id=? AND version=?", tenantID, templateID, version).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return writeError(result.Error, "disable template")
		}
		if result.RowsAffected == 0 {
			return ErrConflict
		}
		return s.syncTemplateAssignments(ctx, tx, tenantID, templateID, operatorID, now, changedApps)
	})
}
func (s *Service) ListPositionAssignments(ctx context.Context, tenantID, positionID string) ([]AssignmentView, error) {
	if err := s.ensurePosition(ctx, tenantID, positionID); err != nil {
		return nil, err
	}
	// 岗位已在上方验证，AssignmentView 也不需要岗位名称，因此直接读取 assignmentModel。
	// 避免把带别名的 a.* 扫描进嵌入模型：GORM 对该形态填充不稳定，曾导致 TemplateID
	// 为空，使实际保存成功后的响应加载误报 PLATFORM_NOT_FOUND。
	var rows []assignmentModel
	err := s.db.WithContext(ctx).
		Where("tenant_id=? AND position_id=?", tenantID, positionID).
		Order("created_at DESC,id DESC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list position template assignments: %w", err)
	}
	views := make([]AssignmentView, 0, len(rows))
	for _, row := range rows {
		t, err := s.get(ctx, tenantID, row.TemplateID)
		if err != nil {
			return nil, err
		}
		views = append(views, AssignmentView{AssignmentID: row.ID, PositionID: row.PositionID, Template: t, Status: row.Status, ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil, Version: row.Version})
	}
	return views, nil
}
func (s *Service) ReplacePositionAssignments(ctx context.Context, tenantID, operatorID, positionID string, input ReplaceAssignmentsInput) ([]AssignmentView, error) {
	if err := s.ensurePosition(ctx, tenantID, positionID); err != nil {
		return nil, err
	}
	now := s.clock.Now().UTC()
	normalized, err := normalizeAssignments(input.Assignments)
	if err != nil {
		return nil, err
	}
	changedApps := map[string]struct{}{}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 以“模板 ID 集合”对账而非物理删除：移除项逻辑停用，已有项复用并递增版本，
		// 新增项创建；每一步同步生成角色绑定，最后统一推进受影响应用的 revision。
		var existing []assignmentModel
		if err := tx.Where("tenant_id=? AND position_id=?", tenantID, positionID).Find(&existing).Error; err != nil {
			return fmt.Errorf("load position assignments: %w", err)
		}
		wanted := map[string]AssignmentInput{}
		for _, a := range normalized {
			wanted[a.TemplateID] = a
		}
		byTemplate := map[string]assignmentModel{}
		for _, a := range existing {
			byTemplate[a.TemplateID] = a
		}
		for _, a := range existing {
			if _, ok := wanted[a.TemplateID]; ok || a.Status == disabledStatus {
				continue
			}
			if err := tx.Model(&assignmentModel{}).Where("id=?", a.ID).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
				return writeError(err, "disable position assignment")
			}
			if err := s.syncAssignment(ctx, tx, tenantID, a.ID, operatorID, now, changedApps); err != nil {
				return err
			}
		}
		for _, wantedAssignment := range normalized {
			if err := s.ensureTemplate(ctx, tx, tenantID, wantedAssignment.TemplateID); err != nil {
				return err
			}
			if old, ok := byTemplate[wantedAssignment.TemplateID]; ok {
				if err := tx.Model(&assignmentModel{}).Where("id=?", old.ID).Updates(map[string]any{"status": wantedAssignment.Status, "valid_from": wantedAssignment.ValidFrom, "valid_until": wantedAssignment.ValidUntil, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
					return writeError(err, "update position assignment")
				}
				if err := s.syncAssignment(ctx, tx, tenantID, old.ID, operatorID, now, changedApps); err != nil {
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
			if err := s.syncAssignment(ctx, tx, tenantID, id, operatorID, now, changedApps); err != nil {
				return err
			}
		}
		return bumpChanged(tx, tenantID, changedApps, now, "position authorization assignments synchronized")
	})
	if err != nil {
		return nil, err
	}
	return s.ListPositionAssignments(ctx, tenantID, positionID)
}
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
		AssignmentFrom  *time.Time `gorm:"column:assignment_from"`
		AssignmentUntil *time.Time `gorm:"column:assignment_until"`
		ItemFrom        *time.Time `gorm:"column:item_from"`
		ItemUntil       *time.Time `gorm:"column:item_until"`
	}
	err := s.db.WithContext(ctx).Table("authz_position_grant_template_assignment AS assignment").Select("assignment.template_id, template.name AS template_name, item.application_id, application.code AS application_code, application.name AS application_name, item.role_id, role.code AS role_code, role.name AS role_name, assignment.valid_from AS assignment_from, assignment.valid_until AS assignment_until, item.valid_from AS item_from, item.valid_until AS item_until").Joins("JOIN authz_position_grant_template AS template ON template.id=assignment.template_id AND template.tenant_id=assignment.tenant_id AND template.status=?", activeStatus).Joins("JOIN authz_position_grant_template_role AS item ON item.template_id=template.id AND item.tenant_id=template.tenant_id AND item.status=?", activeStatus).Joins("JOIN platform_application AS application ON application.id=item.application_id AND application.tenant_id=item.tenant_id AND application.status=?", activeStatus).Joins("JOIN authz_role AS role ON role.id=item.role_id AND role.application_id=item.application_id AND role.tenant_id=item.tenant_id AND role.status=? AND role.role_type IN ?", activeStatus, []string{roleTypeApplication, roleTypePlatform}).Where("assignment.tenant_id=? AND assignment.position_id=? AND assignment.status=?", tenantID, input.PositionID, activeStatus).Find(&rows).Error
	if err != nil {
		return Preview{}, fmt.Errorf("preview position authorization: %w", err)
	}
	byApp := map[string]map[string]struct{}{}
	applicationLabels := map[string]string{}
	for _, row := range rows {
		// 一个岗位角色只有在预览任职、模板、模板分配和模板条目的有效期存在交集时
		// 才生效；交集边界也会成为最终生成绑定的有效期。
		from, until, ok := intersect(input.EffectiveFrom, input.EffectiveTo, row.AssignmentFrom, row.AssignmentUntil, row.ItemFrom, row.ItemUntil)
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
	if err := s.validateItems(ctx, tx, tenantID, inputs); err != nil {
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
func (s *Service) syncTemplateAssignments(ctx context.Context, tx *gorm.DB, tenantID, templateID, operatorID string, now time.Time, changedApps map[string]struct{}) error {
	var assignments []assignmentModel
	if err := tx.Where("tenant_id=? AND template_id=?", tenantID, templateID).Find(&assignments).Error; err != nil {
		return fmt.Errorf("load template assignments: %w", err)
	}
	for _, a := range assignments {
		if err := s.syncAssignment(ctx, tx, tenantID, a.ID, operatorID, now, changedApps); err != nil {
			return err
		}
	}
	return bumpChanged(tx, tenantID, changedApps, now, "position authorization template synchronized")
}
func (s *Service) syncAssignment(ctx context.Context, tx *gorm.DB, tenantID, assignmentID, operatorID string, now time.Time, changedApps map[string]struct{}) error {
	// 模板分配不是查询时临时拼接，而是物化为来源可追踪的 POSITION 角色绑定。
	// origin_id/origin_item_id 将每条绑定精确关联回分配和模板项，便于幂等对账与撤销。
	var assignment assignmentModel
	err := tx.Where("tenant_id=? AND id=?", tenantID, assignmentID).Take(&assignment).Error
	if err != nil {
		return writeError(err, "get position assignment")
	}
	var template templateModel
	err = tx.Where("tenant_id=? AND id=?", tenantID, assignment.TemplateID).Take(&template).Error
	if err != nil {
		return writeError(err, "get assignment template")
	}
	var existing []bindingProjection
	if err := tx.Table("authz_role_binding").Select("id,application_id,role_id,scope_type,scope_id,status,origin_item_id,valid_from,valid_until,version").Where("tenant_id=? AND subject_type=? AND subject_id=? AND grant_origin=? AND origin_id=?", tenantID, subjectPosition, assignment.PositionID, originTemplate, assignment.ID).Find(&existing).Error; err != nil {
		return fmt.Errorf("load generated bindings: %w", err)
	}
	var items []templateRoleModel
	if assignment.Status == activeStatus && template.Status == activeStatus {
		if err := tx.Where("tenant_id=? AND template_id=? AND status=?", tenantID, template.ID, activeStatus).Find(&items).Error; err != nil {
			return fmt.Errorf("load active template roles: %w", err)
		}
		var expanded []templateRoleModel
		expanded, err = s.expandInheritedTemplateRoles(ctx, tx, tenantID, items)
		if err != nil {
			return err
		}
		items = append(items, expanded...)
	}
	desired := map[string]templateRoleModel{}
	for _, item := range items {
		if _, _, ok := intersect(template.ValidFrom, template.ValidUntil, assignment.ValidFrom, assignment.ValidUntil, item.ValidFrom, item.ValidUntil); ok {
			desired[item.ID] = item
		}
	}
	byItem := map[string]bindingProjection{}
	for _, b := range existing {
		byItem[b.OriginItemID] = b
	}
	for _, binding := range existing {
		if _, ok := desired[binding.OriginItemID]; ok || binding.Status == disabledStatus {
			continue
		}
		if err := tx.Table("authz_role_binding").Where("id=?", binding.ID).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return writeError(err, "disable generated role binding")
		}
		changedApps[binding.ApplicationID] = struct{}{}
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
			changedApps[item.ApplicationID] = struct{}{}
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
		changedApps[item.ApplicationID] = struct{}{}
	}
	return nil
}

// expandInheritedTemplateRoles adds one synthetic template item per active mapping.
// The mapping ID becomes origin_item_id, so changing a mapping can be reconciled and
// audited without overwriting the source platform-role binding.
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

func (s *Service) validateItems(ctx context.Context, tx *gorm.DB, tenantID string, items []TemplateRoleInput) error {
	if err := validateTemplateRoleSet(items); err != nil {
		return err
	}
	for _, item := range items {
		var count int64
		query := tx.WithContext(ctx).
			Table("authz_role AS role").
			Joins("JOIN platform_application AS application ON application.id=role.application_id AND application.tenant_id=role.tenant_id AND application.status=?", activeStatus).
			Joins("LEFT JOIN authz_authorization_catalog AS catalog ON catalog.tenant_id=role.tenant_id AND catalog.application_id=role.application_id").
			Where("role.tenant_id=? AND role.id=? AND role.application_id=? AND role.status=?", tenantID, item.RoleID, item.ApplicationID, activeStatus).
			Where("role.role_type=? OR (role.role_type=? AND catalog.sync_status=?)", roleTypePlatform, roleTypeApplication, catalogStatusSynced)
		if err := query.Count(&count).Error; err != nil {
			return fmt.Errorf("validate template role: %w", err)
		}
		if count != 1 {
			return validation("application role must exist, be active, assignable and belong to the selected application; subsystem catalogs must be synchronized")
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

// validateTemplateRoleSet disallows the same application role from being entered twice even
// when scopes differ. A role is an effective-role identity; scope is a restriction, not a
// second independently assignable role. This prevents duplicate TEMPLATE bindings.
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

// templateRoleKey is the canonical identity of a template role. Scope is part of the
// persisted item identity, while validateTemplateRoleSet intentionally passes empty scope
// to reject assigning the same application role more than once across scopes.
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
	var count int64
	err := tx.WithContext(ctx).Table("authz_position_grant_template").Where("tenant_id=? AND id=?", tenantID, templateID).Count(&count).Error
	if err != nil {
		return fmt.Errorf("check template: %w", err)
	}
	if count != 1 {
		return ErrNotFound
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
