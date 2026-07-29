// Package applicationaccess provides application-aware authorization for OAuth/OIDC clients.
//
// The package deliberately resolves access from the OAuth client registration instead of from a
// hard-coded subsystem name.  This keeps roles and permissions isolated by application while
// allowing one user to hold more than one role in the same application.
package applicationaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	activeStatus         = "ACTIVE"
	disabledStatus       = "DISABLED"
	subjectTypeUser      = "USER"
	scopeTypeTenant      = "TENANT"
	scopeTypeEnvironment = "ENVIRONMENT"
)

var (
	ErrNotFound      = errors.New("application authorization resource not found")
	ErrNotConfigured = errors.New("user application access is not configured")
	ErrValidation    = errors.New("application authorization validation failed")
	ErrAccessDenied  = errors.New("user is not authorized for application")
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
	audit AuditRecorder
}

// NewService keeps the audit recorder optional for callers that do not install the platform audit
// pipeline. The variadic form preserves the existing three-argument construction contract while
// allowing bootstrap to provide one recorder.
func NewService(db *gorm.DB, ids IdentifierGenerator, clock Clock, audit ...AuditRecorder) (*Service, error) {
	if db == nil || ids == nil || clock == nil {
		return nil, errors.New("application authorization service dependencies must not be nil")
	}
	if len(audit) > 1 {
		return nil, errors.New("application authorization service accepts at most one audit recorder")
	}
	var recorder AuditRecorder
	if len(audit) == 1 {
		recorder = audit[0]
	}
	return &Service{db: db, ids: ids, clock: clock, audit: recorder}, nil
}

type RoleView struct {
	Code            string     `json:"code"`
	Name            string     `json:"name"`
	ScopeType       string     `json:"scope_type"`
	EnvironmentCode string     `json:"environment_code,omitempty"`
	ValidFrom       *time.Time `json:"valid_from,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
}

type RoleInput struct {
	RoleCode        string     `json:"role_code"`
	ScopeType       string     `json:"scope_type"`
	EnvironmentCode string     `json:"environment_code,omitempty"`
	ValidFrom       *time.Time `json:"valid_from,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
}

type Access struct {
	ApplicationCode string     `json:"application_code"`
	EnvironmentCode string     `json:"environment_code,omitempty"`
	Roles           []RoleView `json:"roles"`
	// Role is retained for clients of the old SYS-004 access endpoint.  New clients should use
	// Roles, which can contain more than one application role.
	Role                 RoleView `json:"role"`
	RolePermissions      []string `json:"role_permissions"`
	CustomPermissions    []string `json:"custom_permissions"`
	EffectivePermissions []string `json:"effective_permissions"`
	RoleConfigHash       string   `json:"role_config_hash"`
	AuthzRevision        uint64   `json:"authz_revision"`
}

type UpdateAccessInput struct {
	TenantID                  string
	UserID                    string
	OperatorID                string
	Roles                     []RoleInput
	RolesProvided             bool
	CustomPermissions         []string
	CustomPermissionsProvided bool
}

type DeleteAccessInput struct {
	TenantID   string
	UserID     string
	OperatorID string
}

type TokenAuthorization struct {
	ApplicationCode string
	EnvironmentCode string
	TenantID        string
	Roles           []string
	Permissions     []string
	RoleConfigHash  string
	AuthzRevision   uint64
}

type clientApplicationRow struct {
	ClientID        string `gorm:"column:client_id"`
	TenantID        string `gorm:"column:tenant_id"`
	ApplicationID   string `gorm:"column:application_id"`
	ApplicationCode string `gorm:"column:application_code"`
	EnvironmentID   string `gorm:"column:environment_id"`
	EnvironmentCode string `gorm:"column:environment_code"`
}

type applicationRow struct {
	ID       string `gorm:"column:id"`
	TenantID string `gorm:"column:tenant_id"`
	Code     string `gorm:"column:code"`
}

