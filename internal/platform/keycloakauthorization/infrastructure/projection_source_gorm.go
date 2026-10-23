package infrastructure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/authorization/applicationaccess"
	projectionapplication "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/application"
	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
	"gorm.io/gorm"
)

type projectionSourceRow struct {
	EnvironmentID    string `gorm:"column:environment_id"`
	KeycloakClientID string `gorm:"column:keycloak_client_id"`
	ApplicationCode  string `gorm:"column:application_code"`
}

type projectionUserRow struct {
	DisplayName     string     `gorm:"column:display_name"`
	Email           *string    `gorm:"column:email"`
	Status          string     `gorm:"column:status"`
	DeletedAt       *time.Time `gorm:"column:deleted_at"`
	HasLoginAccount bool       `gorm:"column:has_login_account"`
}

// ProjectionSource 从平台数据库读取投影事件对应的授权快照，供 Synchronizer
// 写入 Keycloak；Client 映射是权威输入，投影表本身只是输出状态。
type ProjectionSource struct {
	database *gorm.DB
	access   *applicationaccess.Service
}

// NewProjectionSource 构造投影读取器；database 或 access 为空时返回错误。
func NewProjectionSource(database *gorm.DB, access *applicationaccess.Service) (*ProjectionSource, error) {
	if database == nil || access == nil {
		return nil, fmt.Errorf("Keycloak projection source dependencies must not be nil")
	}
	return &ProjectionSource{database: database, access: access}, nil
}

// LoadAuthorizationProjection 加载事件对应的快照：先以已同步的 Client 映射定位
// 环境/Client，再读用户；停用与业务删除的用户刻意投影为空授权（仅置 disabled），
// 活动用户则复用与 OIDC 令牌相同口径的有效授权计算。
func (source *ProjectionSource) LoadAuthorizationProjection(ctx context.Context, event projectionworker.Event) (projectionapplication.Snapshot, error) {
	// 投影表是输出状态，因此在首次同步之前绝不能成为前置依赖。
	// Client 映射才是权威输入。
	var row projectionSourceRow
	err := keycloakClientMappingQuery(source.database.WithContext(ctx), event).Take(&row).Error
	if err != nil {
		return projectionapplication.Snapshot{}, fmt.Errorf("load Keycloak application Client mapping: %w", err)
	}
	var user projectionUserRow
	if err := projectionUserQuery(source.database.WithContext(ctx), event).Take(&user).Error; err != nil {
		return projectionapplication.Snapshot{}, fmt.Errorf("load Keycloak projection user: %w", err)
	}
	enabled := projectionUserEnabled(user)
	base := projectionapplication.Snapshot{TenantID: event.TenantID, IdentityID: event.IdentityID, ApplicationID: event.ApplicationID, EnvironmentID: row.EnvironmentID, ApplicationCode: strings.TrimSpace(row.ApplicationCode), KeycloakClientID: strings.TrimSpace(row.KeycloakClientID), DisplayName: strings.TrimSpace(user.DisplayName), UserEnabled: enabled}
	if user.Email != nil {
		base.Email = strings.TrimSpace(*user.Email)
	}
	// 停用与业务删除的用户被刻意投影为无任何授权授予。applicationaccess 只为
	// 活动用户解析 claims，但本投影仍需把既有 Keycloak 用户置为 disabled。
	if !enabled {
		return base, nil
	}
	authorization, err := source.access.ResolveKeycloakAuthorizationSnapshot(ctx, event.TenantID, event.IdentityID, event.ApplicationID, event.EnvironmentID)
	if err != nil {
		return projectionapplication.Snapshot{}, err
	}
	base.TenantID = authorization.TenantID
	base.IdentityID = authorization.IdentityID
	base.ApplicationID = authorization.ApplicationID
	base.ApplicationCode = authorization.ApplicationCode
	base.PersonID = authorization.PersonID
	base.PrimaryOrganizationID = authorization.PrimaryOrgID
	base.OrganizationIDs = authorization.OrganizationIDs
	base.Roles = authorization.Roles
	base.Permissions = authorization.Permissions
	base.RoleConfigHash = authorization.RoleConfigHash
	base.AuthorizationRevision = authorization.AuthzRevision
	return base, nil
}

func keycloakClientMappingQuery(database *gorm.DB, event projectionworker.Event) *gorm.DB {
	return database.Table("keycloak_application_client_mapping AS client_mapping").
		Select("client_mapping.environment_id, client_mapping.keycloak_client_id, platform_application.code AS application_code").
		Joins("JOIN platform_application ON platform_application.tenant_id = client_mapping.tenant_id AND platform_application.id = client_mapping.application_id").
		Where("client_mapping.tenant_id = ? AND client_mapping.application_id = ? AND client_mapping.status = ?", event.TenantID, event.ApplicationID, "SYNCED").
		Where("client_mapping.environment_id = ?", event.EnvironmentID).
		Order("environment_id ASC")
}

// projectionUserQuery 只按业务状态判断是否投影为启用。
//
// 临时锁定（account.locked_until 在未来）是密码失败策略的瞬态结果，不是业务停用。
// 曾经把锁定当成“没有可用账号”，于是锁定期间 Keycloak 用户被投影为 disabled；管理员
// 只要读取一次账号列表，这个 disabled 又会被反向写回平台，永久停用用户、凭据和任职，
// 锁定到期后无法自动恢复。临时锁定与业务停用必须在投影里分开处理。
func projectionUserQuery(database *gorm.DB, event projectionworker.Event) *gorm.DB {
	return database.Table("iam_user AS user_record").
		Select(`display_name, email, status, deleted_at, EXISTS (
			SELECT 1 FROM iam_account AS account
			WHERE account.tenant_id = user_record.tenant_id AND account.user_id = user_record.id
				AND account.status = 'ACTIVE'
				AND (account.valid_until IS NULL OR account.valid_until > UTC_TIMESTAMP(3))
		) AS has_login_account`).
		Where("user_record.tenant_id = ? AND user_record.id = ?", event.TenantID, event.IdentityID)
}

func projectionUserEnabled(user projectionUserRow) bool {
	return strings.EqualFold(strings.TrimSpace(user.Status), "ACTIVE") && user.DeletedAt == nil && user.HasLoginAccount
}

var _ projectionapplication.Source = (*ProjectionSource)(nil)
