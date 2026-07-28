package sys004

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNotFound      = errors.New("SYS-004 access configuration not found")
	ErrValidation    = errors.New("SYS-004 access validation failed")
	ErrIntegrity     = errors.New("SYS-004 role configuration integrity check failed")
	ErrNotConfigured = errors.New("SYS-004 user access is not configured")
)

type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// SecurityAuditRecorder persists a dedicated audit event when the protected role
// configuration no longer matches the code-defined SYS-004 catalog.
type SecurityAuditRecorder interface {
	RecordContractRoleIntegrityFailure(context.Context, string, string, string, string) error
}

type Service struct {
	db     *gorm.DB
	ids    IdentifierGenerator
	clock  Clock
	logger *slog.Logger
	audit  SecurityAuditRecorder
}

func NewService(db *gorm.DB, ids IdentifierGenerator, clock Clock, logger *slog.Logger, audit SecurityAuditRecorder) (*Service, error) {
	if db == nil || ids == nil || clock == nil || logger == nil {
		return nil, errors.New("SYS-004 service dependencies must not be nil")
	}
	return &Service{db: db, ids: ids, clock: clock, logger: logger, audit: audit}, nil
}

type RoleView struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type Access struct {
	Role                 RoleView `json:"role"`
	RolePermissions      []string `json:"role_permissions"`
	CustomPermissions    []string `json:"custom_permissions"`
	EffectivePermissions []string `json:"effective_permissions"`
	RoleConfigHash       string   `json:"role_config_hash"`
	AuthzRevision        uint64   `json:"authz_revision"`
}

type UpdateAccessInput struct {
	TenantID          string
	UserID            string
	OperatorID        string
	RoleCode          string
	CustomPermissions []string
}

type TokenAuthorization struct {
	ApplicationCode string
	TenantID        string
	Roles           []string
	Permissions     []string
	RoleConfigHash  string
	AuthzRevision   uint64
}

type applicationRow struct {
	ID       string `gorm:"column:id"`
	TenantID string `gorm:"column:tenant_id"`
	Code     string `gorm:"column:code"`
	Status   string `gorm:"column:status"`
}

func (applicationRow) TableName() string { return "platform_application" }