func (applicationRow) TableName() string { return "platform_application" }

type roleRow struct {
	ID   string `gorm:"column:id"`
	Code string `gorm:"column:code"`
	Name string `gorm:"column:name"`
}

type assignedRoleRow struct {
	RoleID          string     `gorm:"column:role_id"`
	Code            string     `gorm:"column:code"`
	Name            string     `gorm:"column:name"`
	ScopeType       string     `gorm:"column:scope_type"`
	ScopeID         string     `gorm:"column:scope_id"`
	EnvironmentCode string     `gorm:"column:environment_code"`
	ValidFrom       *time.Time `gorm:"column:valid_from"`
	ValidUntil      *time.Time `gorm:"column:valid_until"`
}

type bindingRow struct {
	ID         string     `gorm:"column:id"`
	RoleID     string     `gorm:"column:role_id"`
	ScopeType  string     `gorm:"column:scope_type"`
	ScopeID    string     `gorm:"column:scope_id"`
	ValidFrom  *time.Time `gorm:"column:valid_from"`
	ValidUntil *time.Time `gorm:"column:valid_until"`
	Status     string     `gorm:"column:status"`
	Version    uint64     `gorm:"column:version"`
}

type catalogRow struct {
	RoleCode       string `gorm:"column:role_code"`
	PermissionCode string `gorm:"column:permission_code"`
	Effect         string `gorm:"column:effect"`
}

type permissionRow struct {
	ID   string `gorm:"column:id"`
	Code string `gorm:"column:code"`
}

func (s *Service) ResolveOIDCAuthorization(ctx context.Context, tenantID, clientID, userID string) (TokenAuthorization, error) {
	client, err := s.findClientApplication(ctx, tenantID, clientID)
	if err != nil {
		return TokenAuthorization{}, err
	}
	access, err := s.getAccessByApplication(ctx, tenantID, userID, client.ApplicationID, client.ApplicationCode, client.EnvironmentID, client.EnvironmentCode)
	if err != nil {
		return TokenAuthorization{}, err
	}
	roles := make([]string, 0, len(access.Roles))
	for _, role := range access.Roles {
		roles = append(roles, role.Code)
	}
	return TokenAuthorization{
		ApplicationCode: client.ApplicationCode,
		EnvironmentCode: client.EnvironmentCode,
		TenantID:        tenantID,
		Roles:           sortedUnique(roles),
		Permissions:     append([]string(nil), access.EffectivePermissions...),
		RoleConfigHash:  access.RoleConfigHash,
		AuthzRevision:   access.AuthzRevision,
	}, nil
}

func (s *Service) GetAccess(ctx context.Context, tenantID, userID, applicationCode string) (Access, error) {
	application, err := s.findApplication(ctx, tenantID, applicationCode)
	if err != nil {
		return Access{}, err
	}
	access, err := s.getAccessByApplication(ctx, tenantID, userID, application.ID, application.Code, "", "")
	if !errors.Is(err, ErrNotConfigured) {
		return access, err
	}
	roleConfigHash, hashErr := s.loadRoleConfigHash(ctx, tenantID, application.ID)
	if hashErr != nil {
		return Access{}, hashErr
	}
	revision, revisionErr := s.loadRevision(ctx, tenantID, application.ID)
	if revisionErr != nil {
		return Access{}, revisionErr
	}
	return Access{ApplicationCode: application.Code, RoleConfigHash: roleConfigHash, AuthzRevision: revision}, nil
}

