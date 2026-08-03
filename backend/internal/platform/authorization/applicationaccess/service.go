// Package applicationaccess provides application-aware authorization for OAuth/OIDC clients.
//
// The package deliberately resolves access from the OAuth client registration instead of from a
// hard-coded subsystem name. This keeps roles and permissions isolated by application.
// Application-owned catalog policies determine any maximum effective-role constraint.
package applicationaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
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
	subjectTypeOrgUnit   = "ORG_UNIT"
	subjectTypePosition  = "POSITION"
	scopeTypeTenant      = "TENANT"
	scopeTypeEnvironment = "ENVIRONMENT"

	authorizationUnauthorized = "UNAUTHORIZED"
	authorizationGranted      = "GRANTED"
	authorizationConflict     = "CONFLICT"

	grantOriginManual   = "MANUAL"
	grantOriginTemplate = "TEMPLATE"
	grantOriginSystem   = "SYSTEM"

	applicationRoleType = "APPLICATION"

	sourceKindManual    = "MANUAL"
	sourceKindInherited = "INHERITED"
	sourceKindSystem    = "SYSTEM"

	// PlatformApplicationCode is the migration-seeded built-in application that owns the
	// platform's own roles and permissions. The catalog mirror is normally published by a
	// subsystem's catalog-publisher OAuth client; the platform does not have such a client for
	// itself, so the API bootstrap mirrors the migration-seeded role/permission data into the
	// application-owned catalog row instead.
	PlatformApplicationCode = "platform"
	// PlatformCatalogVersion is the stable catalog_version assigned to the bootstrap mirror.
	// Drift detection relies on the catalog_hash; the version is intentionally fixed so the
	// platform can re-acknowledge its built-in data without the UI inferring "new version" on
	// every API restart.
	PlatformCatalogVersion = "v1-platform-builtin"
	// PlatformCatalogSourceType is the source_type used when the API mirror publishes the
	// built-in data. Subsystem catalogs use "APPLICATION"; the platform mirror is its own thing
	// and is not an externally published manifest.
	PlatformCatalogSourceType = "BUILTIN"
	// PlatformCatalogSourceIdentifier is the source_identifier paired with
	// PlatformCatalogSourceType. It is purely descriptive for the audit history.
	PlatformCatalogSourceIdentifier = "platform:bootstrap"
	// BootstrapSuperAdminRoleCode is the built-in role assigned only by the controlled first
	// super administrator flow. It mirrors identityapplication.BootstrapSuperAdminRoleCode so
	// the platform catalog bootstrap can locate the first admin without importing the
	// identity application package (which would create an import cycle).
	BootstrapSuperAdminRoleCode = "platform-super-admin"
	// PlatformCatalogBootstrapOperatorID is a 26-char Crockford Base32 placeholder used as the
	// last_synced_by for the platform catalog when no first super administrator has been
	// created yet. The "PLATFSY" suffix makes the placeholder easy to spot in audit history
	// and impossible to confuse with a real ULID-encoded user id.
	PlatformCatalogBootstrapOperatorID = "01J0000000000000PLATFSY000"
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
	SourceType      string     `json:"source_type"`
	SourceID        string     `json:"source_id"`
	SourceName      string     `json:"source_name"`
	Direct          bool       `json:"direct"`

	// GrantOrigin is the durable provenance of the binding. It is intentionally
	// separate from SourceType: SourceType says which subject owns the binding,
	// while GrantOrigin says how the binding was granted.
	GrantOrigin  string `json:"grant_origin"`
	OriginID     string `json:"origin_id"`
	OriginItemID string `json:"origin_item_id"`
	SourceKind   string `json:"source_kind"`

	AssignmentID string `json:"assignment_id,omitempty"`
	TemplateID   string `json:"template_id,omitempty"`
	TemplateCode string `json:"template_code,omitempty"`
	TemplateName string `json:"template_name,omitempty"`
	PositionID   string `json:"position_id,omitempty"`
	PositionName string `json:"position_name,omitempty"`
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
	DirectRoles     []RoleView `json:"direct_roles"`
	InheritedRoles  []RoleView `json:"inherited_roles"`
	// ManualRoles contains only direct, manually managed user grants. It is
	// provided so management screens can edit manual access without replacing
	// template-inherited or system-provisioned bindings.
	ManualRoles []RoleView `json:"manual_roles"`
	// Role is retained for clients of the old SYS-004 access endpoint.  New clients should use
	// Roles, which can contain more than one application role.
	Role                 RoleView `json:"role"`
	RolePermissions      []string `json:"role_permissions"`
	CustomPermissions    []string `json:"custom_permissions"`
	EffectivePermissions []string `json:"effective_permissions"`
	RoleConfigHash       string   `json:"role_config_hash"`
	AuthzRevision        uint64   `json:"authz_revision"`
	AuthorizationState   string   `json:"authorization_state"`
	Conflicts            []string `json:"conflicts"`
}

type UpdateAccessInput struct {
	TenantID                  string
	UserID                    string
	OperatorID                string
	OperatorName              string
	Roles                     []RoleInput
	RolesProvided             bool
	CustomPermissions         []string
	CustomPermissionsProvided bool
}

type DeleteAccessInput struct {
	TenantID     string
	UserID       string
	OperatorID   string
	OperatorName string
}

type UpdateSubjectAccessInput struct {
	TenantID      string
	SubjectType   string
	SubjectID     string
	OperatorID    string
	OperatorName  string
	Roles         []RoleInput
	RolesProvided bool
}

type DeleteSubjectAccessInput struct {
	TenantID     string
	SubjectType  string
	SubjectID    string
	OperatorID   string
	OperatorName string
}

