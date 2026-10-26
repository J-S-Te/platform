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

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/delegation"
	"github.com/J-S-Te/Basic-Platform/internal/platform/oidc/interfaces/tokenissuer"
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

	grantOriginManual    = "MANUAL"
	grantOriginTemplate  = "TEMPLATE"
	grantOriginInherited = "INHERITANCE" // 仅用于读取历史审计数据；新绑定不再生成该来源
	grantOriginSystem    = "SYSTEM"

	applicationRoleType = "APPLICATION"
	platformRoleType    = "PLATFORM"

	sourceKindManual    = "MANUAL"
	sourceKindInherited = "INHERITED"
	sourceKindSystem    = "SYSTEM"

	// PlatformApplicationCode 是迁移种子内置应用，承载平台自身的角色与权限。
	// 目录镜像通常由子系统的 catalog-publisher OAuth client 发布；平台自身没有
	// 这样的 client，因此由 API bootstrap 把迁移种子的角色/权限数据镜像到
	// 应用自有的目录行。
	PlatformApplicationCode = "platform"
	// PlatformCatalogVersion 是 bootstrap 镜像固定的 catalog_version。漂移检测
	// 依赖 catalog_hash；版本刻意固定，使平台可以反复确认内置数据，而 UI 不会
	// 在每次 API 重启时误判为"新版本"。
	PlatformCatalogVersion = "v1-platform-builtin"
	// PlatformCatalogSourceType 是 API 镜像发布内置数据时使用的 source_type。
	// 子系统目录用 "APPLICATION"；平台镜像自成一类，不是外部发布的清单。
	PlatformCatalogSourceType = "BUILTIN"
	// PlatformCatalogSourceIdentifier 是与 PlatformCatalogSourceType 配对的
	// source_identifier，仅供审计历史描述使用。
	PlatformCatalogSourceIdentifier = "platform:bootstrap"
	// BootstrapSuperAdminRoleCode 是仅由受控的首位超级管理员流程授予的内置角色。
	// 它镜像 identityapplication.BootstrapSuperAdminRoleCode，使平台目录 bootstrap
	// 无需导入 identity application 包（否则会造成 import cycle）即可定位首位管理员。
	BootstrapSuperAdminRoleCode = "platform-super-admin"
	// PlatformCatalogBootstrapOperatorID 是 26 位 Crockford Base32 占位符，在首位
	// 超级管理员尚未创建时充当平台目录的 last_synced_by。"PLATFSY" 后缀让该占位符
	// 在审计历史中一眼可辨，绝不会与真实 ULID 编码的用户 ID 混淆。
	PlatformCatalogBootstrapOperatorID = "01J0000000000000PLATFSY000"
)

var (
	// ErrNotFound 表示目标资源在当前租户内不存在。
	ErrNotFound = errors.New("application authorization resource not found")
	// ErrAccessReadback 表示授权变更（含授权版本与 Keycloak Outbox）已在数据库原子
	// 提交，但随后构造响应视图的读取失败。它与「保存失败」必须区分：保存事实已
	// 成立，客户端应提示「已保存、读取最新状态失败」而不是重试保存。
	ErrAccessReadback = errors.New("authorization committed but the updated access view could not be read")
	// ErrNotConfigured 表示主体尚未配置该应用的任何授权。这是合法业务状态而非故障，
	// 查询方通常以携带当前目录哈希的空授权视图呈现，写入方视为主体无可撤销内容。
	ErrNotConfigured = errors.New("user application access is not configured")
	// ErrValidation 表示请求输入不满足业务约束；错误消息携带可安全展示给管理界面的
	// 静态原因（如角色不可用、作用域非法），不含基础设施细节。
	ErrValidation = errors.New("application authorization validation failed")
	// ErrAccessDenied 表示主体未被授权访问目标应用（有效授权状态非 GRANTED）。
	ErrAccessDenied = errors.New("user is not authorized for application")
	// ErrRevisionConflict 表示整组保存携带的 edit_revision 条件已陈旧：可编辑授权
	// 集合被其他管理员先行修改。调用方必须重新读取后再决策，禁止自动换新版本重提。
	ErrRevisionConflict = errors.New("application access edit revision conflict")
)

// IdentifierGenerator 生成全局唯一、按时间可排序的记录标识（通常为 ULID），
// 用于新建角色绑定等持久化行的主键。
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock 提供当前时间；测试可注入确定性时钟使授权有效期断言可复现。
type Clock interface{ Now() time.Time }

// SystemClock 是生产环境时钟，统一返回 UTC 时间。
type SystemClock struct{}

// Now 返回当前 UTC 时间。
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// Service 实现应用访问授权与授权目录的查询、变更和令牌授权解析。
// 所有查询以租号为边界；授权变更与 revision 推进、Keycloak 投影入队必须在
// 同一数据库事务内提交，保证子系统可靠感知授权变化。
type Service struct {
	db    *gorm.DB
	ids   IdentifierGenerator
	clock Clock
	audit AuditRecorder
}

// NewService 允许审计记录器缺省，使未安装平台审计管线的调用方也能构造服务。
// 可变参数形式在保持既有三参数构造契约的同时，让 bootstrap 可以注入一个记录器。
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