func (s *Service) UpdateAccess(ctx context.Context, in UpdateAccessInput, applicationCode string) (Access, error) {
	in.TenantID = strings.TrimSpace(in.TenantID)
	in.UserID = strings.TrimSpace(in.UserID)
	in.OperatorID = strings.TrimSpace(in.OperatorID)
	applicationCode = strings.TrimSpace(applicationCode)
	if in.TenantID == "" || in.UserID == "" || in.OperatorID == "" || applicationCode == "" {
		return Access{}, validation("tenant_id, user_id, operator_id and application_code are required")
	}
	application, err := s.findApplication(ctx, in.TenantID, applicationCode)
	if err != nil {
		return Access{}, err
	}
	if err := s.ensureUser(ctx, in.TenantID, in.UserID); err != nil {
		return Access{}, err
	}
	now := s.clock.Now().UTC()
	normalizedRoles, err := normalizeRoleInputs(in.Roles, in.RolesProvided, now)
	if err != nil {
		return Access{}, err
	}
	permissionIDs, _, err := s.resolvePermissionCodes(ctx, in.TenantID, application.ID, in.CustomPermissions, in.CustomPermissionsProvided)
	if err != nil {
		return Access{}, err
	}

	type resolvedBinding struct {
		roleID    string
		scopeType string
		scopeID   string
		role      RoleInput
	}
	resolved := make([]resolvedBinding, 0, len(normalizedRoles))
	for _, role := range normalizedRoles {
		var roleRow roleRow
		if err := s.db.WithContext(ctx).Table("authz_role").Where("tenant_id = ? AND application_id = ? AND status = ? AND code = ? AND role_type <> ?", in.TenantID, application.ID, activeStatus, role.RoleCode, "COMPATIBILITY").Take(&roleRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return Access{}, validation("one or more application roles do not exist or are disabled")
			}
			return Access{}, fmt.Errorf("load application role: %w", err)
		}
		scopeType, scopeID := scopeTypeTenant, ""
		if role.ScopeType == "ENVIRONMENT" {
			var environment struct {
				ID string `gorm:"column:id"`
			}
			if err := s.db.WithContext(ctx).Table("platform_application_environment").Select("id").Where("tenant_id = ? AND application_id = ? AND environment = ? AND status = ?", in.TenantID, application.ID, role.EnvironmentCode, activeStatus).Take(&environment).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return Access{}, validation("environment does not exist or is disabled")
				}
				return Access{}, fmt.Errorf("load application environment: %w", err)
			}
			scopeType, scopeID = scopeTypeEnvironment, environment.ID
		}
		resolved = append(resolved, resolvedBinding{roleID: roleRow.ID, scopeType: scopeType, scopeID: scopeID, role: role})
	}

	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if in.RolesProvided {
			var existing []bindingRow
			if err := tx.Table("authz_role_binding AS rb").Select("rb.id, rb.role_id, rb.scope_type, rb.scope_id, rb.valid_from, rb.valid_until, rb.status, rb.version").Joins("JOIN authz_role AS r ON r.id = rb.role_id AND r.tenant_id = rb.tenant_id AND r.application_id = rb.application_id").Where("rb.tenant_id = ? AND rb.application_id = ? AND rb.subject_type = ? AND rb.subject_id = ? AND r.role_type <> ?", in.TenantID, application.ID, subjectTypeUser, in.UserID, "COMPATIBILITY").Find(&existing).Error; err != nil {
				return fmt.Errorf("load existing application role bindings: %w", err)
			}
			key := func(roleID, scopeType, scopeID string) string { return roleID + "\x00" + scopeType + "\x00" + scopeID }
			byKey := make(map[string]bindingRow, len(existing))
			for _, binding := range existing {
				byKey[key(binding.RoleID, binding.ScopeType, binding.ScopeID)] = binding
			}
			desired := make(map[string]resolvedBinding, len(resolved))
			for _, item := range resolved {
				desired[key(item.roleID, item.scopeType, item.scopeID)] = item
			}
			for _, binding := range existing {
				if _, keep := desired[key(binding.RoleID, binding.ScopeType, binding.ScopeID)]; keep || binding.Status != activeStatus {
					continue
				}
				if err := tx.Table("authz_role_binding").Where("id = ?", binding.ID).Updates(map[string]any{"status": disabledStatus, "version": binding.Version + 1, "updated_at": now, "updated_by": in.OperatorID}).Error; err != nil {
					return fmt.Errorf("disable removed application role binding: %w", err)
				}
				changed = true
			}
			for bindingKey, item := range desired {
				if binding, exists := byKey[bindingKey]; exists {
					if binding.Status == activeStatus && sameValidity(binding.ValidFrom, item.role.ValidFrom) && sameValidity(binding.ValidUntil, item.role.ValidUntil) {
						continue
					}
					if err := tx.Table("authz_role_binding").Where("id = ?", binding.ID).Updates(map[string]any{"status": activeStatus, "valid_from": item.role.ValidFrom, "valid_until": item.role.ValidUntil, "version": binding.Version + 1, "updated_at": now, "updated_by": in.OperatorID}).Error; err != nil {
						return fmt.Errorf("activate application role binding: %w", err)
					}
					changed = true
					continue
				}
				id, err := s.ids.New(now)
				if err != nil {
					return fmt.Errorf("generate application role binding ID: %w", err)
				}
				if err := tx.Table("authz_role_binding").Create(map[string]any{"id": id, "tenant_id": in.TenantID, "application_id": application.ID, "role_id": item.roleID, "subject_type": subjectTypeUser, "subject_id": in.UserID, "scope_type": item.scopeType, "scope_id": item.scopeID, "valid_from": item.role.ValidFrom, "valid_until": item.role.ValidUntil, "status": activeStatus, "version": 1, "created_at": now, "created_by": in.OperatorID, "updated_at": now, "updated_by": in.OperatorID}).Error; err != nil {
					return fmt.Errorf("create application role binding: %w", err)
				}
				changed = true
			}
		}
		if in.CustomPermissionsProvided {
			var existingPermissionIDs []string
			if err := tx.Table("authz_user_permission").Where("tenant_id = ? AND application_id = ? AND user_id = ?", in.TenantID, application.ID, in.UserID).Order("permission_id ASC").Pluck("permission_id", &existingPermissionIDs).Error; err != nil {
				return fmt.Errorf("load existing legacy user permissions: %w", err)
			}
			desiredPermissionIDs := append([]string(nil), permissionIDs...)
			sort.Strings(desiredPermissionIDs)
			if !sameStringSlice(existingPermissionIDs, desiredPermissionIDs) {
				if err := tx.Table("authz_user_permission").Where("tenant_id = ? AND application_id = ? AND user_id = ?", in.TenantID, application.ID, in.UserID).Delete(nil).Error; err != nil {
					return fmt.Errorf("replace legacy user permissions: %w", err)
				}
				for _, permissionID := range permissionIDs {
					if err := tx.Table("authz_user_permission").Create(map[string]any{"tenant_id": in.TenantID, "application_id": application.ID, "user_id": in.UserID, "permission_id": permissionID, "created_at": now, "created_by": in.OperatorID}).Error; err != nil {
						return fmt.Errorf("create legacy user permission: %w", err)
					}
				}
				changed = true
			}
		}
		if changed {
			return bumpRevision(tx, in.TenantID, application.ID, now, "user application authorization changed")
		}
		return nil
	})
	if err != nil {
		return Access{}, err
	}
	access, err := s.GetAccess(ctx, in.TenantID, in.UserID, application.Code)
	if err != nil {
		return Access{}, err
	}
	s.recordAudit(ctx, AuditEvent{
		TenantID: in.TenantID, ApplicationID: application.ID, ApplicationCode: application.Code,
		OperatorID: in.OperatorID, SubjectID: in.UserID, Action: "authorization.application_access.updated",
		ResourceType: "application_access", ResourceID: in.UserID, Result: "SUCCESS", RiskLevel: "HIGH",
		Summary: "应用用户授权已更新", OccurredAt: now,
		Metadata: map[string]any{"roles_provided": in.RolesProvided, "custom_permissions_provided": in.CustomPermissionsProvided, "changed": changed},
	})
	return access, nil
}
func (s *Service) DeleteAccess(ctx context.Context, in DeleteAccessInput, applicationCode string) error {
	in.TenantID = strings.TrimSpace(in.TenantID)
	in.UserID = strings.TrimSpace(in.UserID)
	in.OperatorID = strings.TrimSpace(in.OperatorID)
	applicationCode = strings.TrimSpace(applicationCode)
	if in.TenantID == "" || in.UserID == "" || in.OperatorID == "" || applicationCode == "" {
		return validation("tenant_id, user_id, operator_id and application_code are required")
	}
	application, err := s.findApplication(ctx, in.TenantID, applicationCode)
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Table("authz_role_binding").Where("tenant_id = ? AND application_id = ? AND subject_type = ? AND subject_id = ? AND status = ?", in.TenantID, application.ID, subjectTypeUser, in.UserID, activeStatus).Updates(map[string]any{"status": disabledStatus, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": in.OperatorID})
		if result.Error != nil {
			return fmt.Errorf("revoke application role bindings: %w", result.Error)
		}
		if result.RowsAffected > 0 {
			changed = true
		}
		permissionResult := tx.Table("authz_user_permission").Where("tenant_id = ? AND application_id = ? AND user_id = ?", in.TenantID, application.ID, in.UserID).Delete(nil)
		if permissionResult.Error != nil {
			return fmt.Errorf("remove legacy user permissions: %w", permissionResult.Error)
		}
		if permissionResult.RowsAffected > 0 {
			changed = true
		}
		if changed {
			return bumpRevision(tx, in.TenantID, application.ID, now, "user application access revoked")
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.recordAudit(ctx, AuditEvent{
		TenantID: in.TenantID, ApplicationID: application.ID, ApplicationCode: application.Code,
		OperatorID: in.OperatorID, SubjectID: in.UserID, Action: "authorization.application_access.deleted",
		ResourceType: "application_access", ResourceID: in.UserID, Result: "SUCCESS", RiskLevel: "HIGH",
		Summary: "应用用户授权已删除", OccurredAt: now,
		Metadata: map[string]any{"changed": changed},
	})
	return nil
}

// IsApplicationOwner reports whether the authenticated user owns the application.
// Application owners may synchronize their own authorization catalog without receiving
// broad platform administration permissions.
func (s *Service) IsApplicationOwner(ctx context.Context, tenantID, applicationID, userID string) (bool, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(applicationID) == "" || strings.TrimSpace(userID) == "" {
		return false, validation("tenant_id, application_id and user_id are required")
	}
	if _, err := s.findApplicationByID(ctx, tenantID, applicationID); err != nil {
		return false, err
	}
	var count int64
	if err := s.db.WithContext(ctx).Table("platform_application").Where("tenant_id = ? AND id = ? AND owner_user_id = ? AND status = ?", tenantID, applicationID, userID, activeStatus).Count(&count).Error; err != nil {
		return false, fmt.Errorf("check application owner: %w", err)
	}
	return count > 0, nil
}

func (s *Service) findClientApplication(ctx context.Context, tenantID, clientID string) (clientApplicationRow, error) {
	var row clientApplicationRow
	err := s.db.WithContext(ctx).Table("platform_oauth_client AS c").Select(
		"c.client_id, c.tenant_id, c.application_id, a.code AS application_code, c.environment_id, e.environment AS environment_code",
	).Joins("JOIN platform_application AS a ON a.id = c.application_id AND a.tenant_id = c.tenant_id AND a.status = ?", activeStatus).
		Joins("JOIN platform_application_environment AS e ON e.id = c.environment_id AND e.application_id = c.application_id AND e.tenant_id = c.tenant_id AND e.status = ?", activeStatus).
		Where("c.client_id = ? AND c.tenant_id = ? AND c.status = ?", strings.TrimSpace(clientID), strings.TrimSpace(tenantID), activeStatus).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return clientApplicationRow{}, ErrNotFound
	}
	if err != nil {
		return clientApplicationRow{}, fmt.Errorf("load OAuth client application: %w", err)
	}
	return row, nil
}

func (s *Service) findApplication(ctx context.Context, tenantID, code string) (applicationRow, error) {
	var row applicationRow
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND code = ? AND status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(code), activeStatus).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return applicationRow{}, ErrNotFound
	}
	if err != nil {
		return applicationRow{}, fmt.Errorf("load application: %w", err)
	}
	return row, nil
}

