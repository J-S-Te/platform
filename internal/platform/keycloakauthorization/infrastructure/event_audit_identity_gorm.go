package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// AuditIdentityResolver 把 external_subject_id 中保存的 Keycloak 用户 ID 反查为
// 拥有该事件的租户/用户。它绝不猜测租户。
type AuditIdentityResolver struct{ database *gorm.DB }

// NewAuditIdentityResolver 构造身份反查器；database 为空时返回错误。
func NewAuditIdentityResolver(database *gorm.DB) (*AuditIdentityResolver, error) {
	if database == nil {
		return nil, errors.New("Keycloak audit identity database must not be nil")
	}
	return &AuditIdentityResolver{database: database}, nil
}

// ResolveKeycloakAuditIdentity 按 external_subject_id 反查唯一的活动 KEYCLOAK/FEDERATED
// 账号及其属主用户；无匹配或主体为空时返回空租户/用户，绝不猜测归属。
func (resolver *AuditIdentityResolver) ResolveKeycloakAuditIdentity(ctx context.Context, subjectID string) (string, string, error) {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return "", "", nil
	}
	var row struct {
		TenantID string `gorm:"column:tenant_id"`
		UserID   string `gorm:"column:user_id"`
	}
	result := resolver.database.WithContext(ctx).Table("iam_account AS account").
		Select("account.tenant_id, account.user_id").
		Joins("JOIN iam_user AS user ON user.tenant_id = account.tenant_id AND user.id = account.user_id").
		Where("account.external_subject_id = ? AND account.auth_source IN ? AND account.status = ? AND user.status = ?", subjectID, []string{"KEYCLOAK", "FEDERATED"}, "ACTIVE", "ACTIVE").
		Limit(1).
		Find(&row)
	if result.Error != nil {
		return "", "", fmt.Errorf("resolve Keycloak audit identity: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return "", "", nil
	}
	return strings.TrimSpace(row.TenantID), strings.TrimSpace(row.UserID), nil
}