// RoleView 是一条应用角色的展示视图。同一角色可经用户直绑、组织/岗位模板、
// 平台角色继承等多个来源同时生效：视图按来源保留多条以便管理界面追溯，
// 有效角色数与权限集合则按角色 ID 去重。
type RoleView struct {
	Code            string     `json:"code"`
	Name            string     `json:"name"`
	ScopeType       string     `json:"scope_type"`
	ScopeID         string     `json:"scope_id,omitempty"`
	EnvironmentCode string     `json:"environment_code,omitempty"`
	ValidFrom       *time.Time `json:"valid_from,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
	SourceType      string     `json:"source_type"`
	SourceID        string     `json:"source_id"`
	SourceName      string     `json:"source_name"`
	Direct          bool       `json:"direct"`

	// GrantOrigin 是绑定的持久化来源标记。它与 SourceType 刻意分离：
	// SourceType 描述绑定挂在哪个主体上，GrantOrigin 描述绑定是如何被授予的。
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

// RoleInput 是写入侧的角色提交项。调用方只提交稳定角色编码与作用域；
// 平台负责解析为当前目录中的活动角色，禁止用数据库 ID 绕过目录所有权
// 与受保护角色策略。
type RoleInput struct {
	RoleCode        string     `json:"role_code"`
	ScopeType       string     `json:"scope_type"`
	EnvironmentCode string     `json:"environment_code,omitempty"`
	ValidFrom       *time.Time `json:"valid_from,omitempty"`
	ValidUntil      *time.Time `json:"valid_until,omitempty"`
}

// Access 是应用授权的完整视图：按来源分组的角色、有效权限、授权状态与目录
// 兼容性元数据。AuthorizationState 取 GRANTED/CONFLICT/UNAUTHORIZED；
// CONFLICT 表示超出应用目录声明的最大有效角色数，此时有效权限为空（fail-closed），
// 冲突角色编码随 Conflicts 返回供管理界面解释。
type Access struct {
	ApplicationCode string     `json:"application_code"`
	EnvironmentCode string     `json:"environment_code,omitempty"`
	Roles           []RoleView `json:"roles"`
	DirectRoles     []RoleView `json:"direct_roles"`
	InheritedRoles  []RoleView `json:"inherited_roles"`
	// ManualRoles 在 GetAccess 管理响应中包含未到期的人工 USER 分配（含未来生效）。
	// 它不是有效权限来源；Roles/DirectRoles/EffectivePermissions 仍仅取当前有效项。
	ManualRoles []RoleView `json:"manual_roles"`
	// Role 仅为旧 SYS-004 访问端点的客户端保留。新客户端应使用 Roles，
	// 后者可承载多个应用角色。
	Role                 RoleView `json:"role"`
	RolePermissions      []string `json:"role_permissions"`
	CustomPermissions    []string `json:"custom_permissions"`
	EffectivePermissions []string `json:"effective_permissions"`
	RoleConfigHash       string   `json:"role_config_hash"`
	AuthzRevision        uint64   `json:"authz_revision"`
	AuthorizationState   string   `json:"authorization_state"`
	Conflicts            []string `json:"conflicts"`
	// EditRevision 是本响应中可编辑授权集合（MANUAL 直绑 + 历史自定义权限）的
	// 一致快照版本。写回时作为 expected_revision 条件提交；它由集合内容决定，
	// 与应用级 authz_revision 相互独立，不可由客户端推导或修改。
	EditRevision string `json:"edit_revision"`
}

// UpdateAccessInput 是用户应用授权的更新入参。RolesProvided 区分"未提供 roles
// （不修改角色）"与"显式空数组（撤销全部人工直绑）"；CustomPermissionsProvided
// 恒走拒绝路径——权限只能来自应用目录的角色映射，仅为兼容解码保留。
// ExpectedRevision 是条件写入版本（GET 响应的 edit_revision）：非空时在事务锁内
// 比较，陈旧返回 ErrRevisionConflict；兼容期允许为空。
// StrictRevisionRequired 来自部署配置（Handler 构造时读取环境变量），是方案
// 兼容发布第 2 步的开关：全部调用方传条件后置 true，缺省条件即 422 拒绝。
type UpdateAccessInput struct {
	TenantID                  string
	UserID                    string
	OperatorID                string
	OperatorName              string
	Roles                     []RoleInput
	RolesProvided             bool
	CustomPermissions         []string
	CustomPermissionsProvided bool
	ExpectedRevision          string
	StrictRevisionRequired    bool
}

// DeleteAccessInput 撤销用户在指定应用上的全部人工授权。
// ExpectedRevision/StrictRevisionRequired 语义与 UpdateAccessInput 相同；
// HTTP 层经查询参数承载条件，严格开关来自部署配置。
type DeleteAccessInput struct {
	TenantID              string
	UserID                string
	OperatorID            string
	OperatorName          string
	ExpectedRevision      string
	StrictRevisionRequired bool
}

// UpdateSubjectAccessInput 兼容保留的历史组织/岗位写入口入参；当前恒被写策略拒绝。
type UpdateSubjectAccessInput struct {
	TenantID      string
	SubjectType   string
	SubjectID     string
	OperatorID    string
	OperatorName  string
	Roles         []RoleInput
	RolesProvided bool
}

// DeleteSubjectAccessInput 停用单个组织/岗位的直接应用角色绑定。
type DeleteSubjectAccessInput struct {
	TenantID     string
	SubjectType  string
	SubjectID    string
	OperatorID   string
	OperatorName string
}

// TokenAuthorization 是写入平台 OIDC 令牌声明的应用授权快照：角色、有效权限、
// 组织 claims、目录兼容窗口（CatalogVersion/RoleConfigHash 及兼容列表）与修订号。
// 子系统以目录哈希和修订号识别授权变化或权限撤销，避免长期复用已失效的权限集合。
type TokenAuthorization struct {
	ApplicationCode            string
	EnvironmentCode            string
	TenantID                   string
	PersonID                   string
	PrimaryOrgID               string
	OrganizationIDs            []string
	Roles                      []string
	Permissions                []string
	RoleConfigHash             string
	CatalogVersion             string
	CompatibleCatalogVersions  []string
	CompatibleRoleConfigHashes []string
	AuthzRevision              uint64
	DataScopes                 []tokenissuer.DataScope
}

// KeycloakAuthorizationSnapshot 是供 Keycloak 投影 worker 消费的应用级授权结果。
// 它刻意复用与平台签发 OIDC token 完全相同的有效授权计算，保证两条下发路径口径一致。
type KeycloakAuthorizationSnapshot struct {
	TenantID, IdentityID, ApplicationID, ApplicationCode string
	PersonID, PrimaryOrgID, RoleConfigHash               string
	OrganizationIDs, Roles, Permissions                  []string
	AuthzRevision                                        uint64
}

// ResolveKeycloakAuthorizationSnapshot 解析要写入某一个 Keycloak Client 的有效授权。
//
// environmentID 必须是本次投影事件对应的环境：Keycloak Client 是环境级资源，只有
// 租户级绑定和该环境的环境级绑定可以进入它。曾经不传环境，等于把同应用所有环境的
// 绑定写进当前 Client——仅授予 dev 的角色会出现在 prod Client 里；平台在线
// authorization-context 仍有环境校验，但无法纠正已经投出的 Keycloak Client Role。
func (s *Service) ResolveKeycloakAuthorizationSnapshot(ctx context.Context, tenantID, identityID, applicationID, environmentID string) (KeycloakAuthorizationSnapshot, error) {
	environmentID = strings.TrimSpace(environmentID)
	if environmentID == "" {
		return KeycloakAuthorizationSnapshot{}, validation("environment_id is required for a Keycloak authorization snapshot")
	}
	application, err := s.findApplicationByID(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(applicationID))
	if err != nil {
		return KeycloakAuthorizationSnapshot{}, err
	}
	access, err := s.getAccessByApplication(ctx, tenantID, identityID, application.ID, application.Code, environmentID, "")
	if errors.Is(err, ErrNotConfigured) {
		roleConfigHash, hashErr := s.loadRoleConfigHash(ctx, tenantID, application.ID)
		if hashErr != nil {
			return KeycloakAuthorizationSnapshot{}, hashErr
		}
		revision, revisionErr := s.loadRevision(ctx, tenantID, application.ID)
		if revisionErr != nil {
			return KeycloakAuthorizationSnapshot{}, revisionErr
		}
		access = emptyAccess(application.Code, "", roleConfigHash, revision)
	} else if err != nil {
		return KeycloakAuthorizationSnapshot{}, err
	}
	roles := make([]string, 0, len(access.Roles))
	permissions := []string{}
	// 被拒绝或冲突的授权必须投影空集合，对账时才会撤销旧的 Keycloak 角色；
	// 管理侧角色视图不是有效授权，不能进入投影。
	if access.AuthorizationState == authorizationGranted {
		for _, role := range access.Roles {
			roles = append(roles, role.Code)
		}
		permissions = append(permissions, access.EffectivePermissions...)
	}
	authorization, err := s.attachOrganizationClaims(ctx, TokenAuthorization{TenantID: tenantID, Roles: sortedUnique(roles), Permissions: permissions, RoleConfigHash: access.RoleConfigHash, AuthzRevision: access.AuthzRevision}, identityID)
	if err != nil {
		return KeycloakAuthorizationSnapshot{}, err
	}
	return KeycloakAuthorizationSnapshot{TenantID: tenantID, IdentityID: identityID, ApplicationID: application.ID, ApplicationCode: application.Code, PersonID: authorization.PersonID, PrimaryOrgID: authorization.PrimaryOrgID, OrganizationIDs: authorization.OrganizationIDs, Roles: authorization.Roles, Permissions: authorization.Permissions, RoleConfigHash: authorization.RoleConfigHash, AuthzRevision: authorization.AuthzRevision}, nil
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

// TableName 绑定 GORM 模型到 platform_application 表。
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

// ResolveOIDCAuthorization 在 OIDC 登录时把 OAuth Client 解析回目标应用与环境，
// 基于当前数据库授权快照计算用户的有效角色、权限与组织 claims；有效授权状态非
// GRANTED 一律返回 ErrAccessDenied。目录哈希与兼容窗口随令牌下发，子系统据此
// 识别目录变化或权限撤销，避免长期复用已失效的权限集合。
func (s *Service) ResolveOIDCAuthorization(ctx context.Context, tenantID, clientID, userID string) (TokenAuthorization, error) {
	client, err := s.findClientApplication(ctx, tenantID, clientID)
	if err != nil {
		return TokenAuthorization{}, err
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
	dataScopes := dataScopesFromRoles(access.Roles)
	compatibility, err := s.loadCatalogCompatibility(ctx, tenantID, client.ApplicationID, access.RoleConfigHash)
	if err != nil {
		return TokenAuthorization{}, err
	}
	return s.attachOrganizationClaims(ctx, TokenAuthorization{
		ApplicationCode:            client.ApplicationCode,
		EnvironmentCode:            client.EnvironmentCode,
		TenantID:                   tenantID,
		Roles:                      sortedUnique(roles),
		Permissions:                append([]string(nil), access.EffectivePermissions...),
		RoleConfigHash:             compatibility.RoleConfigHash,
		CatalogVersion:             compatibility.CatalogVersion,
		CompatibleCatalogVersions:  compatibility.CatalogVersions,
		CompatibleRoleConfigHashes: compatibility.RoleConfigHashes,
		AuthzRevision:              access.AuthzRevision,
		DataScopes:                 dataScopes,
	}, userID)
}

type catalogAuthorizationCompatibility struct {
	CatalogVersion   string
	RoleConfigHash   string
	CatalogVersions  []string
	RoleConfigHashes []string
}

func (s *Service) loadCatalogCompatibility(ctx context.Context, tenantID, applicationID, currentRoleConfigHash string) (catalogAuthorizationCompatibility, error) {
	var metadata catalogMetadataRow
	err := s.db.WithContext(ctx).Table("authz_authorization_catalog").
		Select("catalog_version, claims_role_config_hash, previous_catalog_version, previous_claims_role_config_hash").
		Where("tenant_id = ? AND application_id = ?", tenantID, applicationID).Take(&metadata).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return catalogAuthorizationCompatibility{
			RoleConfigHash:   currentRoleConfigHash,
			RoleConfigHashes: sortedUnique([]string{currentRoleConfigHash}),
		}, nil
	}
	if err != nil {
		return catalogAuthorizationCompatibility{}, fmt.Errorf("load authorization catalog compatibility window: %w", err)
	}
	return catalogCompatibilityFromMetadata(metadata, currentRoleConfigHash), nil
}

func catalogCompatibilityFromMetadata(metadata catalogMetadataRow, fallbackRoleConfigHash string) catalogAuthorizationCompatibility {
	currentRoleConfigHash := strings.TrimSpace(metadata.ClaimsRoleConfigHash)
	if currentRoleConfigHash == "" {
		// 较早的应用目录可能未发布 claims 哈希。让这些应用继续兼容平台推导的
		// 角色映射哈希；已发布哈希的目录则以其为消费方的权威值。
		currentRoleConfigHash = strings.TrimSpace(fallbackRoleConfigHash)
	}
	return catalogAuthorizationCompatibility{
		CatalogVersion:   metadata.CatalogVersion,
		RoleConfigHash:   currentRoleConfigHash,
		CatalogVersions:  sortedUnique([]string{metadata.CatalogVersion, metadata.PreviousCatalogVersion}),
		RoleConfigHashes: sortedUnique([]string{currentRoleConfigHash, metadata.PreviousClaimsRoleConfigHash}),
	}
}

const maxOIDCOrganizationIDs = 100

type organizationClaimRow struct {
	OrganizationID string `gorm:"column:organization_id"`
	IsPrimary      bool   `gorm:"column:is_primary"`
}

// attachOrganizationClaims 只解析用户当前的直接组织成员关系，刻意不展开组织
// 后代节点：下游系统收到的就是平台维护的精确有效成员集合，不做任何放大。
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

// resolvePMSPersonID 只返回在职平台用户上显式配置的租户内人员绑定。
// 绑定缺失是合法的空 claim，绝不允许回退为 userID、employee_no 或任何登录标识符。
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

// GetAccess 返回用户在指定应用上的有效授权与完整可编辑人工分配。用户未配置任何授权时返回携带
// 当前目录哈希与 revision 的空视图（UNAUTHORIZED）而非 ErrNotConfigured，便于
// 管理界面直接编辑而无需区分"无授权"与"查询失败"。
func (s *Service) GetAccess(ctx context.Context, tenantID, userID, applicationCode string) (Access, error) {
	application, err := s.findApplication(ctx, tenantID, applicationCode)
	if err != nil {
		return Access{}, err
	}
	access, err := s.getAccessByApplication(ctx, tenantID, userID, application.ID, application.Code, "", "")
	if err != nil {
		if !errors.Is(err, ErrNotConfigured) {
			return Access{}, err
		}
		roleConfigHash, hashErr := s.loadRoleConfigHash(ctx, tenantID, application.ID)
		if hashErr != nil {
			return Access{}, hashErr
		}
		revision, revisionErr := s.loadRevision(ctx, tenantID, application.ID)
		if revisionErr != nil {
			return Access{}, revisionErr
		}
		access = emptyAccess(application.Code, "", roleConfigHash, revision)
	}
	roleType := applicationRoleType
	if application.Code == PlatformApplicationCode {
		roleType = platformRoleType
	}
	// 可编辑集合与 edit_revision 必须来自同一一致快照：单事务内读取管理视图的
	// 人工绑定行并计算版本（方案第三阶段第 4 节），避免视图与版本混入不同
	// 时刻的数据。有效权限视图不参与该快照约束。
	now := s.clock.Now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		manualRows, rowsErr := loadManagedManualRolesOn(tx, tenantID, application.ID, userID, roleType, now)
		if rowsErr != nil {
			return rowsErr
		}
		_, manualViews, _, _ := resolveAssignedRoles(manualRows, subjectTypeUser)
		access.ManualRoles = filterManualRoles(manualViews)
		revision, revErr := s.computeEditRevision(tx, tenantID, userID, application.ID)
		if revErr != nil {
			return revErr
		}
		access.EditRevision = revision
		return nil
	})
	if err != nil {
		return Access{}, err
	}
	return access, nil
}

// HasApplicationGrant 判断指定主体在目标应用上是否存在有效的应用授权（任何活动角色绑定）。
// 这是跨环境聚合查询；环境敏感入口必须使用 HasApplicationEnvironmentGrant。
func (s *Service) HasApplicationGrant(ctx context.Context, tenantID, userID, applicationCode string) (bool, error) {
	access, err := s.GetAccess(ctx, tenantID, userID, applicationCode)
	if err != nil {
		return false, err
	}
	return access.AuthorizationState == authorizationGranted, nil
}

// HasApplicationEnvironmentGrant 判断用户在某个活动环境上的有效授权。与管理侧
// 聚合查询不同，空环境或未知环境绝不能被解释为对该应用所有环境的访问。
func (s *Service) HasApplicationEnvironmentGrant(ctx context.Context, tenantID, userID, applicationCode, environmentCode string) (bool, error) {
	tenantID, userID = strings.TrimSpace(tenantID), strings.TrimSpace(userID)
	applicationCode, environmentCode = strings.TrimSpace(applicationCode), strings.TrimSpace(environmentCode)
	if tenantID == "" || userID == "" || applicationCode == "" || environmentCode == "" {
		return false, validation("tenant_id, user_id, application_code and environment are required")
	}
	application, err := s.findApplication(ctx, tenantID, applicationCode)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var environment struct{ ID string }
	err = s.db.WithContext(ctx).Table("platform_application_environment").Select("id").
		Where("tenant_id = ? AND application_id = ? AND environment = ? AND status = ?", tenantID, application.ID, environmentCode, activeStatus).
		Take(&environment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load application grant environment: %w", err)
	}
	if strings.TrimSpace(environment.ID) == "" {
		return false, nil
	}
	access, err := s.getAccessByApplication(ctx, tenantID, userID, application.ID, application.Code, environment.ID, environmentCode)
	if errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrAccessDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return access.AuthorizationState == authorizationGranted, nil
}

// UpdateAccess 以集合对账方式替换用户的 MANUAL 直绑：保留未变化绑定的 ID 与审计链，
// 停用本次不再需要的绑定；模板与系统来源不受本次编辑影响。变更与 revision 推进、
// Keycloak 投影入队同事务提交，并记录 HIGH 风险审计（含变更前后差异）。
// 部分更新语义：未提供 roles 时不修改角色；提供 custom_permissions 恒被拒绝。
// 并发条件：ExpectedRevision 非空时在租户锁内比较 edit_revision，陈旧返回
// ErrRevisionConflict；比较与变更处于同一事务，冲突时不产生任何写入、revision
// 推进或成功投影任务。锁序：iam_tenant 行锁 → 目标/目录校验 → 版本比较 → 变更。
func (s *Service) UpdateAccess(ctx context.Context, in UpdateAccessInput, applicationCode string) (Access, error) {
	in.TenantID = strings.TrimSpace(in.TenantID)
	in.UserID = strings.TrimSpace(in.UserID)
	in.OperatorID = strings.TrimSpace(in.OperatorID)
	applicationCode = strings.TrimSpace(applicationCode)
	if in.TenantID == "" || in.UserID == "" || in.OperatorID == "" || applicationCode == "" {
		return Access{}, validation("tenant_id, user_id, operator_id and application_code are required")
	}
	if err := validateCustomPermissionsUpdate(in.CustomPermissionsProvided); err != nil {
		return Access{}, err
	}
	// 事务外的只读查找仅用于审计元数据；写入决策以锁内的 findApplicationOn 为准。
	application, err := s.findApplication(ctx, in.TenantID, applicationCode)
	if err != nil {
		return Access{}, err
	}
	now := s.clock.Now().UTC()
	normalizedRoles, err := normalizeRoleInputs(in.Roles, in.RolesProvided, now)
	if err != nil {
		return Access{}, err
	}
	// 先读取完整生效视图用于审计差异；真正写入只替换 MANUAL 来源的用户直绑，
	// 组织、岗位模板和系统来源不会被一次用户编辑意外覆盖。审计基线在锁外读取，
	// 仅作差异展示，不参与并发判定。
	before, err := s.GetAccess(ctx, in.TenantID, in.UserID, applicationCode)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return Access{}, err
	}

	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 方案锁序第一环：iam_tenant 行锁串行化同租户的整组保存与撤销，第一轮
		// 不新增版本表。锁内完成全部校验与版本比较，消除"校验后状态被改"窗口。
		if err := lockTenantRow(tx, in.TenantID); err != nil {
			return err
		}
		application, err := findApplicationOn(tx, in.TenantID, applicationCode)
		if err != nil {
			return err
		}
		if err := ensureUserOn(tx, in.TenantID, in.UserID); err != nil {
			return err
		}
		resolved, err := resolveRoleBindingsOn(tx, in.TenantID, application.ID, normalizedRoles)
		if err != nil {
			return err
		}
		if err := validateDirectRoleLimitOn(tx, in.TenantID, application.ID, resolved); err != nil {
			return err
		}
		// 版本比较在全部校验之后、任何写入之前：失败不得修改绑定、推进授权
		// 版本或产生成功投影任务。空条件（兼容期旧调用方）跳过比较。
		if _, err := s.ensureExpectedRevision(tx, in.TenantID, in.UserID, application.ID, in.ExpectedRevision, in.StrictRevisionRequired); err != nil {
			return err
		}
		if in.RolesProvided {
			roleChanged, err := s.replaceManualUserRoleBindings(tx, in.TenantID, application.ID, in.UserID, in.OperatorID, resolved, now)
			if err != nil {
				return err
			}
			changed = changed || roleChanged
		}
		if changed {
			// 授权数据和 revision 必须原子提交，子系统才能可靠识别旧授权快照。
			if err := bumpRevision(tx, in.TenantID, application.ID, now, "user application authorization changed"); err != nil {
				return err
			}
			return s.enqueueKeycloakAuthorizationOutbox(tx, in.TenantID, in.UserID, application.ID, now, "AUTHORIZATION_CHANGED")
		}
		return nil
	})
	if err != nil {
		return Access{}, err
	}
	// 提交事实已成立：读回失败绝不能把已保存的变更误报为保存失败，也不得跳过
	// 成功审计。审计记录 SUCCESS 并标注读取失败原因；Changes 在视图不可用时省略。
	access, readErr := s.GetAccess(ctx, in.TenantID, in.UserID, applicationCode)
	if readErr != nil {
		metadata := map[string]any{"roles_provided": in.RolesProvided, "custom_permissions_provided": in.CustomPermissionsProvided, "changed": changed, "grant_origin": grantOriginManual, "access_view_error": truncateForAudit(readErr)}
		s.recordAudit(ctx, AuditEvent{
			TenantID: in.TenantID, ApplicationID: application.ID, ApplicationCode: applicationCode,
			OperatorID: in.OperatorID, OperatorName: in.OperatorName, SubjectID: in.UserID, Action: "authorization.application_access.updated",
			ResourceType: "application_access", ResourceID: in.UserID, Result: "SUCCESS", RiskLevel: "HIGH",
			Summary: "应用用户授权已更新", OccurredAt: now,
			Metadata: metadata,
		})
		return Access{}, fmt.Errorf("%w: %v", ErrAccessReadback, readErr)
	}
	s.recordAudit(ctx, AuditEvent{
		TenantID: in.TenantID, ApplicationID: application.ID, ApplicationCode: applicationCode,
		OperatorID: in.OperatorID, OperatorName: in.OperatorName, SubjectID: in.UserID, Action: "authorization.application_access.updated",
		ResourceType: "application_access", ResourceID: in.UserID, Result: "SUCCESS", RiskLevel: "HIGH",
		Summary: "应用用户授权已更新", OccurredAt: now,
		Metadata: map[string]any{"roles_provided": in.RolesProvided, "custom_permissions_provided": in.CustomPermissionsProvided, "changed": changed, "grant_origin": grantOriginManual, "authorization_revision": access.AuthzRevision},
		Changes:  accessAuditChanges(before, access),
	})
	return access, nil
}

// truncateForAudit 把读回错误压到审计可承载的长度，避免基础设施长错误污染审计。
func truncateForAudit(err error) string {
	message := err.Error()
	if len(message) > 200 {
		return message[:200]
	}
	return message
}

// DeleteAccess 逻辑撤销用户在指定应用上的全部人工授权：停用 MANUAL 直绑并清理
// 历史自定义权限，不触碰岗位模板等可追溯来源；若用户仍有继承角色，有效视图如实
// 保留。无变更时同样返回成功，仍记录审计以便追溯操作者。
// 并发条件：ExpectedRevision 非空时在租户锁内比较（与 UpdateAccess 同一锁序），
// 防止旧页面"撤销全部"误撤销期间新增的授权。
func (s *Service) DeleteAccess(ctx context.Context, in DeleteAccessInput, applicationCode string) error {
	in.TenantID = strings.TrimSpace(in.TenantID)
	in.UserID = strings.TrimSpace(in.UserID)
	in.OperatorID = strings.TrimSpace(in.OperatorID)
	applicationCode = strings.TrimSpace(applicationCode)
	if in.TenantID == "" || in.UserID == "" || in.OperatorID == "" || applicationCode == "" {
		return validation("tenant_id, user_id, operator_id and application_code are required")
	}
	// 事务外的只读查找仅用于审计元数据；写入决策以锁内的 findApplicationOn 为准。
	application, err := s.findApplication(ctx, in.TenantID, applicationCode)
	if err != nil {
		return err
	}
	before, err := s.GetAccess(ctx, in.TenantID, in.UserID, applicationCode)
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return err
	}
	now := s.clock.Now().UTC()
	changed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 与 UpdateAccess 相同的租户锁序；两个入口修改同一份 MANUAL 集合，必须
		// 受同一并发约束（方案 R11）。
		if err := lockTenantRow(tx, in.TenantID); err != nil {
			return err
		}
		lockedApplication, err := findApplicationOn(tx, in.TenantID, applicationCode)
		if err != nil {
			return err
		}
		// 版本比较在停用操作之前：旧页面基于陈旧快照发起的"撤销全部"在此被拒绝，
		// 不会误删期间新增授权，也不产生 revision 推进或投影任务。
		if _, err := s.ensureExpectedRevision(tx, in.TenantID, in.UserID, lockedApplication.ID, in.ExpectedRevision, in.StrictRevisionRequired); err != nil {
			return err
		}
		// 删除应用授权是逻辑撤销：仅停用人工直绑并清理历史自定义权限，不触碰岗位
		// 模板等可追溯来源；若用户仍有继承角色，有效视图仍会如实保留。
		directClause, directArgs := manualDirectApplicationRoleBindingFilter(in.TenantID, lockedApplication.ID, in.UserID)
		applicationRoleIDs := tx.Table("authz_role").Select("id").Where("tenant_id = ? AND application_id = ? AND role_type = ?", in.TenantID, lockedApplication.ID, applicationRoleType)
		result := tx.Table("authz_role_binding AS rb").Where(directClause, directArgs...).Where("rb.role_id IN (?)", applicationRoleIDs).Where("rb.status = ?", activeStatus).Updates(map[string]any{"status": disabledStatus, "version": gorm.Expr("version + 1"), "updated_at": now, "updated_by": in.OperatorID})
		if result.Error != nil {
			return fmt.Errorf("revoke application role bindings: %w", result.Error)
		}
		if result.RowsAffected > 0 {
			changed = true
		}
		permissionResult := tx.Table("authz_user_permission").Where("tenant_id = ? AND application_id = ? AND user_id = ?", in.TenantID, lockedApplication.ID, in.UserID).Delete(nil)
		if permissionResult.Error != nil {
			return fmt.Errorf("remove legacy user permissions: %w", permissionResult.Error)
		}
		if permissionResult.RowsAffected > 0 {
			changed = true
		}
		if changed {
			if err := bumpRevision(tx, in.TenantID, lockedApplication.ID, now, "user application access revoked"); err != nil {
				return err
			}
			return s.enqueueKeycloakAuthorizationOutbox(tx, in.TenantID, in.UserID, lockedApplication.ID, now, "AUTHORIZATION_CHANGED")
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 与 UpdateAccess 相同的读回语义：撤销已提交后读取失败，成功审计不得跳过。
	after, readErr := s.GetAccess(ctx, in.TenantID, in.UserID, application.Code)
	if readErr != nil && !errors.Is(readErr, ErrNotConfigured) {
		s.recordAudit(ctx, AuditEvent{
			TenantID: in.TenantID, ApplicationID: application.ID, ApplicationCode: application.Code,
			OperatorID: in.OperatorID, OperatorName: in.OperatorName, SubjectID: in.UserID, Action: "authorization.application_access.deleted",
			ResourceType: "application_access", ResourceID: in.UserID, Result: "SUCCESS", RiskLevel: "HIGH",
			Summary: "应用用户授权已删除", OccurredAt: now,
			Metadata: map[string]any{"changed": changed, "grant_origin": grantOriginManual, "access_view_error": truncateForAudit(readErr)},
		})
		return fmt.Errorf("%w: %v", ErrAccessReadback, readErr)
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

// enqueueKeycloakAuthorizationOutbox 刻意与直接的用户授权变更放在同一事务内调用。
// 因此已提交的角色变更绝不会被悄悄漏出 Keycloak 投影队列。
func (s *Service) enqueueKeycloakAuthorizationOutbox(tx *gorm.DB, tenantID, userID, applicationID string, now time.Time, eventType string) error {
	var environmentIDs []string
	if err := tx.Table("keycloak_application_client_mapping").
		Where("tenant_id=? AND application_id=? AND status=?", tenantID, applicationID, "SYNCED").
		Order("environment_id ASC").Pluck("environment_id", &environmentIDs).Error; err != nil {
		return fmt.Errorf("load Keycloak Client mapping targets: %w", err)
	}
	for _, environmentID := range sortedUnique(environmentIDs) {
		id, err := s.ids.New(now)
		if err != nil {
			return fmt.Errorf("generate Keycloak authorization outbox ID: %w", err)
		}
		if err := tx.Table("keycloak_authorization_outbox").Create(map[string]any{
			"id": id, "tenant_id": tenantID, "identity_id": userID, "application_id": applicationID, "environment_id": environmentID,
			"event_type": eventType, "authorization_revision": 0, "status": "PENDING", "available_at": now,
			"attempts": 0, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("enqueue Keycloak authorization outbox: %w", err)
		}
	}
	return nil
}

// enqueueSubjectUsers 把一次历史遗留的组织/岗位绑定变更扇出到当前有效授权包含该
// 主体的所有用户。它与撤销操作共用同一事务，保持 worker 原有的 PENDING 重试语义。
func (s *Service) enqueueSubjectUsers(tx *gorm.DB, tenantID, subjectType, subjectID, applicationID string, now time.Time) error {
	var userIDs []string
	query := tx.Table("iam_membership AS membership").
		Distinct("membership.user_id").
		Joins("JOIN iam_user AS user ON user.tenant_id=membership.tenant_id AND user.id=membership.user_id AND user.status=?", activeStatus).
		Joins("JOIN iam_org_unit AS organization ON organization.tenant_id=membership.tenant_id AND organization.id=membership.org_unit_id AND organization.status=?", activeStatus).
		Joins("JOIN iam_position AS position ON position.tenant_id=membership.tenant_id AND position.id=membership.position_id AND position.org_unit_id=membership.org_unit_id AND position.status=?", activeStatus).
		Where("membership.tenant_id=? AND membership.status=? AND membership.inherit_authorization=?", tenantID, activeStatus, true).
		Where("(membership.valid_from IS NULL OR membership.valid_from <= ?) AND (membership.valid_until IS NULL OR membership.valid_until > ?)", now, now)
	if subjectType == subjectTypeOrgUnit {
		query = query.Where("membership.org_unit_id=?", subjectID)
	} else {
		query = query.Where("membership.position_id=?", subjectID)
	}
	if err := query.Pluck("membership.user_id", &userIDs).Error; err != nil {
		return fmt.Errorf("load users affected by application subject access: %w", err)
	}
	for _, userID := range userIDs {
		if err := s.enqueueKeycloakAuthorizationOutbox(tx, tenantID, userID, applicationID, now, "AUTHORIZATION_CHANGED"); err != nil {
			return err
		}
	}
	return nil
}

// GetSubjectAccess 返回直接分配给组织单元或岗位的绑定。与 GetAccess 不同，
// 它从不展开成员关系：被查询的主体本身就是被管理的授权主体。
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

// UpdateSubjectAccess 作为历史组织/岗位写端点调用方的兼容边界保留。标准人员授权
// 现在完全由岗位授权模板物化，因此本方法恒拒绝人工写入。存量 MANUAL 绑定仍可经
// GetSubjectAccess 查看，并可通过 DeleteSubjectAccess 清理。
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

// DeleteSubjectAccess 仅停用所选组织/岗位自身的直接绑定，不影响其成员用户。
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
			if err := bumpRevision(tx, in.TenantID, application.ID, now, "application subject access revoked"); err != nil {
				return err
			}
			return s.enqueueSubjectUsers(tx, in.TenantID, subjectType, in.SubjectID, application.ID, now)
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

// IsApplicationOwner 报告认证用户是否为该应用的属主。应用属主可以同步自己应用的
// 授权目录，但不会因此获得任何平台级管理权限。
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

// validateSubjectAccessWritePolicy 防止通用应用访问端点演化为第二套标准授权机制。
// 早于岗位模板的组织/岗位绑定属迁移残留：管理员可以查看和撤销，但只有 positiongrant
// 物化器可以创建 TEMPLATE 来源的 POSITION 绑定。
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
	return resolveRoleBindingsOn(s.db.WithContext(ctx), tenantID, applicationID, roles)
}

// resolveRoleBindingsOn 是 resolveRoleBindings 的事务句柄变体。
func resolveRoleBindingsOn(database *gorm.DB, tenantID, applicationID string, roles []RoleInput) ([]resolvedBinding, error) {
	// 外部只提交稳定角色编码和作用域；此处解析为当前租户、当前应用的活动目录角色，
	// 禁止使用数据库 ID 绕过目录所有权、角色状态或受保护角色策略。
	resolved := make([]resolvedBinding, 0, len(roles))
	for _, role := range roles {
		var roleRecord roleRow
		// 应用访问是应用自有的边界。平台控制面角色只能由通用且受保护的角色绑定
		// API 管理。在此保留 role_type 谓词，可防止 platform-super-admin 授予
		// 借应用授权更新被夹带写入。
		if err := database.Table("authz_role").Where("tenant_id = ? AND application_id = ? AND status = ? AND code = ? AND role_type = ?", tenantID, applicationID, activeStatus, role.RoleCode, applicationRoleType).Take(&roleRecord).Error; err != nil {
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
			if err := database.Table("platform_application_environment").Select("id").Where("tenant_id = ? AND application_id = ? AND environment = ? AND status = ?", tenantID, applicationID, role.EnvironmentCode, activeStatus).Take(&environment).Error; err != nil {
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

// validateDirectRoleLimit 拒绝目标应用永远无法授权的角色集合，防止管理写入
// 持久化一条立即冲突的直接分配。跨来源冲突依赖有效成员关系，仍留待授权
// 解析时 fail-closed。
func (s *Service) validateDirectRoleLimit(ctx context.Context, tenantID, applicationID string, resolved []resolvedBinding) error {
	return validateDirectRoleLimitOn(s.db.WithContext(ctx), tenantID, applicationID, resolved)
}

// validateDirectRoleLimitOn 是 validateDirectRoleLimit 的事务句柄变体。
func validateDirectRoleLimitOn(database *gorm.DB, tenantID, applicationID string, resolved []resolvedBinding) error {
	policy, err := resolveApplicationAuthorizationPolicyOn(database, tenantID, applicationID)
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

// replaceManualUserRoleBindings 只负责 MANUAL USER 授予。把 USER 固定在本持久化
// 辅助函数内部，使未来的调用方不可能误将其用于 ACCOUNT、ORG_UNIT 或 POSITION
// 绑定；TEMPLATE 与 SYSTEM 行由查询谓词排除在外。
func (s *Service) replaceManualUserRoleBindings(tx *gorm.DB, tenantID, applicationID, userID, operatorID string, resolved []resolvedBinding, now time.Time) (bool, error) {
	// 采用集合对账而非“全删全建”：保留未变化绑定的 ID 和审计链，恢复历史同源
	// 绑定时递增版本；只将本次不再需要的 MANUAL 绑定置为 DISABLED。
	var existing []bindingRow
	directClause, directArgs := manualDirectApplicationRoleBindingFilter(tenantID, applicationID, userID)
	if err := tx.Table("authz_role_binding AS rb").Clauses(clause.Locking{Strength: "UPDATE"}).Select("rb.id, rb.role_id, rb.scope_type, rb.scope_id, rb.valid_from, rb.valid_until, rb.status, rb.version").Joins("JOIN authz_role AS r ON r.id = rb.role_id AND r.tenant_id = rb.tenant_id AND r.application_id = rb.application_id").Where(directClause, directArgs...).Where("r.role_type = ?", applicationRoleType).Find(&existing).Error; err != nil {
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
	// 在本事务任何写入之前验证全部新增/扩大项，不能先给操作者自己授权再据此
	// 通过后续角色的委派检查。不变、缩短与撤销仍允许，避免失去业务权限后无法收权。
	for _, item := range resolved {
		contained := false
		for _, old := range existing {
			scopeCovered := (old.ScopeType == item.scopeType && old.ScopeID == item.scopeID) ||
				(old.ScopeType == scopeTypeTenant && old.ScopeID == "" && item.scopeType == scopeTypeEnvironment)
			if old.RoleID == item.roleID && old.Status == activeStatus && scopeCovered &&
				(old.ValidFrom == nil || (item.role.ValidFrom != nil && !item.role.ValidFrom.Before(*old.ValidFrom))) &&
				(old.ValidUntil == nil || (item.role.ValidUntil != nil && !item.role.ValidUntil.After(*old.ValidUntil))) {
				contained = true
				break
			}
		}
		if contained {
			continue
		}
		if err := delegation.VerifyRolePermissions(tx.Statement.Context, tx, tenantID, operatorID, applicationID, item.roleID); err != nil {
			return false, fmt.Errorf("verify application role delegation: %w", err)
		}
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
	return findApplicationOn(s.db.WithContext(ctx), tenantID, code)
}

// findApplicationOn 是 findApplication 的数据库句柄参数化变体：事务内调用方
// 传入 tx，保证目标校验与后续变更处于同一锁与快照边界。
func findApplicationOn(database *gorm.DB, tenantID, code string) (applicationRow, error) {
	var row applicationRow
	err := database.Where("tenant_id = ? AND code = ? AND status = ?", strings.TrimSpace(tenantID), strings.TrimSpace(code), activeStatus).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return applicationRow{}, ErrNotFound
	}
	if err != nil {
		return applicationRow{}, fmt.Errorf("load application: %w", err)
	}
	return row, nil
}

func (s *Service) ensureUser(ctx context.Context, tenantID, userID string) error {
	return ensureUserOn(s.db.WithContext(ctx), tenantID, userID)
}

// ensureUserOn 是 ensureUser 的事务句柄变体。
func ensureUserOn(database *gorm.DB, tenantID, userID string) error {
	var count int64
	if err := database.Table("iam_user").Where("tenant_id = ? AND id = ?", tenantID, userID).Count(&count).Error; err != nil {
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
	roleType := applicationRoleType
	if applicationCode == PlatformApplicationCode {
		roleType = platformRoleType
	}
	isSuperAdmin := false
	var checkErr error
	roles, err := s.loadGenericRoles(ctx, tenantID, applicationID, userID, environmentID, now, roleType)
	if err != nil {
		return Access{}, err
	}
	// 平台超级管理员是租户级控制面身份。它不应要求管理员逐个业务应用
	// 维护一组角色，否则新接入应用或新增角色会出现“超管看得到系统但没有权限”
	// 的窗口。这里仅在授权解析时生成只读的有效角色视图，不写入业务应用绑定，
	// 也不改变普通用户、岗位和组织的授权模型。
	if applicationCode != PlatformApplicationCode {
		isSuperAdmin, checkErr = s.isPlatformSuperAdmin(ctx, tenantID, userID, now)
		if checkErr != nil {
			return Access{}, checkErr
		}
		if isSuperAdmin {
			roles, err = s.expandSuperAdminApplicationRoles(ctx, tenantID, applicationID, applicationCode, roles)
			if err != nil {
				return Access{}, err
			}
		}
	}
	roleIDs, roleViews, directRoles, inheritedRoles := resolveAssignedRoles(roles, subjectTypeUser)
	state, conflicts, permissionRoleIDs, err := s.applicationRolePolicy(ctx, tenantID, applicationID, roles, roleIDs)
	if err != nil {
		return Access{}, err
	}
	// 超管的有效角色集合不受目标应用 max_effective_roles 限制；该限制用于
	// 普通业务账号的角色冲突防护，不能把控制面超管变成未授权状态。
	if isSuperAdmin {
		state = authorizationGranted
		conflicts = []string{}
		permissionRoleIDs = roleIDs
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

// applicationRolePolicy 应用目标应用目录声明的最大有效角色数限制。
// 该规则绝不从应用编码、角色名称或权限编码推导。
func (s *Service) applicationRolePolicy(ctx context.Context, tenantID, applicationID string, rows []assignedRoleRow, roleIDs []string) (string, []string, []string, error) {
	policy, err := s.ResolveApplicationAuthorizationPolicy(ctx, tenantID, applicationID)
	if err != nil {
		return authorizationUnauthorized, nil, nil, err
	}
	state, conflicts, permittedRoleIDs := applyApplicationRolePolicy(policy, rows, roleIDs)
	return state, conflicts, permittedRoleIDs, nil
}

// applyApplicationRolePolicy 刻意保持无副作用，使角色数上限语义可独立测试。
// rows 仅用于生成稳定、管理员可读的冲突列表；有效角色数以角色 ID 为准，
// 因为它们才是实际被授予的角色记录。
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

// validateCustomPermissionsUpdate 强制平台边界：应用发布角色-权限目录，
// 平台只负责授予角色。历史 authz_user_permission 行永不参与授权判定。
func validateCustomPermissionsUpdate(customPermissionsProvided bool) error {
	if customPermissionsProvided {
		return validation("custom_permissions are not supported; assign an application role instead")
	}
	return nil
}

// effectivePermissionsForApplication 从已同步的应用角色目录推导全部有效权限。
// 冲突状态一律 fail-closed；未配置授权的用户不可能经历史直绑获得任何权限。
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
		// 来源标记是展示绑定的一部分。人工绑定与模板绑定可以合法地从同一岗位
		// 授予同一角色，必须保持各自可追溯；有效角色数在下文按角色 ID 去重。
		bindingKey := row.RoleID + "\x00" + row.SubjectType + "\x00" + row.SubjectID + "\x00" + row.ScopeType + "\x00" + row.ScopeID + "\x00" + normalizedGrantOrigin(row.GrantOrigin) + "\x00" + row.OriginID + "\x00" + row.OriginItemID
		if _, exists := seenBindings[bindingKey]; exists {
			continue
		}
		seenBindings[bindingKey] = struct{}{}
		sourceKind := sourceKindForRole(row, row.SubjectType == directSubjectType)
		view := RoleView{
			Code: row.Code, Name: row.Name, ScopeType: externalScopeType(row.ScopeType),
			ScopeID:         row.ScopeID,
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
		// 管理侧读取返回全部环境级绑定，让管理员看到完整分配集合；
		// OIDC 链路则必定携带环境 ID。
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

// directApplicationRoleBindingFilter 仅为需要"仅按主体过滤"谓词的调用方/测试保留。
// 变更代码一律使用 manual 变体，使模板/系统绑定处于写保护之下。
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

// loadInheritedPlatformRoles 经与岗位模板相同的显式继承映射，把用户对平台角色的
// 直接 USER 绑定解析到目标应用角色。结果只读：目标子系统绑定不会被复制给用户，
// 来源标记仍是源平台角色。
func (s *Service) loadInheritedPlatformRoles(ctx context.Context, tenantID, targetApplicationID, userID, environmentID string, now time.Time) ([]assignedRoleRow, error) {
	type inheritedRow struct {
		RoleID, Code, Name, ScopeType, ScopeID, EnvironmentCode string
		ValidFrom, ValidUntil                                   *time.Time
		SourceRoleID, SourceRoleName, MappingID                 string
	}
	var rows []inheritedRow
	query := s.db.WithContext(ctx).Table("authz_role_inheritance_mapping AS mapping").Select(`
		target_role.id AS role_id, target_role.code, target_role.name,
		mapping.scope_type, mapping.scope_id, COALESCE(environment.environment, '') AS environment_code,
		mapping.valid_from, mapping.valid_until,
		source_role.id AS source_role_id, source_role.name AS source_role_name, mapping.id AS mapping_id`).
		Joins("JOIN authz_role AS source_role ON source_role.tenant_id=mapping.tenant_id AND source_role.id=mapping.source_role_id AND source_role.role_type=? AND source_role.status=?", platformRoleType, activeStatus).
		Joins("JOIN platform_application AS source_application ON source_application.tenant_id=source_role.tenant_id AND source_application.id=source_role.application_id AND source_application.code=? AND source_application.status=?", PlatformApplicationCode, activeStatus).
		Joins("JOIN authz_role_binding AS source_binding ON source_binding.tenant_id=source_role.tenant_id AND source_binding.application_id=source_role.application_id AND source_binding.role_id=source_role.id AND source_binding.subject_type=? AND source_binding.subject_id=? AND source_binding.scope_type=? AND source_binding.scope_id='' AND source_binding.status=?", subjectTypeUser, userID, scopeTypeTenant, activeStatus).
		Joins("JOIN authz_role AS target_role ON target_role.tenant_id=mapping.tenant_id AND target_role.application_id=mapping.target_application_id AND target_role.id=mapping.target_role_id AND target_role.role_type=? AND target_role.status=?", applicationRoleType, activeStatus).
		Joins("LEFT JOIN platform_application_environment AS environment ON environment.tenant_id=mapping.tenant_id AND environment.application_id=mapping.target_application_id AND environment.id=mapping.scope_id").
		Where("mapping.tenant_id=? AND mapping.target_application_id=? AND mapping.status=?", tenantID, targetApplicationID, activeStatus).
		Where("(mapping.valid_from IS NULL OR mapping.valid_from<=?) AND (mapping.valid_until IS NULL OR mapping.valid_until>?)", now, now).
		Where("(source_binding.valid_from IS NULL OR source_binding.valid_from<=?) AND (source_binding.valid_until IS NULL OR source_binding.valid_until>?)", now, now)
	if strings.TrimSpace(environmentID) == "" {
		query = query.Where("(mapping.scope_type=? AND mapping.scope_id='' OR mapping.scope_type=?)", scopeTypeTenant, scopeTypeEnvironment)
	} else {
		query = query.Where("(mapping.scope_type=? AND mapping.scope_id='' OR mapping.scope_type=? AND mapping.scope_id=?)", scopeTypeTenant, scopeTypeEnvironment, environmentID)
	}
	if err := query.Order("target_role.code ASC, mapping.scope_type ASC, mapping.scope_id ASC, source_role.code ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load inherited platform role mappings: %w", err)
	}
	result := make([]assignedRoleRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, assignedRoleRow{
			RoleID: row.RoleID, Code: row.Code, Name: row.Name,
			ScopeType: row.ScopeType, ScopeID: row.ScopeID, EnvironmentCode: row.EnvironmentCode,
			ValidFrom: row.ValidFrom, ValidUntil: row.ValidUntil,
			SubjectType: "PLATFORM_ROLE", SubjectID: row.SourceRoleID, SourceName: row.SourceRoleName,
			GrantOrigin: grantOriginInherited, OriginItemID: row.MappingID,
		})
	}
	return result, nil
}

// loadManagedManualRoles 是管理集合，不参与有效角色/权限计算。未来分配必须可见，
// 否则管理界面以有效视图提交完整集合时，会把尚未生效的分配置为 DISABLED。
func (s *Service) loadManagedManualRoles(ctx context.Context, tenantID, applicationID, userID, roleType string, now time.Time) ([]assignedRoleRow, error) {
	return loadManagedManualRolesOn(s.db.WithContext(ctx), tenantID, applicationID, userID, roleType, now)
}

// loadManagedManualRolesOn 是 loadManagedManualRoles 的事务句柄变体：管理端
// GET 在事务内调用它，保证可编辑集合与 edit_revision 来自同一快照。
func loadManagedManualRolesOn(database *gorm.DB, tenantID, applicationID, userID, roleType string, now time.Time) ([]assignedRoleRow, error) {
	var rows []assignedRoleRow
	filter, args := manualDirectApplicationRoleBindingFilter(tenantID, applicationID, userID)
	scopeFilter, scopeArgs := applicationAccessScopeFilter("")
	err := database.Table("authz_role_binding AS rb").
		Select("r.id AS role_id, r.code, r.name, rb.scope_type, rb.scope_id, e.environment AS environment_code, rb.valid_from, rb.valid_until, rb.subject_type, rb.subject_id, rb.grant_origin, rb.origin_id, rb.origin_item_id, COALESCE(u.display_name, '') AS source_name").
		Joins("JOIN authz_role AS r ON r.id=rb.role_id AND r.tenant_id=rb.tenant_id AND r.application_id=rb.application_id AND r.status=? AND r.role_type=?", activeStatus, roleType).
		Joins("LEFT JOIN platform_application_environment AS e ON e.id=rb.scope_id AND e.application_id=rb.application_id AND e.tenant_id=rb.tenant_id").
		Joins("LEFT JOIN iam_user AS u ON u.id=rb.subject_id AND u.tenant_id=rb.tenant_id").
		Where(filter, args...).Where("rb.status=?", activeStatus).
		Where("rb.valid_until IS NULL OR rb.valid_until > ?", now).
		Where(scopeFilter, scopeArgs...).Order("r.code ASC, rb.scope_type ASC, rb.scope_id ASC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load editable manual application assignments: %w", err)
	}
	return rows, nil
}

func (s *Service) loadGenericRoles(ctx context.Context, tenantID, applicationID, userID, environmentID string, now time.Time, roleType string) ([]assignedRoleRow, error) {
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
		Where("rb.tenant_id = ? AND rb.application_id = ? AND rb.status = ? AND r.role_type = ?", tenantID, applicationID, activeStatus, roleType).
		Where(subjectClause, subjectArgs...).
		Where("(rb.valid_from IS NULL OR rb.valid_from <= ?) AND (rb.valid_until IS NULL OR rb.valid_until > ?)", now, now).
		Where(scopeClause, scopeArgs...)
	if err := query.Order("CASE rb.subject_type WHEN 'USER' THEN 0 WHEN 'ORG_UNIT' THEN 1 WHEN 'POSITION' THEN 2 ELSE 3 END ASC, r.code ASC, rb.scope_type ASC, rb.scope_id ASC, rb.subject_id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load application role bindings: %w", err)
	}
	return rows, nil
}

// isPlatformSuperAdmin 刻意直接检查受保护的平台角色。若改为查找显示名或名为
// "admin" 的应用角色，跨应用提权将取决于可变的目录数据。
func (s *Service) isPlatformSuperAdmin(ctx context.Context, tenantID, userID string, now time.Time) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Table("authz_role_binding AS binding").
		Joins("JOIN authz_role AS role ON role.id = binding.role_id AND role.tenant_id = binding.tenant_id AND role.status = ? AND role.role_type = ?", activeStatus, platformRoleType).
		Joins("JOIN platform_application AS application ON application.id = role.application_id AND application.tenant_id = role.tenant_id AND application.code = ? AND application.status = ?", PlatformApplicationCode, activeStatus).
		Where("binding.tenant_id = ? AND binding.subject_type = ? AND binding.subject_id = ? AND binding.scope_type = ? AND binding.scope_id = '' AND binding.status = ? AND role.code = ?", tenantID, subjectTypeUser, userID, scopeTypeTenant, activeStatus, BootstrapSuperAdminRoleCode).
		Where("(binding.valid_from IS NULL OR binding.valid_from <= ?) AND (binding.valid_until IS NULL OR binding.valid_until > ?)", now, now).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("check platform super administrator role: %w", err)
	}
	return count > 0, nil
}

// expandSuperAdminApplicationRoles 为每个活动应用角色构建内存态有效角色视图，
// 刻意不持久化任何绑定：目录更新即刻生效，普通用户授予保持可审计且不变。
func (s *Service) expandSuperAdminApplicationRoles(ctx context.Context, tenantID, applicationID, applicationCode string, existing []assignedRoleRow) ([]assignedRoleRow, error) {
	var rows []assignedRoleRow
	err := s.db.WithContext(ctx).Table("authz_role").
		Select("id AS role_id, code, name").
		Where("tenant_id = ? AND application_id = ? AND role_type = ? AND status = ?", tenantID, applicationID, applicationRoleType, activeStatus).
		Order("code ASC, id ASC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load all application roles for platform super administrator: %w", err)
	}
	// 客户门户的浏览器会话要求恰好一个角色。内部平台超管无论数据库是否残留客户
	// 角色，或未来目录是否新增其他业务角色，都只能投影为 portal_super_admin。
	// 原始绑定仍留在数据库供审计和受控清理，不参与本次只读有效权限计算。
	filteredExisting := existing[:0]
	for _, row := range existing {
		if !includeRoleInSuperAdminProjection(applicationCode, row.Code) {
			continue
		}
		filteredExisting = append(filteredExisting, row)
	}
	existing = filteredExisting

	seen := make(map[string]struct{}, len(existing)+len(rows))
	for _, row := range existing {
		if row.RoleID != "" {
			seen[row.RoleID] = struct{}{}
		}
	}
	for index := range rows {
		if !includeRoleInSuperAdminProjection(applicationCode, rows[index].Code) {
			continue
		}
		if _, ok := seen[rows[index].RoleID]; ok {
			continue
		}
		rows[index].ScopeType = scopeTypeTenant
		rows[index].ScopeID = ""
		rows[index].SubjectType = "PLATFORM_ROLE"
		rows[index].SubjectID = BootstrapSuperAdminRoleCode
		rows[index].SourceName = "平台超级管理员（全应用授权）"
		rows[index].GrantOrigin = grantOriginInherited
		existing = append(existing, rows[index])
		seen[rows[index].RoleID] = struct{}{}
	}
	return existing, nil
}

func includeRoleInSuperAdminProjection(applicationCode, roleCode string) bool {
	if applicationCode == "customer_portal" {
		return roleCode == "portal_super_admin"
	}
	return true
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
	// 子系统可为其自身的 Claims 兼容契约发布不透明配置哈希。基础平台刻意不检查、
	// 也不从业务角色/权限语义重算该值。
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

	// 未发布应用级 Claims 哈希的目录保留一个通用的确定性镜像哈希。
	// 这是兜底值，不是按应用定制的规则。
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

// roleConfigHash 是应用未发布显式 Claims 兼容哈希时随令牌下发的通用目录版本。
// 它只对已同步的角色-权限映射做哈希：角色名、来源类型与内置标记属于展示/
// 来源元数据，不参与授权策略。
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

// dataScopesFromRoles 把已解析角色视图收敛为去重的数据范围。同一角色经由不同
// 来源（直绑、岗位模板、平台角色继承）对同一主体生效时，RoleView 按来源保留
// 多条以便管理界面追溯，但 authorization-context 只描述有效访问边界，必须按
// (role_code, scope_type, scope_id, environment_code) 去重，否则严格校验的
// 子系统会因重复数据范围拒绝登录。首见顺序保持稳定。
func dataScopesFromRoles(roles []RoleView) []tokenissuer.DataScope {
	dataScopes := make([]tokenissuer.DataScope, 0, len(roles))
	seen := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		key := strings.Join([]string{role.Code, role.ScopeType, role.ScopeID, role.EnvironmentCode}, "\x00")
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		dataScopes = append(dataScopes, tokenissuer.DataScope{RoleCode: role.Code, ScopeType: role.ScopeType, ScopeID: role.ScopeID, EnvironmentCode: role.EnvironmentCode})
	}
	return dataScopes
}

func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }
