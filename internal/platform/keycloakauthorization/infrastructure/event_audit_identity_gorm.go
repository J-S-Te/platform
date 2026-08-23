package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// AuditIdentityResolver maps the Keycloak user id stored in external_subject_id
// back to the tenant/user that owns the event. It never guesses a tenant.
type AuditIdentityResolver struct{ database *gorm.DB }

func NewAuditIdentityResolver(database *gorm.DB) (*AuditIdentityResolver, error) {
	if database == nil {
		return nil, errors.New("Keycloak audit identity database must not be nil")
	}
	return &AuditIdentityResolver{database: database}, nil
}

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
