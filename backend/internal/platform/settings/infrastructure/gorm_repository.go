// Package infrastructure provides GORM persistence for tenant settings.
package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Repository persists settings in MySQL through GORM.
type Repository struct {
	database *gorm.DB
}

// NewRepository constructs a settings repository.
func NewRepository(database *gorm.DB) (*Repository, error) {
	if database == nil {
		return nil, errors.New("settings database must not be nil")
	}
	return &Repository{database: database}, nil
}

type platformSettingsModel struct {
	ID                string    `gorm:"column:id;primaryKey"`
	TenantID          string    `gorm:"column:tenant_id"`
	OrganizationName  string    `gorm:"column:organization_name"`
	OrganizationAlias string    `gorm:"column:organization_alias"`
	Timezone          string    `gorm:"column:timezone"`
	Qualification     string    `gorm:"column:qualification"`
	Version           uint64    `gorm:"column:version"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	CreatedBy         *string   `gorm:"column:created_by"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
	UpdatedBy         *string   `gorm:"column:updated_by"`
}

func (platformSettingsModel) TableName() string { return "platform_setting" }

type notificationSettingsModel struct {
	ID                string    `gorm:"column:id;primaryKey"`
	TenantID          string    `gorm:"column:tenant_id"`
	InboxEnabled      bool      `gorm:"column:inbox_enabled"`
	EmailEnabled      bool      `gorm:"column:email_enabled"`
	ReminderFrequency string    `gorm:"column:reminder_frequency"`
	Version           uint64    `gorm:"column:version"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	CreatedBy         *string   `gorm:"column:created_by"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
	UpdatedBy         *string   `gorm:"column:updated_by"`
}

func (notificationSettingsModel) TableName() string { return "notification_setting" }

type accessSettingsModel struct {
	ID                        string    `gorm:"column:id;primaryKey"`
	TenantID                  string    `gorm:"column:tenant_id"`
	PublicOrigin              string    `gorm:"column:public_origin"`
	AllowInsecureHTTPRedirect bool      `gorm:"column:allow_insecure_http_redirect"`
	Version                   uint64    `gorm:"column:version"`
	CreatedAt                 time.Time `gorm:"column:created_at"`
	CreatedBy                 *string   `gorm:"column:created_by"`
	UpdatedAt                 time.Time `gorm:"column:updated_at"`
	UpdatedBy                 *string   `gorm:"column:updated_by"`
}

func (accessSettingsModel) TableName() string { return "settings_access" }

// GetPlatformSettings retrieves exactly one tenant settings aggregate.
func (repository *Repository) GetPlatformSettings(ctx context.Context, tenantID string) (domain.PlatformSettings, error) {
	var row platformSettingsModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.PlatformSettings{}, application.ErrNotFound
		}
		return domain.PlatformSettings{}, fmt.Errorf("get platform settings: %w", err)
	}
	return platformSettingsToDomain(row), nil
}

// SavePlatformSettings creates the initial tenant row or replaces it under optimistic locking.
func (repository *Repository) SavePlatformSettings(ctx context.Context, input application.PlatformSettingsUpdateInput, settingsID string, now time.Time) (domain.PlatformSettings, error) {
	var saved domain.PlatformSettings
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row platformSettingsModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ?", input.TenantID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.Version != 1 {
				return application.ErrVersionConflict
			}
			operatorID := input.OperatorID
			row = platformSettingsModel{ID: settingsID, TenantID: input.TenantID, OrganizationName: input.OrganizationName, OrganizationAlias: input.OrganizationAlias, Timezone: input.Timezone, Qualification: input.Qualification, Version: 1, CreatedAt: now, CreatedBy: &operatorID, UpdatedAt: now, UpdatedBy: &operatorID}
			if err := transaction.Create(&row).Error; err != nil {
				return mapError(err)
			}
			saved = platformSettingsToDomain(row)
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock platform settings: %w", err)
		}
		if row.Version != input.Version {
			return application.ErrVersionConflict
		}
		operatorID := input.OperatorID
		row.OrganizationName = input.OrganizationName
		row.OrganizationAlias = input.OrganizationAlias
		row.Timezone = input.Timezone
		row.Qualification = input.Qualification
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = &operatorID
		if err := transaction.Model(&platformSettingsModel{}).
			Where("id = ? AND tenant_id = ?", row.ID, input.TenantID).
			Select("organization_name", "organization_alias", "timezone", "qualification", "version", "updated_at", "updated_by").
			Updates(&row).Error; err != nil {
			return fmt.Errorf("save platform settings: %w", err)
		}
		saved = platformSettingsToDomain(row)
		return nil
	})
	if err != nil {
		return domain.PlatformSettings{}, err
	}
	return saved, nil
}

// GetNotificationSettings retrieves exactly one tenant notification settings aggregate.
func (repository *Repository) GetNotificationSettings(ctx context.Context, tenantID string) (domain.NotificationSettings, error) {
	var row notificationSettingsModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.NotificationSettings{}, application.ErrNotFound
		}
		return domain.NotificationSettings{}, fmt.Errorf("get notification settings: %w", err)
	}
	return notificationSettingsToDomain(row), nil
}