type resourceRow struct {
	ID            string    `gorm:"column:id"`
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	Code          string    `gorm:"column:code"`
	Name          string    `gorm:"column:name"`
	ResourceType  string    `gorm:"column:resource_type"`
	Status        string    `gorm:"column:status"`
	Version       uint64    `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (resourceRow) TableName() string { return "authz_resource" }

type permissionRow struct {
	ID            string    `gorm:"column:id"`
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	ResourceID    string    `gorm:"column:resource_id"`
	Code          string    `gorm:"column:code"`
	Action        string    `gorm:"column:action"`
	Name          string    `gorm:"column:name"`
	RiskLevel     string    `gorm:"column:risk_level"`
	Status        string    `gorm:"column:status"`
	Version       uint64    `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (permissionRow) TableName() string { return "authz_permission" }

type roleRow struct {
	ID            string    `gorm:"column:id"`
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	Code          string    `gorm:"column:code"`
	Name          string    `gorm:"column:name"`
	RoleType      string    `gorm:"column:role_type"`
	BuiltIn       bool      `gorm:"column:built_in"`
	Status        string    `gorm:"column:status"`
	Version       uint64    `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

func (roleRow) TableName() string { return "authz_role" }

type rolePermissionRow struct {
	RoleID       string    `gorm:"column:role_id"`
	PermissionID string    `gorm:"column:permission_id"`
	Effect       string    `gorm:"column:effect"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (rolePermissionRow) TableName() string { return "authz_role_permission" }

type userApplicationRoleRow struct {
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	UserID        string    `gorm:"column:user_id"`
	RoleID        string    `gorm:"column:role_id"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	CreatedBy     *string   `gorm:"column:created_by"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
	UpdatedBy     *string   `gorm:"column:updated_by"`
}

func (userApplicationRoleRow) TableName() string { return "authz_user_application_role" }

type userPermissionRow struct {
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	UserID        string    `gorm:"column:user_id"`
	PermissionID  string    `gorm:"column:permission_id"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	CreatedBy     *string   `gorm:"column:created_by"`
}

func (userPermissionRow) TableName() string { return "authz_user_permission" }

type policyRevisionRow struct {
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	Revision      uint64    `gorm:"column:revision"`
	ChangedAt     time.Time `gorm:"column:changed_at"`
	ChangeReason  string    `gorm:"column:change_reason"`
}

func (policyRevisionRow) TableName() string { return "authz_policy_revision" }

type userRow struct {
	ID       string `gorm:"column:id"`
	TenantID string `gorm:"column:tenant_id"`
	Status   string `gorm:"column:status"`
}

func (userRow) TableName() string { return "iam_user" }

type oauthClientApplicationRow struct {
	ApplicationID   string `gorm:"column:application_id"`
	ApplicationCode string `gorm:"column:application_code"`
}

// EnsureExistingApplications verifies or initializes every currently registered
// contract-management application. It is safe to call at startup and repeatedly.
func (s *Service) EnsureExistingApplications(ctx context.Context) error {
	var applications []applicationRow
	if err := s.db.WithContext(ctx).Where("code = ? AND status = ?", ApplicationCode, "ACTIVE").Find(&applications).Error; err != nil {
		return fmt.Errorf("list SYS-004 applications: %w", err)
	}
	for _, application := range applications {
		if _, err := s.ensureCatalogForApplication(ctx, application); err != nil {
			return err
		}
	}
	return nil
}

// EnsureCatalog initializes missing SYS-004 objects and rejects any mutation of
// an existing protected role or role-permission mapping.
func (s *Service) EnsureCatalog(ctx context.Context, tenantID string) (string, error) {
	application, err := s.findApplication(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return s.ensureCatalogForApplication(ctx, application)
}

func (s *Service) ensureCatalogForApplication(ctx context.Context, application applicationRow) (string, error) {
	expectedHash := RoleConfigHash()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := s.clock.Now().UTC()
		resources := make(map[string]resourceRow)
		for _, definition := range resourceDefinitions() {
			var row resourceRow
			err := tx.Where("tenant_id = ? AND application_id = ? AND code = ?", application.TenantID, application.ID, definition.Code).First(&row).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				id, idErr := s.ids.New(now)
				if idErr != nil {
					return fmt.Errorf("generate SYS-004 resource ID: %w", idErr)
				}
				row = resourceRow{ID: id, TenantID: application.TenantID, ApplicationID: application.ID, Code: definition.Code, Name: definition.Name, ResourceType: "API", Status: "ACTIVE", Version: 1, CreatedAt: now, UpdatedAt: now}
				if createErr := tx.Create(&row).Error; createErr != nil {
					return fmt.Errorf("create SYS-004 resource %s: %w", definition.Code, createErr)
				}
			case err != nil:
				return fmt.Errorf("load SYS-004 resource %s: %w", definition.Code, err)
			case row.Name != definition.Name || row.ResourceType != "API" || row.Status != "ACTIVE":
				return s.integrityError(ctx, application, expectedHash, "resource:"+definition.Code)
			}
			resources[definition.Code] = row
		}

		permissions := make(map[string]permissionRow, len(PermissionNames))
		for _, code := range PermissionCodes() {
			resourceCode, action := permissionResourceAndAction(code)
			var row permissionRow
			err := tx.Where("tenant_id = ? AND application_id = ? AND code = ?", application.TenantID, application.ID, code).First(&row).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				id, idErr := s.ids.New(now)
				if idErr != nil {
					return fmt.Errorf("generate SYS-004 permission ID: %w", idErr)
				}
				row = permissionRow{ID: id, TenantID: application.TenantID, ApplicationID: application.ID, ResourceID: resources[resourceCode].ID, Code: code, Action: action, Name: PermissionNames[code], RiskLevel: permissionRiskLevel(code), Status: "ACTIVE", Version: 1, CreatedAt: now, UpdatedAt: now}
				if createErr := tx.Create(&row).Error; createErr != nil {
					return fmt.Errorf("create SYS-004 permission %s: %w", code, createErr)
				}
			case err != nil:
				return fmt.Errorf("load SYS-004 permission %s: %w", code, err)
			case row.ResourceID != resources[resourceCode].ID || row.Action != action || row.Name != PermissionNames[code] || row.Status != "ACTIVE":
				return s.integrityError(ctx, application, expectedHash, "permission:"+code)
			}
			permissions[code] = row
		}

		for _, definition := range Roles {
			var row roleRow
			err := tx.Where("tenant_id = ? AND application_id = ? AND code = ?", application.TenantID, application.ID, definition.Code).First(&row).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				id, idErr := s.ids.New(now)
				if idErr != nil {
					return fmt.Errorf("generate SYS-004 role ID: %w", idErr)
				}
				row = roleRow{ID: id, TenantID: application.TenantID, ApplicationID: application.ID, Code: definition.Code, Name: definition.Name, RoleType: "SYSTEM", BuiltIn: true, Status: "ACTIVE", Version: 1, CreatedAt: now, UpdatedAt: now}
				if createErr := tx.Create(&row).Error; createErr != nil {
					return fmt.Errorf("create SYS-004 role %s: %w", definition.Code, createErr)
				}
				for _, permissionCode := range definition.Permissions {
					mapping := rolePermissionRow{RoleID: row.ID, PermissionID: permissions[permissionCode].ID, Effect: "ALLOW", CreatedAt: now}
					if createErr := tx.Create(&mapping).Error; createErr != nil {
						return fmt.Errorf("map SYS-004 role %s permission %s: %w", definition.Code, permissionCode, createErr)
					}
				}
			case err != nil:
				return fmt.Errorf("load SYS-004 role %s: %w", definition.Code, err)
			case row.Name != definition.Name || row.RoleType != "SYSTEM" || !row.BuiltIn || row.Status != "ACTIVE":
				return s.integrityError(ctx, application, expectedHash, "role:"+definition.Code)
			default:
				actual, mappingCount, queryErr := rolePermissionCodes(tx, application.TenantID, application.ID, row.ID)
				if queryErr != nil {
					return queryErr
				}
				expected := sortedUnique(definition.Permissions)
				if mappingCount != int64(len(actual)) || !equalStrings(actual, expected) {
					return s.integrityError(ctx, application, expectedHash, definition.Code+"="+strings.Join(actual, ","))
				}
			}
		}

		var actualPermissionCodes []string
		if err := tx.Model(&permissionRow{}).
			Where("tenant_id = ? AND application_id = ?", application.TenantID, application.ID).
			Order("code ASC").Pluck("code", &actualPermissionCodes).Error; err != nil {
			return fmt.Errorf("list SYS-004 permission catalog: %w", err)
		}
		if !equalStrings(sortedUnique(actualPermissionCodes), PermissionCodes()) {
			return s.integrityError(ctx, application, expectedHash, "permissions="+strings.Join(sortedUnique(actualPermissionCodes), ","))
		}

		var actualRoleCodes []string
		if err := tx.Model(&roleRow{}).
			Where("tenant_id = ? AND application_id = ?", application.TenantID, application.ID).
			Order("code ASC").Pluck("code", &actualRoleCodes).Error; err != nil {
			return fmt.Errorf("list SYS-004 role catalog: %w", err)
		}
		if !equalStrings(sortedUnique(actualRoleCodes), RoleCodes()) {
			return s.integrityError(ctx, application, expectedHash, "roles="+strings.Join(sortedUnique(actualRoleCodes), ","))
		}

		revision := policyRevisionRow{TenantID: application.TenantID, ApplicationID: application.ID, Revision: 1, ChangedAt: now, ChangeReason: "initialize SYS-004 contract authorization catalog"}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&revision).Error; err != nil {
			return fmt.Errorf("initialize SYS-004 policy revision: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return expectedHash, nil
}

func (s *Service) GetAccess(ctx context.Context, tenantID, userID string) (Access, error) {
	tenantID = strings.TrimSpace(tenantID)
	userID = strings.TrimSpace(userID)
	if tenantID == "" || userID == "" {
		return Access{}, validation("tenant_id and user_id are required")
	}
	application, err := s.findApplication(ctx, tenantID)
	if err != nil {
		return Access{}, err
	}
	if _, err := s.ensureCatalogForApplication(ctx, application); err != nil {
		return Access{}, err
	}
	return s.getAccessForApplication(ctx, application, userID)
}

func (s *Service) UpdateAccess(ctx context.Context, input UpdateAccessInput) (Access, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.UserID = strings.TrimSpace(input.UserID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.RoleCode = strings.TrimSpace(input.RoleCode)
	if input.TenantID == "" || input.UserID == "" || input.OperatorID == "" || input.RoleCode == "" {
		return Access{}, validation("tenant_id, user_id, operator_id and role_code are required")
	}
	roleDefinition, ok := Role(input.RoleCode)
	if !ok {
		return Access{}, validation("role_code is not a protected SYS-004 role")
	}
	custom, err := validateCustomPermissions(input.CustomPermissions)
	if err != nil {
		return Access{}, err
	}
	if roleDefinition.Code == "admin" && len(custom) != 0 {
		return Access{}, validation("admin must not have custom permissions")
	}

	application, err := s.findApplication(ctx, input.TenantID)
	if err != nil {
		return Access{}, err
	}
	if _, err := s.ensureCatalogForApplication(ctx, application); err != nil {
		return Access{}, err
	}
	now := s.clock.Now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user userRow
		if err := tx.Where("tenant_id = ? AND id = ?", input.TenantID, input.UserID).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("load SYS-004 user: %w", err)
		}
		var role roleRow
		if err := tx.Where("tenant_id = ? AND application_id = ? AND code = ? AND built_in = ? AND status = ?", input.TenantID, application.ID, input.RoleCode, true, "ACTIVE").First(&role).Error; err != nil {
			return fmt.Errorf("load protected SYS-004 role: %w", err)
		}
		operator := input.OperatorID
		assignment := userApplicationRoleRow{TenantID: input.TenantID, ApplicationID: application.ID, UserID: input.UserID, RoleID: role.ID, CreatedAt: now, CreatedBy: &operator, UpdatedAt: now, UpdatedBy: &operator}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "application_id"}, {Name: "user_id"}},
			DoUpdates: clause.Assignments(map[string]any{"role_id": role.ID, "updated_at": now, "updated_by": operator}),
		}).Create(&assignment).Error; err != nil {
			return fmt.Errorf("save SYS-004 user role: %w", err)
		}
		if err := tx.Where("tenant_id = ? AND application_id = ? AND user_id = ?", input.TenantID, application.ID, input.UserID).Delete(&userPermissionRow{}).Error; err != nil {
			return fmt.Errorf("clear SYS-004 custom permissions: %w", err)
		}
		for _, code := range custom {
			var permission permissionRow
			if err := tx.Where("tenant_id = ? AND application_id = ? AND code = ? AND status = ?", input.TenantID, application.ID, code, "ACTIVE").First(&permission).Error; err != nil {
				return fmt.Errorf("load SYS-004 custom permission %s: %w", code, err)
			}
			row := userPermissionRow{TenantID: input.TenantID, ApplicationID: application.ID, UserID: input.UserID, PermissionID: permission.ID, CreatedAt: now, CreatedBy: &operator}
			if err := tx.Create(&row).Error; err != nil {
				return fmt.Errorf("save SYS-004 custom permission %s: %w", code, err)
			}
		}
		result := tx.Model(&policyRevisionRow{}).
			Where("tenant_id = ? AND application_id = ?", input.TenantID, application.ID).
			Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "changed_at": now, "change_reason": "update user contract access"})
		if result.Error != nil {
			return fmt.Errorf("increment SYS-004 policy revision: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return errors.New("SYS-004 policy revision is missing")
		}
		return nil
	})
	if err != nil {
		return Access{}, err
	}
	return s.getAccessForApplication(ctx, application, input.UserID)
}

// ResolveOIDCAuthorization scopes authorization claims to the OAuth client's
// application. Non-contract clients intentionally receive no SYS-004 claims.
func (s *Service) ResolveOIDCAuthorization(ctx context.Context, tenantID, clientID, userID string) (TokenAuthorization, error) {
	tenantID = strings.TrimSpace(tenantID)
	clientID = strings.TrimSpace(clientID)
	userID = strings.TrimSpace(userID)
	if tenantID == "" || clientID == "" || userID == "" {
		return TokenAuthorization{}, validation("tenant_id, client_id and user_id are required")
	}
	var client oauthClientApplicationRow
	err := s.db.WithContext(ctx).Table("platform_oauth_client AS client").
		Select("client.application_id, application.code AS application_code").
		Joins("JOIN platform_application AS application ON application.id = client.application_id AND application.tenant_id = client.tenant_id").
		Where("client.tenant_id = ? AND client.client_id = ? AND client.status = ? AND application.status = ?", tenantID, clientID, "ACTIVE", "ACTIVE").
		Take(&client).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return TokenAuthorization{}, ErrNotFound
		}
		return TokenAuthorization{}, fmt.Errorf("resolve OAuth client application: %w", err)
	}
	if client.ApplicationCode != ApplicationCode {
		return TokenAuthorization{ApplicationCode: client.ApplicationCode, TenantID: tenantID, Roles: []string{}, Permissions: []string{}}, nil
	}
	application := applicationRow{ID: client.ApplicationID, TenantID: tenantID, Code: client.ApplicationCode, Status: "ACTIVE"}
	if _, err := s.ensureCatalogForApplication(ctx, application); err != nil {
		return TokenAuthorization{}, err
	}
	access, err := s.getAccessForApplication(ctx, application, userID)
	if err != nil {
		return TokenAuthorization{}, err
	}
	return TokenAuthorization{
		ApplicationCode: ApplicationCode,
		TenantID:        tenantID,
		Roles:           []string{access.Role.Code},
		Permissions:     append([]string(nil), access.EffectivePermissions...),
		RoleConfigHash:  access.RoleConfigHash,
		AuthzRevision:   access.AuthzRevision,
	}, nil
}

func (s *Service) getAccessForApplication(ctx context.Context, application applicationRow, userID string) (Access, error) {
	var assigned struct {
		RoleID            string `gorm:"column:role_id"`
		RoleTenantID      string `gorm:"column:role_tenant_id"`
		RoleApplicationID string `gorm:"column:role_application_id"`
		RoleCode          string `gorm:"column:role_code"`
		RoleName          string `gorm:"column:role_name"`
		RoleBuiltIn       bool   `gorm:"column:role_built_in"`
		RoleStatus        string `gorm:"column:role_status"`
	}
	err := s.db.WithContext(ctx).Table("authz_user_application_role AS assignment").
		Select("role.id AS role_id, role.tenant_id AS role_tenant_id, role.application_id AS role_application_id, role.code AS role_code, role.name AS role_name, role.built_in AS role_built_in, role.status AS role_status").
		Joins("JOIN authz_role AS role ON role.id = assignment.role_id").
		Where("assignment.tenant_id = ? AND assignment.application_id = ? AND assignment.user_id = ?", application.TenantID, application.ID, userID).
		Take(&assigned).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Access{}, ErrNotConfigured
		}
		return Access{}, fmt.Errorf("load SYS-004 user role: %w", err)
	}
	if assigned.RoleTenantID != application.TenantID || assigned.RoleApplicationID != application.ID || !assigned.RoleBuiltIn || assigned.RoleStatus != "ACTIVE" {
		return Access{}, s.integrityError(ctx, application, RoleConfigHash(), "assigned-role-scope:"+assigned.RoleID)
	}
	definition, ok := Role(assigned.RoleCode)
	if !ok {
		return Access{}, s.integrityError(ctx, application, RoleConfigHash(), "assigned-role:"+assigned.RoleCode)
	}
	rolePermissions, mappingCount, err := rolePermissionCodes(s.db.WithContext(ctx), application.TenantID, application.ID, assigned.RoleID)
	if err != nil {
		return Access{}, err
	}
	if mappingCount != int64(len(rolePermissions)) || !equalStrings(rolePermissions, sortedUnique(definition.Permissions)) {
		return Access{}, s.integrityError(ctx, application, RoleConfigHash(), assigned.RoleCode+"="+strings.Join(rolePermissions, ","))
	}
	var directRows []struct {
		PermissionID            string `gorm:"column:permission_id"`
		PermissionTenantID      string `gorm:"column:permission_tenant_id"`
		PermissionApplicationID string `gorm:"column:permission_application_id"`
		PermissionCode          string `gorm:"column:permission_code"`
		PermissionStatus        string `gorm:"column:permission_status"`
	}
	if err := s.db.WithContext(ctx).Table("authz_user_permission AS direct").
		Select("permission.id AS permission_id, permission.tenant_id AS permission_tenant_id, permission.application_id AS permission_application_id, permission.code AS permission_code, permission.status AS permission_status").
		Joins("JOIN authz_permission AS permission ON permission.id = direct.permission_id").
		Where("direct.tenant_id = ? AND direct.application_id = ? AND direct.user_id = ?", application.TenantID, application.ID, userID).
		Order("permission.code ASC").Scan(&directRows).Error; err != nil {
		return Access{}, fmt.Errorf("load SYS-004 custom permissions: %w", err)
	}
	custom := make([]string, 0, len(directRows))
	for _, direct := range directRows {
		if direct.PermissionTenantID != application.TenantID || direct.PermissionApplicationID != application.ID || direct.PermissionStatus != "ACTIVE" {
			return Access{}, s.integrityError(ctx, application, RoleConfigHash(), "custom-permission-scope:"+direct.PermissionID)
		}
		if !IsCustomPermissionAllowed(direct.PermissionCode) {
			return Access{}, s.integrityError(ctx, application, RoleConfigHash(), "custom-permission:"+direct.PermissionCode)
		}
		custom = append(custom, direct.PermissionCode)
	}
	if assigned.RoleCode == "admin" && len(custom) != 0 {
		return Access{}, s.integrityError(ctx, application, RoleConfigHash(), "admin-custom-permissions")
	}
	var revision policyRevisionRow
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND application_id = ?", application.TenantID, application.ID).First(&revision).Error; err != nil {
		return Access{}, fmt.Errorf("load SYS-004 policy revision: %w", err)
	}
	return Access{
		Role:                 RoleView{Code: assigned.RoleCode, Name: assigned.RoleName},
		RolePermissions:      rolePermissions,
		CustomPermissions:    custom,
		EffectivePermissions: sortedUnique(append(append([]string(nil), rolePermissions...), custom...)),
		RoleConfigHash:       RoleConfigHash(),
		AuthzRevision:        revision.Revision,
	}, nil
}

func (s *Service) findApplication(ctx context.Context, tenantID string) (applicationRow, error) {
	var application applicationRow
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND code = ? AND status = ?", strings.TrimSpace(tenantID), ApplicationCode, "ACTIVE").First(&application).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return applicationRow{}, ErrNotFound
		}
		return applicationRow{}, fmt.Errorf("load contract application: %w", err)
	}
	return application, nil
}

func (s *Service) integrityError(ctx context.Context, application applicationRow, expectedHash, actual string) error {
	s.logger.ErrorContext(ctx, "SYS-004 protected role configuration mismatch",
		"security_event", "contract_role_config_integrity_failure",
		"tenant_id", application.TenantID,
		"application_id", application.ID,
		"application_code", ApplicationCode,
		"expected_hash", expectedHash,
		"actual", actual,
	)
	if s.audit != nil {
		if err := s.audit.RecordContractRoleIntegrityFailure(ctx, application.TenantID, application.ID, expectedHash, actual); err != nil {
			s.logger.ErrorContext(ctx, "record SYS-004 integrity audit event", "error", err, "tenant_id", application.TenantID, "application_id", application.ID)
		}
	}
	return fmt.Errorf("%w: expected=%s actual=%s", ErrIntegrity, expectedHash, actual)
}

type resourceDefinition struct{ Code, Name string }

func resourceDefinitions() []resourceDefinition {
	return []resourceDefinition{
		{Code: "sys004.system", Name: "合同系统超级权限"},
		{Code: "sys004.dashboard", Name: "合同系统仪表盘"},
		{Code: "sys004.contract", Name: "合同"},
		{Code: "sys004.customer", Name: "客户"},
		{Code: "sys004.contract_type", Name: "合同类型"},
		{Code: "sys004.contract_template", Name: "合同模板"},
		{Code: "sys004.approval", Name: "审批"},
		{Code: "sys004.user", Name: "合同系统用户"},
		{Code: "sys004.audit", Name: "合同系统审计日志"},
	}
}

func permissionResourceAndAction(code string) (string, string) {
	if code == "all" {
		return "sys004.system", "all"
	}
	if code == "dashboard" {
		return "sys004.dashboard", "read"
	}
	resource, action, _ := strings.Cut(code, ".")
	return "sys004." + resource, action
}

func permissionRiskLevel(code string) string {
	if code == "all" || code == "user.manage" || strings.HasSuffix(code, ".delete") || strings.HasSuffix(code, ".manage") || code == "approval.process" {
		return "HIGH"
	}
	if strings.HasSuffix(code, ".create") || strings.HasSuffix(code, ".edit") {
		return "MEDIUM"
	}
	return "LOW"
}

func rolePermissionCodes(db *gorm.DB, tenantID, applicationID, roleID string) ([]string, int64, error) {
	var mappingCount int64
	if err := db.Table("authz_role_permission").Where("role_id = ?", roleID).Count(&mappingCount).Error; err != nil {
		return nil, 0, fmt.Errorf("count protected role permission mappings: %w", err)
	}
	var codes []string
	if err := db.Table("authz_role_permission AS mapping").
		Select("permission.code").
		Joins("JOIN authz_permission AS permission ON permission.id = mapping.permission_id").
		Where("mapping.role_id = ? AND mapping.effect = ? AND permission.tenant_id = ? AND permission.application_id = ? AND permission.status = ?", roleID, "ALLOW", tenantID, applicationID, "ACTIVE").
		Order("permission.code ASC").Scan(&codes).Error; err != nil {
		return nil, 0, fmt.Errorf("load protected role permissions: %w", err)
	}
	return sortedUnique(codes), mappingCount, nil
}

func validateCustomPermissions(input []string) ([]string, error) {
	custom := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, raw := range input {
		code := strings.TrimSpace(raw)
		if code == "" {
			return nil, validation("custom_permissions contains an empty value")
		}
		if !IsCustomPermissionAllowed(code) {
			return nil, validation("custom permission is unknown or protected: " + code)
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		custom = append(custom, code)
	}
	sort.Strings(custom)
	return custom, nil
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

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }
