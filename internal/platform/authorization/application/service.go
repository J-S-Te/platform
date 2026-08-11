// Package application coordinates RBAC management and authorization decisions.
package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/domain"
)

var (
	// ErrNotFound identifies a tenant-scoped authorization aggregate that does not exist.
	ErrNotFound = errors.New("authorization resource not found")
	// ErrConflict identifies a uniqueness or lifecycle conflict.
	ErrConflict = errors.New("authorization resource conflict")
	// ErrVersionConflict identifies stale optimistic-lock input.
	ErrVersionConflict = errors.New("authorization version conflict")
	// ErrForbidden identifies an authorization-management operation the current operator may not perform.
	ErrForbidden = errors.New("authorization operation forbidden")
	// ErrValidation signals a caller-correctable request error.
	ErrValidation = errors.New("authorization validation failed")
)

// IdentifierGenerator creates stable public identifiers.
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock makes timestamps testable.
type Clock interface{ Now() time.Time }

// SystemClock reads the current wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// PageRequest bounds RBAC list queries.
type PageRequest struct {
	Page, PageSize  int
	Keyword, Status string
}

// PageResult contains a page of RBAC aggregates.
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// Repository persists authorization aggregates and evaluates policy state.
type Repository interface {
	ListResources(context.Context, string, PageRequest) (PageResult[domain.Resource], error)
	CreateResource(context.Context, string, string, domain.Resource) (domain.Resource, error)
	ListPermissions(context.Context, string, PageRequest) (PageResult[domain.Permission], error)
	CreatePermission(context.Context, string, string, domain.Permission) (domain.Permission, error)
	ListRoles(context.Context, string, PageRequest) (PageResult[domain.Role], error)
	CreateRole(context.Context, string, string, string, domain.Role, []string) (domain.Role, error)
	GetRole(context.Context, string, string) (domain.Role, error)
	UpdateRole(context.Context, string, string, string, domain.Role, []string) (domain.Role, error)
	ListRoleBindings(context.Context, string, PageRequest) (PageResult[domain.RoleBinding], error)
	CreateRoleBinding(context.Context, string, string, domain.RoleBinding) (domain.RoleBinding, error)
	UpdateRoleBinding(context.Context, string, string, domain.RoleBinding) (domain.RoleBinding, error)
	PreviewEffectiveAccess(context.Context, string, string, string, time.Time) (domain.EffectiveAccessPreview, error)
	PreviewRoleBindingImpact(context.Context, RoleBindingImpactInput, time.Time) (domain.RoleBindingImpactPreview, error)
	Check(context.Context, CheckInput) (domain.Decision, error)
}

// Service is the authorization application service.
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService validates authorization dependencies.
func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("authorization service dependencies must not be nil")
	}
	return &Service{repository: repository, ids: ids, clock: clock}, nil
}

type ResourceCreateInput struct{ TenantID, OperatorID, ApplicationCode, Code, Name, ResourceType string }

func (s *Service) ListResources(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Resource], error) {
	return s.repository.ListResources(ctx, tenantID, normalizePage(query))
}
func (s *Service) CreateResource(ctx context.Context, in ResourceCreateInput) (domain.Resource, error) {
	if err := require(in.TenantID, in.OperatorID, in.ApplicationCode, in.Code, in.Name, in.ResourceType); err != nil {
		return domain.Resource{}, err
	}
	if !oneOf(in.ResourceType, "MENU", "PAGE", "API", "DATA") {
		return domain.Resource{}, validation("resource_type is invalid")
	}
	if err := lengthAtMost("code", in.Code, 128); err != nil {
		return domain.Resource{}, err
	}
	if err := lengthAtMost("name", in.Name, 100); err != nil {
		return domain.Resource{}, err
	}
	id, err := s.ids.New(s.clock.Now())
	if err != nil {
		return domain.Resource{}, fmt.Errorf("generate resource ID: %w", err)
	}
	return s.repository.CreateResource(ctx, in.TenantID, in.OperatorID, domain.Resource{ID: id, ApplicationCode: strings.TrimSpace(in.ApplicationCode), Code: strings.TrimSpace(in.Code), Name: strings.TrimSpace(in.Name), ResourceType: in.ResourceType})
}

