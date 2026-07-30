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
	RoleID   string `json:"role_id"`
	RoleCode string `json:"role_code"`
	RoleName string `json:"role_name"`
}

// AuthorizationPositionView contains exactly the active position fields needed to assign a
// position authorization template. It intentionally omits organization-management details.
type AuthorizationPositionView struct {
	PositionID   string `json:"position_id"`
	PositionCode string `json:"position_code"`
	PositionName string `json:"position_name"`
}

type authorizationPositionRow struct {
	PositionID   string `gorm:"column:position_id"`
	PositionCode string `gorm:"column:position_code"`
	PositionName string `gorm:"column:position_name"`
}

// AuthorizationTargetView is intentionally limited to the information needed when building a
// position authorization template. It must not expose application management configuration or a
// full application authorization catalog to a role-binding operator.
type AuthorizationTargetView struct {
	ApplicationID   string                        `json:"application_id"`
	ApplicationCode string                        `json:"application_code"`
	ApplicationName string                        `json:"application_name"`
	Roles           []AuthorizationTargetRoleView `json:"roles"`
}

type authorizationTargetRow struct {
	ApplicationID   string `gorm:"column:application_id"`
	ApplicationCode string `gorm:"column:application_code"`
	ApplicationName string `gorm:"column:application_name"`
	RoleID          string `gorm:"column:role_id"`
	RoleCode        string `gorm:"column:role_code"`
	RoleName        string `gorm:"column:role_name"`
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
type assignmentProjection struct {
	assignmentModel
	PositionName string `gorm:"column:position_name"`
}
type bindingProjection struct {
	ID, ApplicationID, RoleID, ScopeType, ScopeID, Status, OriginItemID string
	ValidFrom, ValidUntil                                               *time.Time
	Version                                                             uint64
}

// ListAuthorizationTargets returns only active applications with active, non-compatibility
// roles. It shares the role-binding permission boundary used by position templates and avoids
// requiring broad application-management or authorization-catalog read access.
func (s *Service) ListAuthorizationTargets(ctx context.Context, tenantID string) ([]AuthorizationTargetView, error) {
	var rows []authorizationTargetRow
	err := s.db.WithContext(ctx).
		Table("platform_application AS application").
		Select("application.id AS application_id, application.code AS application_code, application.name AS application_name, role.id AS role_id, role.code AS role_code, role.name AS role_name").
		Joins("JOIN authz_role AS role ON role.tenant_id=application.tenant_id AND role.application_id=application.id AND role.status=? AND role.role_type=?", activeStatus, "APPLICATION").
		Where("application.tenant_id=? AND application.status=?", tenantID, activeStatus).
		Order("application.code ASC, role.code ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list position authorization targets: %w", err)
	}
	targets := make([]AuthorizationTargetView, 0)
	indexes := make(map[string]int)
	for _, row := range rows {
		index, ok := indexes[row.ApplicationID]
		if !ok {
			index = len(targets)
			indexes[row.ApplicationID] = index
			targets = append(targets, AuthorizationTargetView{
				ApplicationID: row.ApplicationID, ApplicationCode: row.ApplicationCode, ApplicationName: row.ApplicationName,
				Roles: make([]AuthorizationTargetRoleView, 0),
			})
		}
		targets[index].Roles = append(targets[index].Roles, AuthorizationTargetRoleView{RoleID: row.RoleID, RoleCode: row.RoleCode, RoleName: row.RoleName})
	}
	return targets, nil
}

