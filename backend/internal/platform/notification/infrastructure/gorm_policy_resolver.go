package infrastructure

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/notification/domain"
	"gorm.io/gorm"
)

// InboxPolicy reads the tenant-owned in-app notification switch without importing settings internals.
type InboxPolicy struct{ database *gorm.DB }

// NewInboxPolicy constructs the settings read adapter used by notification creation.
func NewInboxPolicy(database *gorm.DB) (*InboxPolicy, error) {
	if database == nil {
		return nil, fmt.Errorf("notification policy database must not be nil")
	}
	return &InboxPolicy{database: database}, nil
}

type inboxSettingRow struct {
	InboxEnabled bool `gorm:"column:inbox_enabled"`
}

func (inboxSettingRow) TableName() string { return "notification_setting" }

// InboxEnabled defaults to true before a tenant writes notification settings, matching the existing settings read model.
func (policy *InboxPolicy) InboxEnabled(ctx context.Context, tenantID string) (bool, error) {
	var row inboxSettingRow
	err := policy.database.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&row).Error
	if err == nil {
		return row.InboxEnabled, nil
	}
	if err == gorm.ErrRecordNotFound {
		return true, nil
	}
	return false, fmt.Errorf("read notification inbox policy: %w", err)
}

// RecipientResolver resolves only active users in the current tenant. Role and organization
// audiences are data selectors; they never imply a permission grant.
type RecipientResolver struct{ database *gorm.DB }

// NewRecipientResolver constructs the GORM tenant audience resolver.
func NewRecipientResolver(database *gorm.DB) (*RecipientResolver, error) {
	if database == nil {
		return nil, fmt.Errorf("notification recipient resolver database must not be nil")
	}
	return &RecipientResolver{database: database}, nil
}

type recipientUserRow struct {
	ID string `gorm:"column:id"`
}

// ResolveRecipients resolves USER, ORGANIZATION and ROLE targets to enabled tenant users.
func (resolver *RecipientResolver) ResolveRecipients(ctx context.Context, tenantID string, targets []domain.RecipientTarget, at time.Time) ([]string, error) {
	users := make(map[string]struct{})
	for _, target := range targets {
		targetID := strings.TrimSpace(target.ID)
		if targetID == "" {
			continue
		}
		var rows []recipientUserRow
		var err error
		switch target.Type {
		case domain.RecipientTypeUser:
			err = resolver.database.WithContext(ctx).Table("iam_user AS u").Select("u.id").Where("u.tenant_id = ? AND u.id = ? AND u.status = ?", tenantID, targetID, "ACTIVE").Find(&rows).Error
		case domain.RecipientTypeOrganization:
			err = resolver.database.WithContext(ctx).Table("iam_membership AS m").Joins("JOIN iam_user AS u ON u.id = m.user_id AND u.tenant_id = m.tenant_id").Select("DISTINCT u.id").Where("m.tenant_id = ? AND m.org_unit_id = ? AND m.status = ? AND u.status = ? AND (m.valid_from IS NULL OR m.valid_from <= ?) AND (m.valid_until IS NULL OR m.valid_until >= ?)", tenantID, targetID, "ACTIVE", "ACTIVE", at, at).Find(&rows).Error
		case domain.RecipientTypeRole:
			err = resolver.database.WithContext(ctx).Table("authz_role_binding AS b").Joins("JOIN iam_user AS u ON u.id = b.subject_id AND u.tenant_id = b.tenant_id").Select("DISTINCT u.id").Where("b.tenant_id = ? AND b.role_id = ? AND b.status = ? AND b.subject_type = ? AND u.status = ?", tenantID, targetID, "ACTIVE", "USER", "ACTIVE").Find(&rows).Error
		default:
			return nil, fmt.Errorf("unsupported notification recipient target %q", target.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve notification recipients: %w", err)
		}
		for _, row := range rows {
			if row.ID != "" {
				users[row.ID] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(users))
	for userID := range users {
		result = append(result, userID)
	}
	sort.Strings(result)
	return result, nil
}