type PermissionCreateInput struct{ TenantID, OperatorID, ResourceID, Code, Name, Action string }

func (s *Service) ListPermissions(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Permission], error) {
	return s.repository.ListPermissions(ctx, tenantID, normalizePage(query))
}
func (s *Service) CreatePermission(ctx context.Context, in PermissionCreateInput) (domain.Permission, error) {
	if err := require(in.TenantID, in.OperatorID, in.ResourceID, in.Code, in.Name, in.Action); err != nil {
		return domain.Permission{}, err
	}
	// The migration-owned authz_permission.code column is VARCHAR(128); do not silently truncate OpenAPI input.
	if err := lengthAtMost("code", in.Code, 128); err != nil {
		return domain.Permission{}, err
	}
	if err := lengthAtMost("name", in.Name, 100); err != nil {
		return domain.Permission{}, err
	}
	id, err := s.ids.New(s.clock.Now())
	if err != nil {
		return domain.Permission{}, fmt.Errorf("generate permission ID: %w", err)
	}
	return s.repository.CreatePermission(ctx, in.TenantID, in.OperatorID, domain.Permission{ID: id, Code: strings.TrimSpace(in.Code), Name: strings.TrimSpace(in.Name), Action: strings.TrimSpace(in.Action), Resource: domain.Reference{ID: strings.TrimSpace(in.ResourceID)}})
}

type RoleCreateInput struct {
	TenantID, OperatorID, OperatorAccountID, Name string
	Description                                   *string
	PermissionIDs                                 []string
}

func (s *Service) ListRoles(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.Role], error) {
	return s.repository.ListRoles(ctx, tenantID, normalizePage(query))
}
func (s *Service) CreateRole(ctx context.Context, in RoleCreateInput) (domain.Role, error) {
	if err := require(in.TenantID, in.OperatorID, in.OperatorAccountID, in.Name); err != nil {
		return domain.Role{}, err
	}
	if err := uniqueIDs(in.PermissionIDs); err != nil {
		return domain.Role{}, err
	}
	if err := lengthAtMost("name", in.Name, 100); err != nil {
		return domain.Role{}, err
	}
	if in.Description != nil {
		if err := lengthAtMost("description", *in.Description, 500); err != nil {
			return domain.Role{}, err
		}
	}
	id, err := s.ids.New(s.clock.Now())
	if err != nil {
		return domain.Role{}, fmt.Errorf("generate role ID: %w", err)
	}
	// 自定义角色编码由不可变的 ULID 派生，名称调整不会改变外部引用；真正的“能否授予”
	// 仍由仓储在同一事务内按操作者可委派权限集合校验，不能只相信管理接口权限。
	return s.repository.CreateRole(ctx, in.TenantID, in.OperatorID, in.OperatorAccountID, domain.Role{ID: id, Code: generatedRoleCode(id), Name: strings.TrimSpace(in.Name), Description: trimPointer(in.Description), Status: domain.StatusActive}, in.PermissionIDs)
}
func (s *Service) GetRole(ctx context.Context, tenantID, roleID string) (domain.Role, error) {
	if err := require(tenantID, roleID); err != nil {
		return domain.Role{}, err
	}
	return s.repository.GetRole(ctx, tenantID, roleID)
}

type RoleUpdateInput struct {
	TenantID, OperatorID, OperatorAccountID, RoleID, Name, Status string
	Description                                                   *string
	PermissionIDs                                                 []string
	Version                                                       uint64
}

func (s *Service) UpdateRole(ctx context.Context, in RoleUpdateInput) (domain.Role, error) {
	if err := require(in.TenantID, in.OperatorID, in.OperatorAccountID, in.RoleID, in.Name, in.Status); err != nil {
		return domain.Role{}, err
	}
	if in.Version == 0 {
		return domain.Role{}, validation("version is required")
	}
	if !oneOf(in.Status, domain.StatusActive, domain.StatusDisabled) {
		return domain.Role{}, validation("status is invalid")
	}
	if err := uniqueIDs(in.PermissionIDs); err != nil {
		return domain.Role{}, err
	}
	if err := lengthAtMost("name", in.Name, 100); err != nil {
		return domain.Role{}, err
	}
	if in.Description != nil {
		if err := lengthAtMost("description", *in.Description, 500); err != nil {
			return domain.Role{}, err
		}
	}
	// 版本号保护管理员并发编辑；权限子集和内置角色不可编辑等安全边界由仓储结合
	// 当前数据库状态复核，避免校验与写入之间出现时序窗口。
	return s.repository.UpdateRole(ctx, in.TenantID, in.OperatorID, in.OperatorAccountID, domain.Role{ID: in.RoleID, Name: strings.TrimSpace(in.Name), Description: trimPointer(in.Description), Status: in.Status, Version: in.Version}, in.PermissionIDs)
}