func (s *Service) ensureUser(ctx context.Context, tenantID, userID string) error {
	var count int64
	if err := s.db.WithContext(ctx).Table("iam_user").Where("tenant_id = ? AND id = ?", tenantID, userID).Count(&count).Error; err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) getAccessByApplication(ctx context.Context, tenantID, userID, applicationID, applicationCode, environmentID, environmentCode string) (Access, error) {
	userID = strings.TrimSpace(userID)
	if strings.TrimSpace(tenantID) == "" || userID == "" || applicationID == "" {
		return Access{}, validation("tenant_id, user_id and application are required")
	}
	now := s.clock.Now().UTC()
	roles, err := s.loadGenericRoles(ctx, tenantID, applicationID, userID, environmentID, now)
	if err != nil {
		return Access{}, err
	}
	roleIDs := make([]string, 0, len(roles))
	roleViews := make([]RoleView, 0, len(roles))
	seenRoles := map[string]struct{}{}
	for _, role := range roles {
		roleIDs = append(roleIDs, role.RoleID)
		roleKey := role.Code + "\x00" + role.ScopeType + "\x00" + role.ScopeID
		if _, exists := seenRoles[roleKey]; exists {
			continue
		}
		seenRoles[roleKey] = struct{}{}
		roleViews = append(roleViews, RoleView{Code: role.Code, Name: role.Name, ScopeType: externalScopeType(role.ScopeType), EnvironmentCode: role.EnvironmentCode, ValidFrom: role.ValidFrom, ValidUntil: role.ValidUntil})
	}
	rolePermissions, err := s.loadRolePermissions(ctx, tenantID, applicationID, sortedUnique(roleIDs))
	if err != nil {
		return Access{}, err
	}
	customPermissions, err := s.loadCustomPermissions(ctx, tenantID, applicationID, userID)
	if err != nil {
		return Access{}, err
	}
	rolePermissions, customPermissions = sortedUnique(rolePermissions), sortedUnique(customPermissions)
	effective := sortedUnique(append(append([]string(nil), rolePermissions...), customPermissions...))
	if len(roleViews) == 0 && len(effective) == 0 {
		return Access{}, ErrNotConfigured
	}
	roleConfigHash, err := s.loadRoleConfigHash(ctx, tenantID, applicationID)
	if err != nil {
		return Access{}, err
	}
	revision, err := s.loadRevision(ctx, tenantID, applicationID)
	if err != nil {
		return Access{}, err
	}
	access := Access{ApplicationCode: applicationCode, EnvironmentCode: environmentCode, Roles: roleViews, RolePermissions: rolePermissions, CustomPermissions: customPermissions, EffectivePermissions: effective, RoleConfigHash: roleConfigHash, AuthzRevision: revision}
	if len(roleViews) > 0 {
		access.Role = roleViews[0]
	}
	return access, nil
}
func externalScopeType(scopeType string) string {
	if scopeType == scopeTypeTenant {
		return "APPLICATION"
	}
	return scopeType
}