// ListAuthorizationPositions returns active positions for the same role-binding
// permission boundary used by template assignment. Calling the general position-management
// endpoint would incorrectly require platform:position:read from a role-binding operator.
func (s *Service) ListAuthorizationPositions(ctx context.Context, tenantID string) ([]AuthorizationPositionView, error) {
	var rows []authorizationPositionRow
	if err := s.db.WithContext(ctx).
		Table("iam_position").
		Select("id AS position_id, code AS position_code, name AS position_name").
		Where("tenant_id=? AND status=?", tenantID, activeStatus).
		Order("name ASC, code ASC").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list position authorization positions: %w", err)
	}
	items := make([]AuthorizationPositionView, 0, len(rows))
	for _, row := range rows {
		items = append(items, AuthorizationPositionView{PositionID: row.PositionID, PositionCode: row.PositionCode, PositionName: row.PositionName})
	}
	return items, nil
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
		if err := s.validateItems(ctx, tx, tenantID, input.Roles); err != nil {
			return err
		}
		row := templateModel{ID: id, TenantID: tenantID, Code: generatedTemplateCode(id), Name: input.Name, Description: input.Description, Status: input.Status, ValidFrom: input.ValidFrom, ValidUntil: input.ValidUntil, Version: 1, CreatedAt: now, UpdatedAt: now, CreatedBy: &operatorID, UpdatedBy: &operatorID}
		if err := tx.Create(&row).Error; err != nil {
			return writeError(err, "create position authorization template")
		}
		return s.upsertTemplateItems(tx, tenantID, id, operatorID, input.Roles, now)
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
		if err := s.validateItems(ctx, tx, tenantID, input.Roles); err != nil {
			return err
		}
		// Code is immutable: updates may change the template content, never its system-generated identifier.
		result = tx.Model(&templateModel{}).Where("tenant_id=? AND id=? AND version=?", tenantID, templateID, input.Version).Updates(map[string]any{"name": input.Name, "description": input.Description, "status": input.Status, "valid_from": input.ValidFrom, "valid_until": input.ValidUntil, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return writeError(result.Error, "update template")
		}
		if result.RowsAffected == 0 {
			return ErrConflict
		}
		if err := s.upsertTemplateItems(tx, tenantID, templateID, operatorID, input.Roles, now); err != nil {
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
	var rows []assignmentProjection
	err := s.db.WithContext(ctx).Table("authz_position_grant_template_assignment AS a").Select("a.*, p.name AS position_name").Joins("JOIN iam_position AS p ON p.id=a.position_id AND p.tenant_id=a.tenant_id").Where("a.tenant_id=? AND a.position_id=?", tenantID, positionID).Order("a.created_at DESC,a.id DESC").Find(&rows).Error
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
	err := s.db.WithContext(ctx).Table("authz_position_grant_template_assignment AS assignment").Select("assignment.template_id, template.name AS template_name, item.application_id, application.code AS application_code, application.name AS application_name, item.role_id, role.code AS role_code, role.name AS role_name, assignment.valid_from AS assignment_from, assignment.valid_until AS assignment_until, item.valid_from AS item_from, item.valid_until AS item_until").Joins("JOIN authz_position_grant_template AS template ON template.id=assignment.template_id AND template.tenant_id=assignment.tenant_id AND template.status=?", activeStatus).Joins("JOIN authz_position_grant_template_role AS item ON item.template_id=template.id AND item.tenant_id=template.tenant_id AND item.status=?", activeStatus).Joins("JOIN platform_application AS application ON application.id=item.application_id AND application.tenant_id=item.tenant_id AND application.status=?", activeStatus).Joins("JOIN authz_role AS role ON role.id=item.role_id AND role.application_id=item.application_id AND role.tenant_id=item.tenant_id AND role.status=? AND role.role_type=?", activeStatus, "APPLICATION").Where("assignment.tenant_id=? AND assignment.position_id=? AND assignment.status=?", tenantID, input.PositionID, activeStatus).Find(&rows).Error
	if err != nil {
		return Preview{}, fmt.Errorf("preview position authorization: %w", err)
	}
	byApp := map[string]map[string]struct{}{}
	applicationLabels := map[string]string{}
	for _, row := range rows {
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

func (s *Service) upsertTemplateItems(tx *gorm.DB, tenantID, templateID, operatorID string, inputs []TemplateRoleInput, now time.Time) error {
	var existing []templateRoleModel
	if err := tx.Where("tenant_id=? AND template_id=?", tenantID, templateID).Find(&existing).Error; err != nil {
		return fmt.Errorf("load template roles: %w", err)
	}
	key := func(v TemplateRoleInput) string {
		return v.ApplicationID + "\x00" + v.RoleID + "\x00" + v.ScopeType + "\x00" + v.ScopeID
	}
	byKey := map[string]templateRoleModel{}
	for _, item := range existing {
		byKey[item.ApplicationID+"\x00"+item.RoleID+"\x00"+item.ScopeType+"\x00"+item.ScopeID] = item
	}
	desired := map[string]TemplateRoleInput{}
	for _, item := range inputs {
		desired[key(item)] = item
	}
	for _, old := range existing {
		if _, ok := desired[old.ApplicationID+"\x00"+old.RoleID+"\x00"+old.ScopeType+"\x00"+old.ScopeID]; ok || old.Status == disabledStatus {
			continue
		}
		if err := tx.Model(&templateRoleModel{}).Where("id=?", old.ID).Updates(map[string]any{"status": disabledStatus, "updated_at": now, "updated_by": operatorID, "version": gorm.Expr("version + 1")}).Error; err != nil {
			return writeError(err, "disable removed template role")
		}
	}
	for _, item := range inputs {
		if old, ok := byKey[key(item)]; ok {
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
func bumpChanged(tx *gorm.DB, tenantID string, applications map[string]struct{}, now time.Time, reason string) error {
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
		query := tx.WithContext(ctx).Table("authz_role AS role").Joins("JOIN platform_application AS application ON application.id=role.application_id AND application.tenant_id=role.tenant_id AND application.status=?", activeStatus).Where("role.tenant_id=? AND role.id=? AND role.application_id=? AND role.status=? AND role.role_type=?", tenantID, item.RoleID, item.ApplicationID, activeStatus, "APPLICATION")
		if err := query.Count(&count).Error; err != nil {
			return fmt.Errorf("validate template role: %w", err)
		}
		if count != 1 {
			return validation("application role must exist, be active, synchronized and belong to the selected application")
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
		key := item.ApplicationID + "\x00" + item.RoleID
		if _, ok := seen[key]; ok {
			return validation("template roles must not repeat the same application role across scopes")
		}
		seen[key] = struct{}{}
	}
	return nil
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