// generatedRoleCode creates a stable custom-role code from its ULID primary key.
func generatedRoleCode(id string) string {
	return "ROLE-" + strings.ToUpper(strings.TrimSpace(id))
}

type RoleBindingCreateInput struct {
	TenantID, OperatorID, RoleID, SubjectType, SubjectID, ScopeType string
	ScopeID                                                         *string
	ExpiresAt                                                       *time.Time
}

func (s *Service) ListRoleBindings(ctx context.Context, tenantID string, query PageRequest) (PageResult[domain.RoleBinding], error) {
	return s.repository.ListRoleBindings(ctx, tenantID, normalizePage(query))
}
func (s *Service) CreateRoleBinding(ctx context.Context, in RoleBindingCreateInput) (domain.RoleBinding, error) {
	if err := validateBinding(in.TenantID, in.OperatorID, in.RoleID, in.SubjectType, in.SubjectID, in.ScopeType, in.ScopeID, in.ExpiresAt, s.clock.Now()); err != nil {
		return domain.RoleBinding{}, err
	}
	id, err := s.ids.New(s.clock.Now())
	if err != nil {
		return domain.RoleBinding{}, fmt.Errorf("generate role binding ID: %w", err)
	}
	// 此处只规范绑定形状；受保护角色、操作者可委派范围、主体归属和作用域目标
	// 必须在事务内基于可信数据库记录再次校验。
	return s.repository.CreateRoleBinding(ctx, in.TenantID, in.OperatorID, bindingFromInput(id, in, 0))
}

type RoleBindingUpdateInput struct {
	TenantID, OperatorID, BindingID, RoleID, SubjectType, SubjectID, ScopeType, Status string
	ScopeID                                                                            *string
	ExpiresAt                                                                          *time.Time
	Version                                                                            uint64
}

func (s *Service) UpdateRoleBinding(ctx context.Context, in RoleBindingUpdateInput) (domain.RoleBinding, error) {
	if err := validateBinding(in.TenantID, in.OperatorID, in.RoleID, in.SubjectType, in.SubjectID, in.ScopeType, in.ScopeID, in.ExpiresAt, s.clock.Now()); err != nil {
		return domain.RoleBinding{}, err
	}
	if err := require(in.BindingID, in.Status); err != nil {
		return domain.RoleBinding{}, err
	}
	if !oneOf(in.Status, domain.StatusActive, domain.StatusDisabled) {
		return domain.RoleBinding{}, validation("status is invalid")
	}
	if in.Version == 0 {
		return domain.RoleBinding{}, validation("version is required")
	}
	binding := bindingFromInput(in.BindingID, RoleBindingCreateInput{RoleID: in.RoleID, SubjectType: in.SubjectType, SubjectID: in.SubjectID, ScopeType: in.ScopeType, ScopeID: in.ScopeID, ExpiresAt: in.ExpiresAt}, in.Version)
	binding.Status = in.Status
	return s.repository.UpdateRoleBinding(ctx, in.TenantID, in.OperatorID, binding)
}
func bindingFromInput(id string, in RoleBindingCreateInput, version uint64) domain.RoleBinding {
	return domain.RoleBinding{ID: id, Role: domain.Reference{ID: strings.TrimSpace(in.RoleID)}, SubjectType: in.SubjectType, Subject: domain.Reference{ID: strings.TrimSpace(in.SubjectID)}, ScopeType: in.ScopeType, ScopeID: trimPointer(in.ScopeID), Status: domain.StatusActive, ExpiresAt: copyTime(in.ExpiresAt), Version: version}
}