func (s *Service) loadGenericRoles(ctx context.Context, tenantID, applicationID, userID, environmentID string, now time.Time) ([]assignedRoleRow, error) {
	var rows []assignedRoleRow
	query := s.db.WithContext(ctx).Table("authz_role_binding AS rb").Select("r.id AS role_id, r.code, r.name, rb.scope_type, rb.scope_id, e.environment AS environment_code, rb.valid_from, rb.valid_until").Joins("JOIN authz_role AS r ON r.id = rb.role_id AND r.tenant_id = rb.tenant_id AND r.application_id = rb.application_id AND r.status = ?", activeStatus).Joins("LEFT JOIN platform_application_environment AS e ON e.id = rb.scope_id AND e.application_id = rb.application_id AND e.tenant_id = rb.tenant_id").Where("rb.tenant_id = ? AND rb.application_id = ? AND rb.subject_type = ? AND rb.subject_id = ? AND rb.status = ? AND r.role_type <> ?", tenantID, applicationID, subjectTypeUser, userID, activeStatus, "COMPATIBILITY").Where("(rb.valid_from IS NULL OR rb.valid_from <= ?) AND (rb.valid_until IS NULL OR rb.valid_until > ?)", now, now)
	if strings.TrimSpace(environmentID) == "" {
		// Management reads return the complete application authorization set.
		// OIDC reads always provide the OAuth client's environment ID and are
		// therefore restricted to tenant-wide plus that environment's bindings.
		query = query.Where("(rb.scope_type = ? AND rb.scope_id = ?) OR rb.scope_type = ?", scopeTypeTenant, "", scopeTypeEnvironment)
	} else {
		query = query.Where("(rb.scope_type = ? AND rb.scope_id = ?) OR (rb.scope_type = ? AND rb.scope_id = ?)", scopeTypeTenant, "", scopeTypeEnvironment, environmentID)
	}
	if err := query.Order("r.code ASC, rb.scope_type ASC, rb.scope_id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load application role bindings: %w", err)
	}
	return rows, nil
}
func (s *Service) loadRolePermissions(ctx context.Context, tenantID, applicationID string, roleIDs []string) ([]string, error) {
	if len(roleIDs) == 0 {
		return nil, nil
	}
	var codes []string
	err := s.db.WithContext(ctx).Table("authz_role_permission AS rp").Select("p.code").
		Joins("JOIN authz_permission AS p ON p.id = rp.permission_id AND p.tenant_id = ? AND p.application_id = ? AND p.status = ?", tenantID, applicationID, activeStatus).
		Where("rp.role_id IN ? AND rp.effect = ?", roleIDs, "ALLOW").Find(&codes).Error
	if err != nil {
		return nil, fmt.Errorf("load role permissions: %w", err)
	}
	return codes, nil
}