// SaveNotificationSettings creates the initial tenant row or replaces it under optimistic locking.
func (repository *Repository) SaveNotificationSettings(ctx context.Context, input application.NotificationSettingsUpdateInput, settingsID string, now time.Time) (domain.NotificationSettings, error) {
	var saved domain.NotificationSettings
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row notificationSettingsModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ?", input.TenantID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.Version != 1 {
				return application.ErrVersionConflict
			}
			operatorID := input.OperatorID
			row = notificationSettingsModel{ID: settingsID, TenantID: input.TenantID, InboxEnabled: input.InboxEnabled, EmailEnabled: input.EmailEnabled, ReminderFrequency: string(input.ReminderFrequency), Version: 1, CreatedAt: now, CreatedBy: &operatorID, UpdatedAt: now, UpdatedBy: &operatorID}
			if err := transaction.Create(&row).Error; err != nil {
				return mapError(err)
			}
			saved = notificationSettingsToDomain(row)
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock notification settings: %w", err)
		}
		if row.Version != input.Version {
			return application.ErrVersionConflict
		}
		operatorID := input.OperatorID
		row.InboxEnabled = input.InboxEnabled
		row.EmailEnabled = input.EmailEnabled
		row.ReminderFrequency = string(input.ReminderFrequency)
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = &operatorID
		if err := transaction.Model(&notificationSettingsModel{}).
			Where("id = ? AND tenant_id = ?", row.ID, input.TenantID).
			Select("inbox_enabled", "email_enabled", "reminder_frequency", "version", "updated_at", "updated_by").
			Updates(&row).Error; err != nil {
			return fmt.Errorf("save notification settings: %w", err)
		}
		saved = notificationSettingsToDomain(row)
		return nil
	})
	if err != nil {
		return domain.NotificationSettings{}, err
	}
	return saved, nil
}

// GetAccessSettings retrieves exactly one tenant access settings aggregate.
func (repository *Repository) GetAccessSettings(ctx context.Context, tenantID string) (domain.AccessSettings, error) {
	var row accessSettingsModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.AccessSettings{}, application.ErrNotFound
		}
		return domain.AccessSettings{}, fmt.Errorf("get access settings: %w", err)
	}
	return accessSettingsToDomain(row), nil
}

// SaveAccessSettings 首次保存只接受默认版本 1；已有记录先加行锁再核对 Version，
// 防止两个管理端同时修改对外地址时后提交者静默覆盖先提交者。
func (repository *Repository) SaveAccessSettings(ctx context.Context, input application.AccessSettingsUpdateInput, settingsID string, now time.Time) (domain.AccessSettings, error) {
	var saved domain.AccessSettings
	err := repository.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var row accessSettingsModel
		err := transaction.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ?", input.TenantID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.Version != 1 {
				return application.ErrVersionConflict
			}
			operatorID := input.OperatorID
			row = accessSettingsModel{ID: settingsID, TenantID: input.TenantID, PublicOrigin: input.PublicOrigin, AllowInsecureHTTPRedirect: input.AllowInsecureHTTPRedirect, Version: 1, CreatedAt: now, CreatedBy: &operatorID, UpdatedAt: now, UpdatedBy: &operatorID}
			if err := transaction.Create(&row).Error; err != nil {
				return mapError(err)
			}
			saved = accessSettingsToDomain(row)
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock access settings: %w", err)
		}
		if row.Version != input.Version {
			return application.ErrVersionConflict
		}
		operatorID := input.OperatorID
		row.PublicOrigin = input.PublicOrigin
		row.AllowInsecureHTTPRedirect = input.AllowInsecureHTTPRedirect
		row.Version++
		row.UpdatedAt = now
		row.UpdatedBy = &operatorID
		if err := transaction.Model(&accessSettingsModel{}).
			Where("id = ? AND tenant_id = ?", row.ID, input.TenantID).
			Select("public_origin", "allow_insecure_http_redirect", "version", "updated_at", "updated_by").
			Updates(&row).Error; err != nil {
			return fmt.Errorf("save access settings: %w", err)
		}
		saved = accessSettingsToDomain(row)
		return nil
	})
	if err != nil {
		return domain.AccessSettings{}, err
	}
	return saved, nil
}

func accessSettingsToDomain(row accessSettingsModel) domain.AccessSettings {
	return domain.AccessSettings{ID: row.ID, TenantID: row.TenantID, PublicOrigin: row.PublicOrigin, AllowInsecureHTTPRedirect: row.AllowInsecureHTTPRedirect, Version: row.Version, UpdatedAt: row.UpdatedAt}
}

func platformSettingsToDomain(row platformSettingsModel) domain.PlatformSettings {
	return domain.PlatformSettings{ID: row.ID, TenantID: row.TenantID, OrganizationName: row.OrganizationName, OrganizationAlias: row.OrganizationAlias, Timezone: row.Timezone, Qualification: row.Qualification, Version: row.Version, UpdatedAt: row.UpdatedAt}
}

func notificationSettingsToDomain(row notificationSettingsModel) domain.NotificationSettings {
	return domain.NotificationSettings{ID: row.ID, TenantID: row.TenantID, InboxEnabled: row.InboxEnabled, EmailEnabled: row.EmailEnabled, ReminderFrequency: domain.ReminderFrequency(row.ReminderFrequency), Version: row.Version, UpdatedAt: row.UpdatedAt}
}

func mapError(err error) error {
	if isDuplicateKey(err) {
		return application.ErrConflict
	}
	return err
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}

	message := err.Error()
	return strings.Contains(message, "Duplicate entry") || strings.Contains(message, "duplicate key")
}