// PreviewEffectiveAccess explains the roles and permissions that are currently effective for a
// specific login account. The account is required so account-level grants remain unambiguous.
func (s *Service) PreviewEffectiveAccess(ctx context.Context, tenantID, userID, accountID string) (domain.EffectiveAccessPreview, error) {
	if err := require(tenantID, userID, accountID); err != nil {
		return domain.EffectiveAccessPreview{}, err
	}
	return s.repository.PreviewEffectiveAccess(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(userID), strings.TrimSpace(accountID), s.clock.Now())
}

// RoleBindingImpactInput mirrors a proposed role binding without assigning it an ID or persisting it.
type RoleBindingImpactInput struct {
	TenantID, OperatorID, RoleID, SubjectType, SubjectID, ScopeType string
	ScopeID                                                         *string
	ExpiresAt                                                       *time.Time
}

// PreviewRoleBindingImpact returns the users that would receive the proposed active binding now.
func (s *Service) PreviewRoleBindingImpact(ctx context.Context, in RoleBindingImpactInput) (domain.RoleBindingImpactPreview, error) {
	if err := validateBinding(in.TenantID, in.OperatorID, in.RoleID, in.SubjectType, in.SubjectID, in.ScopeType, in.ScopeID, in.ExpiresAt, s.clock.Now()); err != nil {
		return domain.RoleBindingImpactPreview{}, err
	}
	return s.repository.PreviewRoleBindingImpact(ctx, in, s.clock.Now())
}

type CheckInput struct {
	TenantID, UserID, AccountID, PermissionCode, ResourceType, ResourceID string
	Context                                                               map[string]any
}

func (s *Service) Check(ctx context.Context, input CheckInput) (domain.Decision, error) {
	if err := require(input.TenantID, input.UserID, input.PermissionCode); err != nil {
		return domain.Decision{}, err
	}
	// 授权判断不能由会话中的扁平权限码单独完成；仓储还会把用户、账号、任职关系
	// 以及服务端解析出的组织/资源上下文共同带入策略查询。
	return s.repository.Check(ctx, input)
}

func normalizePage(q PageRequest) PageRequest {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 20
	}
	if q.PageSize > 100 {
		q.PageSize = 100
	}
	q.Keyword = strings.TrimSpace(q.Keyword)
	q.Status = strings.TrimSpace(q.Status)
	return q
}
func require(values ...string) error {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return validation("required field is empty")
		}
	}
	return nil
}
func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }
func lengthAtMost(field, value string, max int) error {
	if len([]rune(strings.TrimSpace(value))) > max {
		return validation(fmt.Sprintf("%s exceeds maximum length %d", field, max))
	}
	return nil
}
func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
func uniqueIDs(ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return validation("permission_ids contains an empty value")
		}
		if _, ok := seen[id]; ok {
			return validation("permission_ids must be unique")
		}
		seen[id] = struct{}{}
	}
	return nil
}
func validateBinding(tenant, operator, role, subjectType, subjectID, scopeType string, scopeID *string, expiresAt *time.Time, now time.Time) error {
	if err := require(tenant, operator, role, subjectType, subjectID, scopeType); err != nil {
		return err
	}
	if !oneOf(subjectType, "USER", "ACCOUNT", "ORG_UNIT", "POSITION") {
		return validation("subject_type is invalid")
	}
	if !oneOf(scopeType, "TENANT", "ORG_UNIT", "RESOURCE") {
		return validation("scope_type is invalid")
	}
	normalizedScopeID := ""
	if scopeID != nil {
		normalizedScopeID = strings.TrimSpace(*scopeID)
	}
	if scopeType == "TENANT" && normalizedScopeID != "" {
		return validation("tenant scope must not include scope_id")
	}
	if scopeType != "TENANT" && normalizedScopeID == "" {
		return validation("organization and resource scopes require scope_id")
	}
	// TENANT 用空 scope_id 表示全租户；组织和资源作用域必须显式指向目标。
	// 这一规范化约定也是数据库唯一约束和后续策略匹配的组成部分。
	if expiresAt != nil && !expiresAt.After(now.UTC()) {
		return validation("expires_at must be in the future")
	}
	return nil
}
func trimPointer(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}
func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := value.UTC()
	return &copied
}