func (s *Service) loadCustomPermissions(ctx context.Context, tenantID, applicationID, userID string) ([]string, error) {
	var codes []string
	err := s.db.WithContext(ctx).Table("authz_user_permission AS up").Select("p.code").
		Joins("JOIN authz_permission AS p ON p.id = up.permission_id AND p.tenant_id = ? AND p.application_id = ? AND p.status = ?", tenantID, applicationID, activeStatus).
		Where("up.tenant_id = ? AND up.application_id = ? AND up.user_id = ?", tenantID, applicationID, userID).Find(&codes).Error
	if err != nil {
		return nil, fmt.Errorf("load user permissions: %w", err)
	}
	return codes, nil
}

func (s *Service) resolvePermissionCodes(ctx context.Context, tenantID, applicationID string, codes []string, provided bool) ([]string, []string, error) {
	if !provided {
		return nil, nil, nil
	}
	codes = sortedUnique(codes)
	if len(codes) == 0 {
		return nil, nil, nil
	}
	var rows []permissionRow
	if err := s.db.WithContext(ctx).Table("authz_permission").Select("id, code").Where(
		"tenant_id = ? AND application_id = ? AND status = ? AND code IN ?", tenantID, applicationID, activeStatus, codes,
	).Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("load application permissions: %w", err)
	}
	if len(rows) != len(codes) {
		return nil, nil, validation("one or more permissions do not exist or are disabled")
	}
	byCode := make(map[string]string, len(rows))
	for _, row := range rows {
		byCode[row.Code] = row.ID
	}
	ids := make([]string, 0, len(codes))
	for _, code := range codes {
		id, ok := byCode[code]
		if !ok {
			return nil, nil, validation("one or more permissions do not exist or are disabled")
		}
		ids = append(ids, id)
	}
	return ids, codes, nil
}