type TokenAuthorization struct {
	ApplicationCode string
	EnvironmentCode string
	TenantID        string
	PersonID        string
	PrimaryOrgID    string
	OrganizationIDs []string
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
	ID       string `gorm:"column:id"`
	Code     string `gorm:"column:code"`
	Name     string `gorm:"column:name"`
	RoleType string `gorm:"column:role_type"`
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
	SubjectType     string     `gorm:"column:subject_type"`
	SubjectID       string     `gorm:"column:subject_id"`
	SourceName      string     `gorm:"column:source_name"`
	GrantOrigin     string     `gorm:"column:grant_origin"`
	OriginID        string     `gorm:"column:origin_id"`
	OriginItemID    string     `gorm:"column:origin_item_id"`
	AssignmentID    string     `gorm:"column:assignment_id"`
	TemplateID      string     `gorm:"column:template_id"`
	TemplateCode    string     `gorm:"column:template_code"`
	TemplateName    string     `gorm:"column:template_name"`
	PositionID      string     `gorm:"column:position_id"`
	PositionName    string     `gorm:"column:position_name"`
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

type resolvedBinding struct {
	roleID    string
	scopeType string
	scopeID   string
	role      RoleInput
}

type catalogRow struct {
	RoleCode         string `gorm:"column:role_code"`
	RoleName         string `gorm:"column:role_name"`
	RoleType         string `gorm:"column:role_type"`
	RoleBuiltIn      bool   `gorm:"column:role_built_in"`
	RoleStatus       string `gorm:"column:role_status"`
	PermissionCode   string `gorm:"column:permission_code"`
	PermissionName   string `gorm:"column:permission_name"`
	PermissionStatus string `gorm:"column:permission_status"`
	Effect           string `gorm:"column:effect"`
}

func (s *Service) ResolveOIDCAuthorization(ctx context.Context, tenantID, clientID, userID string) (TokenAuthorization, error) {
	client, err := s.findClientApplication(ctx, tenantID, clientID)
	if err != nil {
		return TokenAuthorization{}, err
	}
	if client.ApplicationCode != PlatformApplicationCode {
		// 平台超级管理员进入子系统时也不携带平台权限。只有目标应用存在约定的 admin
		// 角色时，才以该应用目录中的角色和权限生成令牌；否则回到普通绑定解析。
		inherited, ok, inheritErr := s.resolvePlatformSuperAdminAuthorization(ctx, tenantID, userID, client)
		if inheritErr != nil {
			return TokenAuthorization{}, inheritErr
		}
		if ok {
			return s.attachOrganizationClaims(ctx, inherited, userID)
		}
	}
	access, err := s.getAccessByApplication(ctx, tenantID, userID, client.ApplicationID, client.ApplicationCode, client.EnvironmentID, client.EnvironmentCode)
	if err != nil {
		return TokenAuthorization{}, err
	}
	if err := requireGrantedAuthorization(access); err != nil {
		return TokenAuthorization{}, ErrAccessDenied
	}
	// OIDC 声明来自当前数据库授权快照，而不是登录时缓存；目录哈希和修订号供
	// 子系统检测目录变化或权限撤销，避免长期复用已经失效的权限集合。
	roles := make([]string, 0, len(access.Roles))
	for _, role := range access.Roles {
		roles = append(roles, role.Code)
	}
	return s.attachOrganizationClaims(ctx, TokenAuthorization{
		ApplicationCode: client.ApplicationCode,
		EnvironmentCode: client.EnvironmentCode,
		TenantID:        tenantID,
		Roles:           sortedUnique(roles),
		Permissions:     append([]string(nil), access.EffectivePermissions...),
		RoleConfigHash:  access.RoleConfigHash,
		AuthzRevision:   access.AuthzRevision,
	}, userID)
}

const maxOIDCOrganizationIDs = 100

type organizationClaimRow struct {
	OrganizationID string `gorm:"column:organization_id"`
	IsPrimary      bool   `gorm:"column:is_primary"`
}

// attachOrganizationClaims resolves only the user's current direct memberships. It deliberately
// does not expand organization descendants: downstream systems receive the exact active
// membership set maintained by the platform.
func (s *Service) attachOrganizationClaims(ctx context.Context, authorization TokenAuthorization, userID string) (TokenAuthorization, error) {
	now := s.clock.Now().UTC()
	rows := make([]organizationClaimRow, 0)
	err := buildOrganizationClaimsQuery(s.db.WithContext(ctx), authorization.TenantID, userID, now).
		Find(&rows).Error
	if err != nil {
		return TokenAuthorization{}, fmt.Errorf("load OIDC organization claims: %w", err)
	}
	primaryOrgID, organizationIDs, err := organizationClaimsFromRows(rows)
	if err != nil {
		return TokenAuthorization{}, err
	}
	authorization.PrimaryOrgID = primaryOrgID
	authorization.OrganizationIDs = organizationIDs
	personID, err := s.resolvePMSPersonID(ctx, authorization.TenantID, userID)
	if err != nil {
		return TokenAuthorization{}, err
	}
	authorization.PersonID = personID
	return authorization, nil
}

// resolvePMSPersonID returns only the explicit tenant-scoped binding on an active platform user.
// An absent binding is a valid empty claim and must never be replaced by userID, employee_no, or
// any login identifier.
func (s *Service) resolvePMSPersonID(ctx context.Context, tenantID, userID string) (string, error) {
	var row struct {
		PMSPersonID *string `gorm:"column:pms_person_id"`
	}
	err := s.db.WithContext(ctx).Table("iam_user").
		Select("pms_person_id").
		Where("tenant_id = ? AND id = ? AND status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(userID), activeStatus).
		Take(&row).Error
	if err != nil {
		return "", fmt.Errorf("load OIDC PMS person binding: %w", err)
	}
	if row.PMSPersonID == nil {
		return "", nil
	}
	personID := *row.PMSPersonID
	if personID == "" || personID != strings.TrimSpace(personID) || len([]byte(personID)) > 64 || strings.IndexFunc(personID, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0 {
		return "", errors.New("OIDC PMS person binding is invalid")
	}
	return personID, nil
}

func buildOrganizationClaimsQuery(database *gorm.DB, tenantID, userID string, now time.Time) *gorm.DB {
	return database.
		Table("iam_membership AS membership").
		Select(`membership.org_unit_id AS organization_id,
			MAX(CASE WHEN membership.is_primary = 1 AND user.primary_org_id = membership.org_unit_id THEN 1 ELSE 0 END) AS is_primary`).
		Joins("JOIN iam_user AS user ON user.id = membership.user_id AND user.tenant_id = membership.tenant_id AND user.status = ?", activeStatus).
		Joins("JOIN iam_org_unit AS organization ON organization.id = membership.org_unit_id AND organization.tenant_id = membership.tenant_id AND organization.status = ?", activeStatus).
		Where("membership.tenant_id = ? AND membership.user_id = ? AND membership.status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(userID), activeStatus).
		Where("(membership.valid_from IS NULL OR membership.valid_from <= ?) AND (membership.valid_until IS NULL OR membership.valid_until > ?)", now, now).
		Group("membership.org_unit_id").
		Order("membership.org_unit_id ASC").
		Limit(maxOIDCOrganizationIDs + 1)
}

func organizationClaimsFromRows(rows []organizationClaimRow) (string, []string, error) {
	if len(rows) > maxOIDCOrganizationIDs {
		return "", nil, errors.New("OIDC organization list exceeds the supported maximum")
	}
	organizationIDs := make([]string, 0, len(rows))
	primaryOrgID := ""
	for _, row := range rows {
		organizationID := strings.TrimSpace(row.OrganizationID)
		if organizationID == "" || len([]byte(organizationID)) > 64 {
			return "", nil, errors.New("OIDC organization query returned an invalid identifier")
		}
		if len(organizationIDs) > 0 && organizationIDs[len(organizationIDs)-1] >= organizationID {
			return "", nil, errors.New("OIDC organization query returned a non-canonical set")
		}
		organizationIDs = append(organizationIDs, organizationID)
		if row.IsPrimary {
			if primaryOrgID != "" {
				return "", nil, errors.New("OIDC organization query returned multiple primary memberships")
			}
			primaryOrgID = organizationID
		}
	}
	return primaryOrgID, organizationIDs, nil
}

// resolvePlatformSuperAdminAuthorization gives the tenant's controlled platform super
// administrator access only when the target application exposes one unambiguous canonical admin
// role. Applications that model administration as several least-privilege roles intentionally
// have no synthetic "admin" role; for those applications this resolver declines inheritance and
// lets the normal application bindings decide access.
//
// It does not copy platform permissions into a subsystem token: when inheritance is applicable,
// the emitted permissions still come exclusively from the target application's active admin role
// and catalog.
func (s *Service) resolvePlatformSuperAdminAuthorization(ctx context.Context, tenantID, userID string, client clientApplicationRow) (TokenAuthorization, bool, error) {
	now := s.clock.Now().UTC()
	var bindingCount int64
	err := s.db.WithContext(ctx).Table("authz_role_binding AS binding").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.application_id = binding.application_id AND role.status = ?", activeStatus).
		Joins("JOIN platform_application AS application ON application.id = binding.application_id AND application.tenant_id = binding.tenant_id AND application.status = ?", activeStatus).
		Where(
			"binding.tenant_id = ? AND binding.subject_type = ? AND binding.subject_id = ? AND binding.scope_type = ? AND binding.scope_id = '' AND binding.status = ? AND "+
				"application.code = ? AND role.code = ? AND (binding.valid_from IS NULL OR binding.valid_from <= ?) AND "+
				"(binding.valid_until IS NULL OR binding.valid_until > ?)",
			strings.TrimSpace(tenantID), subjectTypeUser, strings.TrimSpace(userID), scopeTypeTenant, activeStatus,
			PlatformApplicationCode, BootstrapSuperAdminRoleCode, now, now,
		).
		Count(&bindingCount).Error
	if err != nil {
		return TokenAuthorization{}, false, fmt.Errorf("load platform super administrator binding: %w", err)
	}
	if bindingCount == 0 {
		return TokenAuthorization{}, false, nil
	}

	var adminRole roleRow
	err = s.db.WithContext(ctx).Table("authz_role").
		Select("id", "code", "name").
		Where("tenant_id = ? AND application_id = ? AND code = ? AND status = ?", tenantID, client.ApplicationID, "admin", activeStatus).
		Take(&adminRole).Error
	adminRole, available, err := canonicalAdminRoleResult(adminRole, err)
	if err != nil {
		return TokenAuthorization{}, false, err
	}
	if !available {
		return TokenAuthorization{}, false, nil
	}
	permissions, err := s.loadRolePermissions(ctx, tenantID, client.ApplicationID, []string{adminRole.ID})
	if err != nil {
		return TokenAuthorization{}, false, err
	}
	roleConfigHash, err := s.loadRoleConfigHash(ctx, tenantID, client.ApplicationID)
	if err != nil {
		return TokenAuthorization{}, false, err
	}
	revision, err := s.loadRevision(ctx, tenantID, client.ApplicationID)
	if err != nil {
		return TokenAuthorization{}, false, err
	}
	return TokenAuthorization{
		ApplicationCode: client.ApplicationCode,
		EnvironmentCode: client.EnvironmentCode,
		TenantID:        tenantID,
		Roles:           []string{adminRole.Code},
		Permissions:     sortedUnique(permissions),
		RoleConfigHash:  roleConfigHash,
		AuthzRevision:   revision,
	}, true, nil
}

// canonicalAdminRoleResult converts the optional conventional admin-role lookup into the
// inheritance decision used by OIDC authorization. A missing role is not an authorization error:
// it means the application uses its own assigned role model and resolution must continue there.
func canonicalAdminRoleResult(role roleRow, err error) (roleRow, bool, error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return roleRow{}, false, nil
	}
	if err != nil {
		return roleRow{}, false, fmt.Errorf("load target application admin role: %w", err)
	}
	return role, true, nil
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
	return emptyAccess(application.Code, "", roleConfigHash, revision), nil
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
	if err := validateCustomPermissionsUpdate(in.CustomPermissionsProvided); err != nil {
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
	resolved, err := s.resolveRoleBindings(ctx, in.TenantID, application.ID, normalizedRoles)
	if err != nil {
		return Access{}, err
	}
	if err := s.validateDirectRoleLimit(ctx, in.TenantID, application.ID, resolved); err != nil {
		return Access{}, err
	}
	// 先读取完整生效视图用于审计差异；真正写入只替换 MANUAL 来源的用户直绑，
	// 组织、岗位模板和系统来源不会被一次用户编辑意外覆盖。
	before, err := s.GetAccess(ctx, in.TenantID, in.UserID, application.Code)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return Access{}, err
	}

	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if in.RolesProvided {
			roleChanged, err := s.replaceManualUserRoleBindings(tx, in.TenantID, application.ID, in.UserID, in.OperatorID, resolved, now)
			if err != nil {
				return err
			}
			changed = changed || roleChanged
		}
		if changed {
			// 授权数据和 revision 必须原子提交，子系统才能可靠识别旧授权快照。
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
		OperatorID: in.OperatorID, OperatorName: in.OperatorName, SubjectID: in.UserID, Action: "authorization.application_access.updated",
		ResourceType: "application_access", ResourceID: in.UserID, Result: "SUCCESS", RiskLevel: "HIGH",
		Summary: "应用用户授权已更新", OccurredAt: now,
		Metadata: map[string]any{"roles_provided": in.RolesProvided, "custom_permissions_provided": in.CustomPermissionsProvided, "changed": changed, "grant_origin": grantOriginManual, "authorization_revision": access.AuthzRevision},
		Changes:  accessAuditChanges(before, access),
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
	before, err := s.GetAccess(ctx, in.TenantID, in.UserID, application.Code)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return err
	}
	now := s.clock.Now().UTC()
	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 删除应用授权是逻辑撤销：仅停用人工直绑并清理历史自定义权限，不触碰岗位
		// 模板等可追溯来源；若用户仍有继承角色，有效视图仍会如实保留。
		directClause, directArgs := manualDirectApplicationRoleBindingFilter(in.TenantID, application.ID, in.UserID)
		applicationRoleIDs := tx.Table("authz_role").Select("id").Where("tenant_id = ? AND application_id = ? AND role_type = ?", in.TenantID, application.ID, applicationRoleType)
		result := tx.Table("authz_role_binding AS rb").Where(directClause, directArgs...).Where("rb.role_id IN (?)", applicationRoleIDs).Where("rb.status = ?", activeStatus).Updates(map[string]any{"status": disabledStatus, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": in.OperatorID})
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
	after, err := s.GetAccess(ctx, in.TenantID, in.UserID, application.Code)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return err
	}
	s.recordAudit(ctx, AuditEvent{
		TenantID: in.TenantID, ApplicationID: application.ID, ApplicationCode: application.Code,
		OperatorID: in.OperatorID, OperatorName: in.OperatorName, SubjectID: in.UserID, Action: "authorization.application_access.deleted",
		ResourceType: "application_access", ResourceID: in.UserID, Result: "SUCCESS", RiskLevel: "HIGH",
		Summary: "应用用户授权已删除", OccurredAt: now,
		Metadata: map[string]any{"changed": changed, "grant_origin": grantOriginManual, "authorization_revision": after.AuthzRevision},
		Changes:  accessAuditChanges(before, after),
	})
	return nil
}

// GetSubjectAccess returns bindings assigned directly to an organization unit or position.
// Unlike GetAccess, it never expands memberships because the requested subject is the managed
// authorization principal itself.
func (s *Service) GetSubjectAccess(ctx context.Context, tenantID, subjectType, subjectID, applicationCode string) (Access, error) {
	tenantID = strings.TrimSpace(tenantID)
	subjectID = strings.TrimSpace(subjectID)
	applicationCode = strings.TrimSpace(applicationCode)
	subjectType, err := normalizeManagedSubjectType(subjectType)
	if err != nil {
		return Access{}, err
	}
	if tenantID == "" || subjectID == "" || applicationCode == "" {
		return Access{}, validation("tenant_id, subject_type, subject_id and application_code are required")
	}
	application, err := s.findApplication(ctx, tenantID, applicationCode)
	if err != nil {
		return Access{}, err
	}
	if err := s.ensureManagedSubject(ctx, tenantID, subjectType, subjectID); err != nil {
		return Access{}, err
	}
	access, err := s.getSubjectAccessByApplication(ctx, tenantID, subjectType, subjectID, application.ID, application.Code, "", "")
	if !errors.Is(err, ErrNotConfigured) {
		return access, err
	}
	roleConfigHash, err := s.loadRoleConfigHash(ctx, tenantID, application.ID)
	if err != nil {
		return Access{}, err
	}
	revision, err := s.loadRevision(ctx, tenantID, application.ID)
	if err != nil {
		return Access{}, err
	}
	return emptyAccess(application.Code, "", roleConfigHash, revision), nil
}

// UpdateSubjectAccess is retained as a compatibility boundary for callers of the historical
// organization/position write endpoint. Standard personnel authorization is now materialized
// exclusively by position authorization templates, so this method always rejects manual writes.
// Existing MANUAL bindings remain visible through GetSubjectAccess and can be cleaned up through
// DeleteSubjectAccess.
func (s *Service) UpdateSubjectAccess(ctx context.Context, in UpdateSubjectAccessInput, applicationCode string) (Access, error) {
	in.TenantID = strings.TrimSpace(in.TenantID)
	in.SubjectID = strings.TrimSpace(in.SubjectID)
	in.OperatorID = strings.TrimSpace(in.OperatorID)
	applicationCode = strings.TrimSpace(applicationCode)
	subjectType, err := normalizeManagedSubjectType(in.SubjectType)
	if err != nil {
		return Access{}, err
	}
	if in.TenantID == "" || in.SubjectID == "" || in.OperatorID == "" || applicationCode == "" {
		return Access{}, validation("tenant_id, subject_type, subject_id, operator_id and application_code are required")
	}
	return Access{}, validateSubjectAccessWritePolicy(subjectType)
}

// DeleteSubjectAccess disables only the selected organization or position's direct bindings.
func (s *Service) DeleteSubjectAccess(ctx context.Context, in DeleteSubjectAccessInput, applicationCode string) error {
	in.TenantID = strings.TrimSpace(in.TenantID)
	in.SubjectID = strings.TrimSpace(in.SubjectID)
	in.OperatorID = strings.TrimSpace(in.OperatorID)
	applicationCode = strings.TrimSpace(applicationCode)
	subjectType, err := normalizeManagedSubjectType(in.SubjectType)
	if err != nil {
		return err
	}
	if in.TenantID == "" || in.SubjectID == "" || in.OperatorID == "" || applicationCode == "" {
		return validation("tenant_id, subject_type, subject_id, operator_id and application_code are required")
	}
	application, err := s.findApplication(ctx, in.TenantID, applicationCode)
	if err != nil {
		return err
	}
	if err := s.ensureManagedSubject(ctx, in.TenantID, subjectType, in.SubjectID); err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		directClause, directArgs := manualSubjectRoleBindingFilter(in.TenantID, application.ID, subjectType, in.SubjectID)
		applicationRoleIDs := tx.Table("authz_role").Select("id").Where("tenant_id = ? AND application_id = ? AND role_type = ?", in.TenantID, application.ID, applicationRoleType)
		result := tx.Table("authz_role_binding AS rb").Where(directClause, directArgs...).Where("rb.role_id IN (?)", applicationRoleIDs).Where("rb.status = ?", activeStatus).Updates(map[string]any{
			"status": disabledStatus, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": in.OperatorID,
		})
		if result.Error != nil {
			return fmt.Errorf("revoke application subject role bindings: %w", result.Error)
		}
		changed = result.RowsAffected > 0
		if changed {
			return bumpRevision(tx, in.TenantID, application.ID, now, "application subject access revoked")
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.recordAudit(ctx, AuditEvent{
		TenantID: in.TenantID, ApplicationID: application.ID, ApplicationCode: application.Code,
		OperatorID: in.OperatorID, OperatorName: in.OperatorName, SubjectID: in.SubjectID, Action: "authorization.application_subject_access.deleted",
		ResourceType: "application_subject_access", ResourceID: in.SubjectID, Result: "SUCCESS", RiskLevel: "HIGH",
		Summary: "应用组织岗位授权已删除", OccurredAt: now,
		Metadata: map[string]any{"subject_type": subjectType, "changed": changed},
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

func normalizeManagedSubjectType(subjectType string) (string, error) {
	subjectType = strings.ToUpper(strings.TrimSpace(subjectType))
	if subjectType != subjectTypeOrgUnit && subjectType != subjectTypePosition {
		return "", validation("subject_type must be ORG_UNIT or POSITION")
	}
	return subjectType, nil
}

// validateSubjectAccessWritePolicy prevents the generic application-access endpoint from
// becoming a second standard-authorization mechanism. Organization and position bindings that
// predate position templates are migration residue: administrators may inspect and revoke them,
// but only the positiongrant materializer may create TEMPLATE-origin POSITION bindings.
func validateSubjectAccessWritePolicy(subjectType string) error {
	switch subjectType {
	case subjectTypeOrgUnit, subjectTypePosition:
		return validation("manual ORG_UNIT/POSITION application role writes are disabled; use a position authorization template")
	default:
		return validation("subject application role writes are not supported")
	}
}

func (s *Service) ensureManagedSubject(ctx context.Context, tenantID, subjectType, subjectID string) error {
	var count int64
	var query *gorm.DB
	switch subjectType {
	case subjectTypeOrgUnit:
		query = s.db.WithContext(ctx).Table("iam_org_unit").Where("tenant_id = ? AND id = ? AND status = ?", tenantID, subjectID, activeStatus)
	case subjectTypePosition:
		query = s.db.WithContext(ctx).Table("iam_position AS p").Joins("JOIN iam_org_unit AS o ON o.id = p.org_unit_id AND o.tenant_id = p.tenant_id AND o.status = ?", activeStatus).Where("p.tenant_id = ? AND p.id = ? AND p.status = ?", tenantID, subjectID, activeStatus)
	default:
		return validation("subject_type must be ORG_UNIT or POSITION")
	}
	if err := query.Count(&count).Error; err != nil {
		return fmt.Errorf("load application authorization subject: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) resolveRoleBindings(ctx context.Context, tenantID, applicationID string, roles []RoleInput) ([]resolvedBinding, error) {
	// 外部只提交稳定角色编码和作用域；此处解析为当前租户、当前应用的活动目录角色，
	// 禁止使用数据库 ID 绕过目录所有权、角色状态或受保护角色策略。
	resolved := make([]resolvedBinding, 0, len(roles))
	for _, role := range roles {
		var roleRecord roleRow
		// Application access is an application-owned boundary. Platform control-plane roles
		// must only be managed by the generic, protected role-binding API. Keeping the role
		// type predicate here prevents a platform-super-admin assignment from being smuggled
		// through an application access update.
		if err := s.db.WithContext(ctx).Table("authz_role").Where("tenant_id = ? AND application_id = ? AND status = ? AND code = ? AND role_type = ?", tenantID, applicationID, activeStatus, role.RoleCode, applicationRoleType).Take(&roleRecord).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, validation("one or more application roles do not exist or are disabled")
			}
			return nil, fmt.Errorf("load application role: %w", err)
		}
		if err := ensureApplicationAccessRole(roleRecord); err != nil {
			return nil, err
		}
		scopeType, scopeID := scopeTypeTenant, ""
		if role.ScopeType == scopeTypeEnvironment {
			var environment struct {
				ID string `gorm:"column:id"`
			}
			if err := s.db.WithContext(ctx).Table("platform_application_environment").Select("id").Where("tenant_id = ? AND application_id = ? AND environment = ? AND status = ?", tenantID, applicationID, role.EnvironmentCode, activeStatus).Take(&environment).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil, validation("environment does not exist or is disabled")
				}
				return nil, fmt.Errorf("load application environment: %w", err)
			}
			scopeType, scopeID = scopeTypeEnvironment, environment.ID
		}
		resolved = append(resolved, resolvedBinding{roleID: roleRecord.ID, scopeType: scopeType, scopeID: scopeID, role: role})
	}
	return resolved, nil
}

func ensureApplicationAccessRole(role roleRow) error {
	if !strings.EqualFold(strings.TrimSpace(role.RoleType), applicationRoleType) {
		return validation("one or more application roles do not exist or are disabled")
	}
	return nil
}

// validateDirectRoleLimit rejects a role set that the target application can never
// authorize. This protects management writes from persisting an immediately
// conflicting direct assignment. Cross-source conflicts remain fail-closed at
// authorization resolution because they depend on effective memberships.
func (s *Service) validateDirectRoleLimit(ctx context.Context, tenantID, applicationID string, resolved []resolvedBinding) error {
	policy, err := s.ResolveApplicationAuthorizationPolicy(ctx, tenantID, applicationID)
	if err != nil {
		return err
	}
	roleIDs := make([]string, 0, len(resolved))
	for _, binding := range resolved {
		roleIDs = append(roleIDs, binding.roleID)
	}
	return validateMaximumRoleCount(policy.MaxEffectiveRoles, roleIDs)
}

func validateMaximumRoleCount(maximum int, roleIDs []string) error {
	if maximum <= 0 || len(sortedUnique(roleIDs)) <= maximum {
		return nil
	}
	return validation("the selected roles exceed the application's maximum effective role count")
}

// replaceManualUserRoleBindings owns only MANUAL USER grants. Keeping USER fixed inside this
// persistence helper makes it impossible for a future caller to accidentally use it for ACCOUNT,
// ORG_UNIT or POSITION bindings; TEMPLATE and SYSTEM rows are excluded by the query predicate.
func (s *Service) replaceManualUserRoleBindings(tx *gorm.DB, tenantID, applicationID, userID, operatorID string, resolved []resolvedBinding, now time.Time) (bool, error) {
	// 采用集合对账而非“全删全建”：保留未变化绑定的 ID 和审计链，恢复历史同源
	// 绑定时递增版本；只将本次不再需要的 MANUAL 绑定置为 DISABLED。
	var existing []bindingRow
	directClause, directArgs := manualDirectApplicationRoleBindingFilter(tenantID, applicationID, userID)
	if err := tx.Table("authz_role_binding AS rb").Select("rb.id, rb.role_id, rb.scope_type, rb.scope_id, rb.valid_from, rb.valid_until, rb.status, rb.version").Joins("JOIN authz_role AS r ON r.id = rb.role_id AND r.tenant_id = rb.tenant_id AND r.application_id = rb.application_id").Where(directClause, directArgs...).Where("r.role_type = ?", applicationRoleType).Find(&existing).Error; err != nil {
		return false, fmt.Errorf("load existing application role bindings: %w", err)
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
	changed := false
	for _, binding := range existing {
		if _, keep := desired[key(binding.RoleID, binding.ScopeType, binding.ScopeID)]; keep || binding.Status != activeStatus {
			continue
		}
		if err := tx.Table("authz_role_binding").Where("id = ?", binding.ID).Updates(map[string]any{"status": disabledStatus, "version": binding.Version + 1, "updated_at": now, "updated_by": operatorID}).Error; err != nil {
			return false, fmt.Errorf("disable removed application role binding: %w", err)
		}
		changed = true
	}
	for bindingKey, item := range desired {
		if binding, exists := byKey[bindingKey]; exists {
			if binding.Status == activeStatus && sameValidity(binding.ValidFrom, item.role.ValidFrom) && sameValidity(binding.ValidUntil, item.role.ValidUntil) {
				continue
			}
			if err := tx.Table("authz_role_binding").Where("id = ?", binding.ID).Updates(map[string]any{"status": activeStatus, "valid_from": item.role.ValidFrom, "valid_until": item.role.ValidUntil, "version": binding.Version + 1, "updated_at": now, "updated_by": operatorID}).Error; err != nil {
				return false, fmt.Errorf("activate application role binding: %w", err)
			}
			changed = true
			continue
		}
		id, err := s.ids.New(now)
		if err != nil {
			return false, fmt.Errorf("generate application role binding ID: %w", err)
		}
		if err := tx.Table("authz_role_binding").Create(map[string]any{"id": id, "tenant_id": tenantID, "application_id": applicationID, "role_id": item.roleID, "subject_type": subjectTypeUser, "subject_id": userID, "scope_type": item.scopeType, "scope_id": item.scopeID, "valid_from": item.role.ValidFrom, "valid_until": item.role.ValidUntil, "status": activeStatus, "grant_origin": grantOriginManual, "origin_id": "", "origin_item_id": "", "version": 1, "created_at": now, "created_by": operatorID, "updated_at": now, "updated_by": operatorID}).Error; err != nil {
			return false, fmt.Errorf("create application role binding: %w", err)
		}
		changed = true
	}
	return changed, nil
}

func accessAuditChanges(before, after Access) []AuditFieldChange {
	changes := make([]AuditFieldChange, 0, 4)
	appendChange := func(field string, left, right any) {
		if reflect.DeepEqual(left, right) {
			return
		}
		changes = append(changes, AuditFieldChange{Field: field, Before: left, After: right})
	}
	appendChange("manual_role_codes", manualRoleCodes(before.ManualRoles), manualRoleCodes(after.ManualRoles))
	appendChange("effective_role_codes", roleViewCodes(before.Roles), roleViewCodes(after.Roles))
	appendChange("effective_permissions", before.EffectivePermissions, after.EffectivePermissions)
	appendChange("authorization_state", before.AuthorizationState, after.AuthorizationState)
	return changes
}

func manualRoleCodes(roles []RoleView) []string { return roleViewCodes(roles) }
func roleViewCodes(roles []RoleView) []string {
	codes := make([]string, 0, len(roles))
	for _, role := range roles {
		codes = append(codes, role.Code)
	}
	return sortedUnique(codes)
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

func emptyAccess(applicationCode, environmentCode, roleConfigHash string, revision uint64) Access {
	return Access{
		ApplicationCode: applicationCode,
		EnvironmentCode: environmentCode,
		Roles:           []RoleView{}, DirectRoles: []RoleView{}, InheritedRoles: []RoleView{}, ManualRoles: []RoleView{},
		RolePermissions: []string{}, CustomPermissions: []string{}, EffectivePermissions: []string{},
		RoleConfigHash: roleConfigHash, AuthzRevision: revision,
		AuthorizationState: authorizationUnauthorized, Conflicts: []string{},
	}
}

func (s *Service) getSubjectAccessByApplication(ctx context.Context, tenantID, subjectType, subjectID, applicationID, applicationCode, environmentID, environmentCode string) (Access, error) {
	roles, err := s.loadSubjectRoles(ctx, tenantID, applicationID, subjectType, subjectID, environmentID, s.clock.Now().UTC())
	if err != nil {
		return Access{}, err
	}
	roleIDs, roleViews, directRoles, _ := resolveAssignedRoles(roles, subjectType)
	state, conflicts, permissionRoleIDs, err := s.applicationRolePolicy(ctx, tenantID, applicationID, roles, roleIDs)
	if err != nil {
		return Access{}, err
	}
	if state == authorizationUnauthorized {
		return Access{}, ErrNotConfigured
	}
	rolePermissions := []string{}
	if state == authorizationGranted {
		rolePermissions, err = s.loadRolePermissions(ctx, tenantID, applicationID, permissionRoleIDs)
		if err != nil {
			return Access{}, err
		}
		rolePermissions = sortedUnique(rolePermissions)
	}
	roleConfigHash, err := s.loadRoleConfigHash(ctx, tenantID, applicationID)
	if err != nil {
		return Access{}, err
	}
	revision, err := s.loadRevision(ctx, tenantID, applicationID)
	if err != nil {
		return Access{}, err
	}
	access := Access{
		ApplicationCode: applicationCode, EnvironmentCode: environmentCode,
		Roles: roleViews, DirectRoles: directRoles, InheritedRoles: []RoleView{}, ManualRoles: filterManualRoles(roleViews),
		RolePermissions: rolePermissions, CustomPermissions: []string{}, EffectivePermissions: append([]string(nil), rolePermissions...),
		RoleConfigHash: roleConfigHash, AuthzRevision: revision,
		AuthorizationState: state, Conflicts: conflicts,
	}
	access.Role = roleViews[0]
	return access, nil
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
	roleIDs, roleViews, directRoles, inheritedRoles := resolveAssignedRoles(roles, subjectTypeUser)
	state, conflicts, permissionRoleIDs, err := s.applicationRolePolicy(ctx, tenantID, applicationID, roles, roleIDs)
	if err != nil {
		return Access{}, err
	}
	rolePermissions := []string{}
	if state == authorizationGranted {
		rolePermissions, err = s.loadRolePermissions(ctx, tenantID, applicationID, permissionRoleIDs)
		if err != nil {
			return Access{}, err
		}
		rolePermissions = sortedUnique(rolePermissions)
	}
	effective, err := effectivePermissionsForApplication(state, rolePermissions)
	if err != nil {
		return Access{}, err
	}

	roleConfigHash, err := s.loadRoleConfigHash(ctx, tenantID, applicationID)
	if err != nil {
		return Access{}, err
	}
	revision, err := s.loadRevision(ctx, tenantID, applicationID)
	if err != nil {
		return Access{}, err
	}
	access := Access{
		ApplicationCode: applicationCode, EnvironmentCode: environmentCode,
		Roles: roleViews, DirectRoles: directRoles, InheritedRoles: inheritedRoles, ManualRoles: filterManualRoles(roleViews),
		RolePermissions: rolePermissions, CustomPermissions: []string{}, EffectivePermissions: effective,
		RoleConfigHash: roleConfigHash, AuthzRevision: revision,
		AuthorizationState: state, Conflicts: conflicts,
	}
	if len(roleViews) > 0 {
		access.Role = roleViews[0]
	}
	return access, nil
}

// applicationRolePolicy applies the target application's catalog-declared
// effective-role limit. The platform never derives this rule from an
// application code, a role name, or a permission code.
func (s *Service) applicationRolePolicy(ctx context.Context, tenantID, applicationID string, rows []assignedRoleRow, roleIDs []string) (string, []string, []string, error) {
	policy, err := s.ResolveApplicationAuthorizationPolicy(ctx, tenantID, applicationID)
	if err != nil {
		return authorizationUnauthorized, nil, nil, err
	}
	state, conflicts, permittedRoleIDs := applyApplicationRolePolicy(policy, rows, roleIDs)
	return state, conflicts, permittedRoleIDs, nil
}

// applyApplicationRolePolicy is intentionally side-effect free so the limit
// semantics remain independently testable. Rows are used only to provide a
// stable, administrator-readable conflict list; role IDs determine effective
// cardinality because they are the actual assigned role records.
func applyApplicationRolePolicy(policy ApplicationAuthorizationPolicy, rows []assignedRoleRow, roleIDs []string) (string, []string, []string) {
	distinctRoleIDs := sortedUnique(roleIDs)
	if len(distinctRoleIDs) == 0 {
		return authorizationUnauthorized, []string{}, []string{}
	}
	if policy.MaxEffectiveRoles == 0 || len(distinctRoleIDs) <= policy.MaxEffectiveRoles {
		return authorizationGranted, []string{}, distinctRoleIDs
	}
	// 冲突来源继续返回给管理界面解释，但不把任何冲突角色放入有效权限，防止通过
	// 叠加用户、组织、岗位等来源绕过应用声明的最大角色数。

	roleCodeByID := make(map[string]string, len(rows))
	for _, row := range rows {
		roleID := strings.TrimSpace(row.RoleID)
		roleCode := strings.TrimSpace(row.Code)
		if roleID == "" || roleCode == "" {
			continue
		}
		if existing, exists := roleCodeByID[roleID]; !exists || roleCode < existing {
			roleCodeByID[roleID] = roleCode
		}
	}
	conflicts := make([]string, 0, len(distinctRoleIDs))
	for _, roleID := range distinctRoleIDs {
		if code := roleCodeByID[roleID]; code != "" {
			conflicts = append(conflicts, code)
		} else {
			conflicts = append(conflicts, roleID)
		}
	}
	return authorizationConflict, sortedUnique(conflicts), []string{}
}

// validateCustomPermissionsUpdate enforces the platform boundary: applications
// publish their role-permission catalog, while the platform assigns roles only.
// Historical authz_user_permission rows are never read for authorization.
func validateCustomPermissionsUpdate(customPermissionsProvided bool) error {
	if customPermissionsProvided {
		return validation("custom_permissions are not supported; assign an application role instead")
	}
	return nil
}

// effectivePermissionsForApplication derives every effective permission from the
// synchronized application role catalog. A conflict fails closed and an
// unconfigured user cannot receive permissions from historical direct grants.
func effectivePermissionsForApplication(state string, rolePermissions []string) ([]string, error) {
	if state == authorizationConflict {
		return []string{}, nil
	}
	if state == authorizationUnauthorized {
		return nil, ErrNotConfigured
	}
	return sortedUnique(rolePermissions), nil
}

func requireGrantedAuthorization(access Access) error {
	if access.AuthorizationState != authorizationGranted {
		return ErrAccessDenied
	}
	return nil
}

func externalScopeType(scopeType string) string {
	if scopeType == scopeTypeTenant {
		return "APPLICATION"
	}
	return scopeType
}

func resolveAssignedRoles(rows []assignedRoleRow, directSubjectType string) ([]string, []RoleView, []RoleView, []RoleView) {
	roleIDs := make([]string, 0, len(rows))
	roles := make([]RoleView, 0, len(rows))
	directRoles := make([]RoleView, 0, len(rows))
	inheritedRoles := make([]RoleView, 0, len(rows))
	seenBindings := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		roleIDs = append(roleIDs, row.RoleID)
		// Provenance is part of a displayed binding. A manual and a template binding can
		// legitimately grant the same role from the same position and must remain
		// separately traceable, even though the effective role count is de-duplicated by
		// role ID below.
		bindingKey := row.RoleID + "\x00" + row.SubjectType + "\x00" + row.SubjectID + "\x00" + row.ScopeType + "\x00" + row.ScopeID + "\x00" + normalizedGrantOrigin(row.GrantOrigin) + "\x00" + row.OriginID + "\x00" + row.OriginItemID
		if _, exists := seenBindings[bindingKey]; exists {
			continue
		}
		seenBindings[bindingKey] = struct{}{}
		sourceKind := sourceKindForRole(row, row.SubjectType == directSubjectType)
		view := RoleView{
			Code: row.Code, Name: row.Name, ScopeType: externalScopeType(row.ScopeType),
			EnvironmentCode: row.EnvironmentCode, ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil,
			SourceType: row.SubjectType, SourceID: row.SubjectID, SourceName: row.SourceName,
			Direct: row.SubjectType == directSubjectType, GrantOrigin: normalizedGrantOrigin(row.GrantOrigin),
			OriginID: row.OriginID, OriginItemID: row.OriginItemID, SourceKind: sourceKind,
			AssignmentID: row.AssignmentID, TemplateID: row.TemplateID, TemplateCode: row.TemplateCode, TemplateName: row.TemplateName,
			PositionID: row.PositionID, PositionName: row.PositionName,
		}
		roles = append(roles, view)
		if view.Direct {
			directRoles = append(directRoles, view)
		} else {
			inheritedRoles = append(inheritedRoles, view)
		}
	}
	return sortedUnique(roleIDs), roles, directRoles, inheritedRoles
}

func normalizedGrantOrigin(origin string) string {
	origin = strings.ToUpper(strings.TrimSpace(origin))
	if origin == "" {
		return grantOriginManual
	}
	return origin
}

func sourceKindForRole(row assignedRoleRow, direct bool) string {
	switch normalizedGrantOrigin(row.GrantOrigin) {
	case grantOriginSystem:
		return sourceKindSystem
	case grantOriginTemplate:
		return sourceKindInherited
	default:
		if direct {
			return sourceKindManual
		}
		return sourceKindInherited
	}
}

func filterManualRoles(rows []RoleView) []RoleView {
	result := make([]RoleView, 0, len(rows))
	for _, row := range rows {
		if row.SourceKind == sourceKindManual && row.Direct {
			result = append(result, row)
		}
	}
	return result
}

func applicationAccessSubjectFilter(userID string, now time.Time) (string, []any) {
	return `(
		rb.subject_type = ? AND rb.subject_id = ?
		OR (
			rb.subject_type IN (?, ?)
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
				WHERE membership.tenant_id = rb.tenant_id
					AND membership.user_id = ?
					AND membership.status = ?
					AND membership.inherit_authorization = ?
					AND (membership.valid_from IS NULL OR membership.valid_from <= ?)
					AND (membership.valid_until IS NULL OR membership.valid_until > ?)
					AND (
						(rb.subject_type = ? AND membership.org_unit_id = rb.subject_id)
						OR (rb.subject_type = ? AND membership.position_id = rb.subject_id)
					)
			)
		)
	)`, []any{
			subjectTypeUser, userID,
			subjectTypeOrgUnit, subjectTypePosition,
			activeStatus, activeStatus,
			userID, activeStatus, true, now, now,
			subjectTypeOrgUnit, subjectTypePosition,
		}
}

func applicationAccessScopeFilter(environmentID string) (string, []any) {
	if strings.TrimSpace(environmentID) == "" {
		// Management reads return all environment-specific bindings so administrators can see
		// the complete assignment set. OIDC always supplies an environment ID.
		return "((rb.scope_type = ? AND rb.scope_id = ?) OR rb.scope_type = ?)", []any{scopeTypeTenant, "", scopeTypeEnvironment}
	}
	return "((rb.scope_type = ? AND rb.scope_id = ?) OR (rb.scope_type = ? AND rb.scope_id = ?))", []any{scopeTypeTenant, "", scopeTypeEnvironment, environmentID}
}

func subjectRoleBindingFilter(tenantID, applicationID, subjectType, subjectID string) (string, []any) {
	return "rb.tenant_id = ? AND rb.application_id = ? AND rb.subject_type = ? AND rb.subject_id = ?", []any{tenantID, applicationID, subjectType, subjectID}
}

func manualDirectApplicationRoleBindingFilter(tenantID, applicationID, userID string) (string, []any) {
	clause, args := subjectRoleBindingFilter(tenantID, applicationID, subjectTypeUser, userID)
	return clause + " AND rb.grant_origin = ?", append(args, grantOriginManual)
}

// directApplicationRoleBindingFilter is retained for callers/tests that need the subject-only
// predicate. Mutation code uses the manual variant so template/system bindings are protected.
func directApplicationRoleBindingFilter(tenantID, applicationID, userID string) (string, []any) {
	return subjectRoleBindingFilter(tenantID, applicationID, subjectTypeUser, userID)
}

func manualSubjectRoleBindingFilter(tenantID, applicationID, subjectType, subjectID string) (string, []any) {
	clause, args := subjectRoleBindingFilter(tenantID, applicationID, subjectType, subjectID)
	return clause + " AND rb.grant_origin = ?", append(args, grantOriginManual)
}

func (s *Service) loadSubjectRoles(ctx context.Context, tenantID, applicationID, subjectType, subjectID, environmentID string, now time.Time) ([]assignedRoleRow, error) {
	var rows []assignedRoleRow
	scopeClause, scopeArgs := applicationAccessScopeFilter(environmentID)
	query := s.db.WithContext(ctx).Table("authz_role_binding AS rb").Select(`
		r.id AS role_id, r.code, r.name, rb.scope_type, rb.scope_id,
		e.environment AS environment_code, rb.valid_from, rb.valid_until,
		rb.subject_type, rb.subject_id, rb.grant_origin, rb.origin_id, rb.origin_item_id,
		ta.id AS assignment_id, ta.template_id, template.code AS template_code, template.name AS template_name,
		rb.subject_id AS position_id, COALESCE(p.name, '') AS position_name,
		CASE rb.subject_type
			WHEN 'ORG_UNIT' THEN COALESCE(o.name, '')
			WHEN 'POSITION' THEN COALESCE(p.name, '')
			ELSE ''
		END AS source_name`).
		Joins("JOIN authz_role AS r ON r.id = rb.role_id AND r.tenant_id = rb.tenant_id AND r.application_id = rb.application_id AND r.status = ?", activeStatus).
		Joins("LEFT JOIN platform_application_environment AS e ON e.id = rb.scope_id AND e.application_id = rb.application_id AND e.tenant_id = rb.tenant_id").
		Joins("LEFT JOIN iam_org_unit AS o ON rb.subject_type = ? AND o.id = rb.subject_id AND o.tenant_id = rb.tenant_id", subjectTypeOrgUnit).
		Joins("LEFT JOIN iam_position AS p ON rb.subject_type = ? AND p.id = rb.subject_id AND p.tenant_id = rb.tenant_id", subjectTypePosition).
		Joins("LEFT JOIN authz_position_grant_template_assignment AS ta ON ta.tenant_id = rb.tenant_id AND ta.id = rb.origin_id AND rb.grant_origin = ?", grantOriginTemplate).
		Joins("LEFT JOIN authz_position_grant_template AS template ON template.tenant_id = ta.tenant_id AND template.id = ta.template_id").
		Where("rb.tenant_id = ? AND rb.application_id = ? AND rb.subject_type = ? AND rb.subject_id = ? AND rb.status = ? AND r.role_type = ?", tenantID, applicationID, subjectType, subjectID, activeStatus, applicationRoleType).
		Where("(rb.valid_from IS NULL OR rb.valid_from <= ?) AND (rb.valid_until IS NULL OR rb.valid_until > ?)", now, now).
		Where(scopeClause, scopeArgs...)
	if err := query.Order("r.code ASC, rb.scope_type ASC, rb.scope_id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load application subject role bindings: %w", err)
	}
	return rows, nil
}

func (s *Service) loadGenericRoles(ctx context.Context, tenantID, applicationID, userID, environmentID string, now time.Time) ([]assignedRoleRow, error) {
	// 用户直绑、组织绑定和岗位绑定在查询时合并；组织/岗位来源仅对当前有效且允许
	// 继承授权的任职生效，因此任职调整无需等待重新登录。
	var rows []assignedRoleRow
	subjectClause, subjectArgs := applicationAccessSubjectFilter(userID, now)
	scopeClause, scopeArgs := applicationAccessScopeFilter(environmentID)
	query := s.db.WithContext(ctx).Table("authz_role_binding AS rb").Select(`
		r.id AS role_id, r.code, r.name, rb.scope_type, rb.scope_id,
		e.environment AS environment_code, rb.valid_from, rb.valid_until,
		rb.subject_type, rb.subject_id, rb.grant_origin, rb.origin_id, rb.origin_item_id,
		ta.id AS assignment_id, ta.template_id, template.code AS template_code, template.name AS template_name,
		rb.subject_id AS position_id, COALESCE(p.name, '') AS position_name,
		CASE rb.subject_type
			WHEN 'USER' THEN COALESCE(u.display_name, '')
			WHEN 'ORG_UNIT' THEN COALESCE(o.name, '')
			WHEN 'POSITION' THEN COALESCE(p.name, '')
			ELSE ''
		END AS source_name`).
		Joins("JOIN authz_role AS r ON r.id = rb.role_id AND r.tenant_id = rb.tenant_id AND r.application_id = rb.application_id AND r.status = ?", activeStatus).
		Joins("LEFT JOIN platform_application_environment AS e ON e.id = rb.scope_id AND e.application_id = rb.application_id AND e.tenant_id = rb.tenant_id").
		Joins("LEFT JOIN iam_user AS u ON rb.subject_type = ? AND u.id = rb.subject_id AND u.tenant_id = rb.tenant_id", subjectTypeUser).
		Joins("LEFT JOIN iam_org_unit AS o ON rb.subject_type = ? AND o.id = rb.subject_id AND o.tenant_id = rb.tenant_id", subjectTypeOrgUnit).
		Joins("LEFT JOIN iam_position AS p ON rb.subject_type = ? AND p.id = rb.subject_id AND p.tenant_id = rb.tenant_id", subjectTypePosition).
		Joins("LEFT JOIN authz_position_grant_template_assignment AS ta ON ta.tenant_id = rb.tenant_id AND ta.id = rb.origin_id AND rb.grant_origin = ?", grantOriginTemplate).
		Joins("LEFT JOIN authz_position_grant_template AS template ON template.tenant_id = ta.tenant_id AND template.id = ta.template_id").
		Where("rb.tenant_id = ? AND rb.application_id = ? AND rb.status = ? AND r.role_type = ?", tenantID, applicationID, activeStatus, applicationRoleType).
		Where(subjectClause, subjectArgs...).
		Where("(rb.valid_from IS NULL OR rb.valid_from <= ?) AND (rb.valid_until IS NULL OR rb.valid_until > ?)", now, now).
		Where(scopeClause, scopeArgs...)
	if err := query.Order("CASE rb.subject_type WHEN 'USER' THEN 0 WHEN 'ORG_UNIT' THEN 1 WHEN 'POSITION' THEN 2 ELSE 3 END ASC, r.code ASC, rb.scope_type ASC, rb.scope_id ASC, rb.subject_id ASC").Find(&rows).Error; err != nil {
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

func (s *Service) loadRoleConfigHash(ctx context.Context, tenantID, applicationID string) (string, error) {
	// A subsystem may publish an opaque configuration hash for its own Claims
	// compatibility contract. The base platform deliberately does not inspect or
	// recompute that value from business role/permission semantics.
	var metadata catalogMetadataRow
	err := s.db.WithContext(ctx).Table("authz_authorization_catalog").
		Select("claims_role_config_hash").
		Where("tenant_id = ? AND application_id = ?", tenantID, applicationID).
		Take(&metadata).Error
	if err == nil && strings.TrimSpace(metadata.ClaimsRoleConfigHash) != "" {
		return metadata.ClaimsRoleConfigHash, nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("load application claims role configuration hash: %w", err)
	}

	// Catalogs that do not publish an application-specific Claims hash retain a
	// generic, deterministic mirror hash. This is a fallback, not a per-app rule.
	var rows []catalogRow
	err = s.db.WithContext(ctx).Table("authz_role AS r").Select("r.code AS role_code, p.code AS permission_code, rp.effect").
		Joins("LEFT JOIN authz_role_permission AS rp ON rp.role_id = r.id").
		Joins("LEFT JOIN authz_permission AS p ON p.id = rp.permission_id AND p.tenant_id = r.tenant_id AND p.application_id = r.application_id AND p.status = ?", activeStatus).
		Where("r.tenant_id = ? AND r.application_id = ? AND r.status = ?", tenantID, applicationID, activeStatus).
		Find(&rows).Error
	if err != nil {
		return "", fmt.Errorf("load application role configuration: %w", err)
	}
	return roleConfigHash(rows), nil
}

// roleConfigHash is the generic catalog version carried in tokens when the
// application did not publish an explicit Claims compatibility hash. It hashes
// synchronized role-permission mappings only: role names, source type and
// built-in flags are presentation/provenance metadata, not authorization policy.
func roleConfigHash(rows []catalogRow) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, strings.TrimSpace(row.RoleCode)+"\x00"+strings.TrimSpace(row.PermissionCode)+"\x00"+strings.TrimSpace(row.Effect))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
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
	// revision 是子系统授权缓存失效协议的一部分；数据库原子自增避免并发修改丢版本。
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
		// 未提供 roles 表示“不修改”；显式空数组才表示撤销全部人工直绑。
		return nil, nil
	}
	byRoleCode := make(map[string]RoleInput, len(inputs))
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
		if _, exists := byRoleCode[input.RoleCode]; exists {
			return nil, validation("the same application role can only be assigned once")
		}
		byRoleCode[input.RoleCode] = input
	}
	roleCodes := make([]string, 0, len(byRoleCode))
	for roleCode := range byRoleCode {
		roleCodes = append(roleCodes, roleCode)
	}
	sort.Strings(roleCodes)
	result := make([]RoleInput, 0, len(roleCodes))
	for _, roleCode := range roleCodes {
		result = append(result, byRoleCode[roleCode])
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
