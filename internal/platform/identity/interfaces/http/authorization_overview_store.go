package identityhttp

import (
	"context"
	"errors"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/internal/platform/bootstrapadmin"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"gorm.io/gorm"
)

// 总览只输出服务端显式列投影，绝不再回传原始行。iam_user/iam_account 中的手机号密文、
// 摘要、乐观锁版本与创建/更新人属于管理写入内部字段，不该出现在只读聚合里。
const (
	overviewUserColumns = `id, display_name, employee_no, email, status, employment_status, primary_org_id, manager_user_id, valid_until, created_at, updated_at`
	// external_subject 是既有前端映射使用的字段名（frontend iam/api/authorization.js），
	// 与 external_subject_id 同源，避免前端把 Keycloak 映射显示成“未返回”。
	overviewAccountColumns = `id, user_id, username, account_type, auth_source, external_subject_id, external_subject_id AS external_subject, valid_until, locked_until, last_login_at, status, created_at, updated_at, EXISTS (
		SELECT 1 FROM iam_password_credential AS credential
		WHERE credential.account_id = iam_account.id AND credential.status = 'ACTIVE'
	) AS password_initialized`
	overviewRoleBindingColumns   = `id, role_id, subject_type, subject_id, scope_type, scope_id, grant_origin, valid_until, status, created_at`
	overviewMembershipColumns    = `id, user_id, org_unit_id, position_id, membership_type, is_primary, inherit_authorization, valid_from, valid_until, status, created_at, updated_at`
	overviewPendingChangeColumns = `id, user_id, change_type, status, source_membership_id, target_org_unit_id, target_position_id, effective_at, submitted_by, created_at, updated_at`
	overviewHandoverColumns      = `id, request_id, system_code, resource_type, resource_id, current_owner_id, target_owner_id, status, created_at, updated_at`
	// keycloak_authorization_outbox 是可选表；只投影同步状态字段，不含错误消息或重试内部状态。
	overviewKeycloakSyncColumns = `id, identity_id, event_type, status, created_at, completed_at`
)

// authorizationOverviewSections 是总览除用户自身之外的只读分节。
type authorizationOverviewSections struct {
	Accounts       []map[string]any
	Memberships    []map[string]any
	RoleBindings   []map[string]any
	PendingChanges []map[string]any
	Handover       []map[string]any
	KeycloakSync   []map[string]any
}

// authorizationOverviewStore 提供总览所需的只读查询。生产实现按租户直连平台库；
// 处理器测试可注入确定性假实现，避免把安全判定绑在真实数据库上。
type authorizationOverviewStore interface {
	// User 读取目标用户的显式列投影；用户不存在时返回 application.ErrNotFound。
	User(ctx context.Context, tenantID, userID string) (map[string]any, error)
	// ActiveMembershipOrgUnitIDs 读取目标用户 ACTIVE 任职所在的组织单元。
	ActiveMembershipOrgUnitIDs(ctx context.Context, tenantID, userID string) ([]string, error)
	// Sections 读取账号、任职、角色绑定、在途变更、交接与 Keycloak 同步状态。
	Sections(ctx context.Context, tenantID, userID string) (authorizationOverviewSections, error)
	// BootstrapAdministrator 解析部署初始化超管身份；未初始化时第二个返回值为 false。
	BootstrapAdministrator(ctx context.Context, tenantID string) (bootstrapadmin.Identity, bool, error)
}

type gormAuthorizationOverviewStore struct{ database *gorm.DB }

func (store *gormAuthorizationOverviewStore) User(ctx context.Context, tenantID, userID string) (map[string]any, error) {
	return store.take(ctx, "iam_user", overviewUserColumns, "tenant_id = ? AND id = ?", tenantID, userID)
}

func (store *gormAuthorizationOverviewStore) ActiveMembershipOrgUnitIDs(ctx context.Context, tenantID, userID string) ([]string, error) {
	var orgUnitIDs []string
	if err := store.database.WithContext(ctx).Table("iam_membership").
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, "ACTIVE").
		Distinct().
		Pluck("org_unit_id", &orgUnitIDs).Error; err != nil {
		return nil, fmt.Errorf("load user membership scope: %w", err)
	}
	return orgUnitIDs, nil
}

func (store *gormAuthorizationOverviewStore) Sections(ctx context.Context, tenantID, userID string) (authorizationOverviewSections, error) {
	var sections authorizationOverviewSections
	accounts, err := store.find(ctx, "iam_account", overviewAccountColumns, "tenant_id = ? AND user_id = ?", tenantID, userID)
	if err != nil {
		return authorizationOverviewSections{}, err
	}
	sections.Accounts = accounts
	memberships, err := store.find(ctx, "iam_membership", overviewMembershipColumns, "tenant_id = ? AND user_id = ?", tenantID, userID)
	if err != nil {
		return authorizationOverviewSections{}, err
	}
	sections.Memberships = memberships
	bindings, err := store.find(ctx, "authz_role_binding", overviewRoleBindingColumns, "tenant_id = ? AND subject_type = 'USER' AND subject_id = ?", tenantID, userID)
	if err != nil {
		return authorizationOverviewSections{}, err
	}
	sections.RoleBindings = bindings
	pendingChanges, err := store.find(ctx, "iam_personnel_change_request", overviewPendingChangeColumns, "tenant_id = ? AND user_id = ? AND status IN ('DRAFT','PENDING_APPROVAL','PENDING_HANDOVER','SCHEDULED')", tenantID, userID)
	if err != nil {
		return authorizationOverviewSections{}, err
	}
	sections.PendingChanges = pendingChanges
	handover, err := store.find(ctx, "iam_personnel_handover_item", overviewHandoverColumns, "tenant_id = ? AND current_owner_id = ? AND status NOT IN ('COMPLETED','CANCELLED')", tenantID, userID)
	if err != nil {
		return authorizationOverviewSections{}, err
	}
	sections.Handover = handover
	// outbox 表存在时才输出 Keycloak 同步状态；该可选表缺失是安全情形，故忽略错误。
	var sync []map[string]any
	store.database.WithContext(ctx).Table("keycloak_authorization_outbox").
		Select(overviewKeycloakSyncColumns).
		Where("tenant_id = ? AND identity_id = ?", tenantID, userID).
		Order("created_at DESC").Limit(20).Find(&sync)
	if sync == nil {
		sync = []map[string]any{}
	}
	sections.KeycloakSync = sync
	return sections, nil
}

func (store *gormAuthorizationOverviewStore) BootstrapAdministrator(ctx context.Context, tenantID string) (bootstrapadmin.Identity, bool, error) {
	return bootstrapadmin.Resolve(ctx, store.database, tenantID)
}

func (store *gormAuthorizationOverviewStore) take(ctx context.Context, table, columns, where string, args ...any) (map[string]any, error) {
	var row map[string]any
	result := store.database.WithContext(ctx).Table(table).Select(columns).Where(where, args...).Take(&row)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, application.ErrNotFound
		}
		return nil, fmt.Errorf("read authorization overview row: %w", result.Error)
	}
	return row, nil
}

func (store *gormAuthorizationOverviewStore) find(ctx context.Context, table, columns, where string, args ...any) ([]map[string]any, error) {
	var rows []map[string]any
	if err := store.database.WithContext(ctx).Table(table).Select(columns).Where(where, args...).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read authorization overview rows: %w", err)
	}
	if rows == nil {
		return []map[string]any{}, nil
	}
	return rows, nil
}