func (s *Service) loadRoleConfigHash(ctx context.Context, tenantID, applicationID string) (string, error) {
	var rows []catalogRow
	err := s.db.WithContext(ctx).Table("authz_role AS r").Select("r.code AS role_code, p.code AS permission_code, rp.effect").
		Joins("LEFT JOIN authz_role_permission AS rp ON rp.role_id = r.id").
		Joins("LEFT JOIN authz_permission AS p ON p.id = rp.permission_id AND p.tenant_id = r.tenant_id AND p.application_id = r.application_id AND p.status = ?", activeStatus).
		Where("r.tenant_id = ? AND r.application_id = ? AND r.status = ?", tenantID, applicationID, activeStatus).
		Find(&rows).Error
	if err != nil {
		return "", fmt.Errorf("load application role configuration: %w", err)
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, row.RoleCode+"\x00"+row.PermissionCode+"\x00"+row.Effect)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func (s *Service) loadRevision(ctx context.Context, tenantID, applicationID string) (uint64, error) {
	var row struct {
		Revision uint64 `gorm:"column:revision"`
	}
	err := s.db.WithContext(ctx).Table("authz_policy_revision").Select("revision").Where("tenant_id = ? AND application_id = ?", tenantID, applicationID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load application authorization revision: %w", err)
	}
	return row.Revision, nil
}

func bumpRevision(tx *gorm.DB, tenantID, applicationID string, now time.Time, reason string) error {
	row := map[string]any{"tenant_id": tenantID, "application_id": applicationID, "revision": 1, "changed_at": now, "change_reason": reason}
	err := tx.Table("authz_policy_revision").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"revision": gorm.Expr("revision + 1"), "changed_at": now, "change_reason": reason,
		}),
	}).Create(row).Error
	if err != nil {
		return fmt.Errorf("bump application authorization revision: %w", err)
	}
	return nil
}

func normalizeRoleInputs(inputs []RoleInput, provided bool, now time.Time) ([]RoleInput, error) {
	if !provided {
		return nil, nil
	}
	byKey := make(map[string]RoleInput, len(inputs))
	for _, input := range inputs {
		input.RoleCode = strings.TrimSpace(input.RoleCode)
		input.ScopeType = strings.ToUpper(strings.TrimSpace(input.ScopeType))
		input.EnvironmentCode = strings.TrimSpace(input.EnvironmentCode)
		if input.RoleCode == "" {
			return nil, validation("role_code is required")
		}
		if input.ScopeType == "" || input.ScopeType == "APPLICATION" || input.ScopeType == "TENANT" {
			input.ScopeType = "APPLICATION"
		} else if input.ScopeType != "ENVIRONMENT" {
			return nil, validation("scope_type must be APPLICATION or ENVIRONMENT")
		}
		if input.ScopeType == "ENVIRONMENT" && input.EnvironmentCode == "" {
			return nil, validation("environment_code is required for environment scope")
		}
		if input.ScopeType == "APPLICATION" && input.EnvironmentCode != "" {
			return nil, validation("environment_code is only valid for environment scope")
		}
		if input.ValidFrom != nil && input.ValidUntil != nil && input.ValidUntil.Before(*input.ValidFrom) {
			return nil, validation("valid_until must not be earlier than valid_from")
		}
		if input.ValidUntil != nil && !input.ValidUntil.After(now) {
			return nil, validation("valid_until must be in the future")
		}
		key := input.RoleCode + "\x00" + input.ScopeType + "\x00" + input.EnvironmentCode
		byKey[key] = input
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]RoleInput, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result, nil
}
func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func disabledOrActive(active bool) string {
	if active {
		return activeStatus
	}
	return disabledStatus
}

func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }
