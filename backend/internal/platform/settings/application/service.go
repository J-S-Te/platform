// Package application coordinates platform and notification settings use cases.
package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/settings/domain"
)

var (
	// ErrNotFound indicates the repository has no persisted settings row.
	ErrNotFound = errors.New("settings not found")
	// ErrConflict indicates a uniqueness or lifecycle conflict.
	ErrConflict = errors.New("settings conflict")
	// ErrVersionConflict indicates an optimistic-lock version is stale.
	ErrVersionConflict = errors.New("settings version conflict")
	// ErrValidation indicates invalid application input.
	ErrValidation = errors.New("invalid settings input")
)

// IdentifierGenerator supplies sortable aggregate identifiers.
type IdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// Clock supplies the current time for deterministic tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// PlatformSettingsUpdateInput replaces the typed platform settings for one tenant.
type PlatformSettingsUpdateInput struct {
	TenantID          string
	OperatorID        string
	OrganizationName  string
	OrganizationAlias string
	Timezone          string
	Qualification     string
	Version           uint64
}

// NotificationSettingsUpdateInput replaces the typed notification settings for one tenant.
type NotificationSettingsUpdateInput struct {
	TenantID          string
	OperatorID        string
	InboxEnabled      bool
	EmailEnabled      bool
	ReminderFrequency domain.ReminderFrequency
	Version           uint64
}

// Repository persists settings aggregates.
type Repository interface {
	GetPlatformSettings(ctx context.Context, tenantID string) (domain.PlatformSettings, error)
	SavePlatformSettings(ctx context.Context, input PlatformSettingsUpdateInput, settingsID string, now time.Time) (domain.PlatformSettings, error)
	GetNotificationSettings(ctx context.Context, tenantID string) (domain.NotificationSettings, error)
	SaveNotificationSettings(ctx context.Context, input NotificationSettingsUpdateInput, settingsID string, now time.Time) (domain.NotificationSettings, error)
}

// Service exposes settings application use cases.
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService validates and constructs the settings application service.
func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("settings service dependencies must not be nil")
	}
	return &Service{repository: repository, ids: ids, clock: clock}, nil
}

// GetPlatformSettings returns saved settings or safe documented defaults before first customization.
func (service *Service) GetPlatformSettings(ctx context.Context, tenantID string) (domain.PlatformSettings, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.PlatformSettings{}, ErrValidation
	}
	settings, err := service.repository.GetPlatformSettings(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return defaultPlatformSettings(tenantID), nil
	}
	return settings, err
}

// UpdatePlatformSettings validates and saves one tenant's platform settings.
func (service *Service) UpdatePlatformSettings(ctx context.Context, input PlatformSettingsUpdateInput) (domain.PlatformSettings, error) {
	input = normalizePlatformSettings(input)
	if !validPlatformSettings(input) {
		return domain.PlatformSettings{}, ErrValidation
	}
	settingsID, err := service.ids.New(service.clock.Now().UTC())
	if err != nil {
		return domain.PlatformSettings{}, err
	}
	return service.repository.SavePlatformSettings(ctx, input, settingsID, service.clock.Now().UTC())
}

// GetNotificationSettings returns saved notification settings or documented defaults.
func (service *Service) GetNotificationSettings(ctx context.Context, tenantID string) (domain.NotificationSettings, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.NotificationSettings{}, ErrValidation
	}
	settings, err := service.repository.GetNotificationSettings(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return defaultNotificationSettings(tenantID), nil
	}
	return settings, err
}

// UpdateNotificationSettings validates and saves only in-app and email channel preferences.
func (service *Service) UpdateNotificationSettings(ctx context.Context, input NotificationSettingsUpdateInput) (domain.NotificationSettings, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OperatorID == "" || input.Version == 0 || !validReminderFrequency(input.ReminderFrequency) {
		return domain.NotificationSettings{}, ErrValidation
	}
	settingsID, err := service.ids.New(service.clock.Now().UTC())
	if err != nil {
		return domain.NotificationSettings{}, err
	}
	return service.repository.SaveNotificationSettings(ctx, input, settingsID, service.clock.Now().UTC())
}

func defaultPlatformSettings(tenantID string) domain.PlatformSettings {
	return domain.PlatformSettings{TenantID: tenantID, OrganizationName: "基础能力平台", OrganizationAlias: "基础平台", Timezone: "Asia/Shanghai", Version: 1}
}

func defaultNotificationSettings(tenantID string) domain.NotificationSettings {
	return domain.NotificationSettings{TenantID: tenantID, InboxEnabled: true, EmailEnabled: true, ReminderFrequency: domain.ReminderFrequencyDaily, Version: 1}
}

func normalizePlatformSettings(input PlatformSettingsUpdateInput) PlatformSettingsUpdateInput {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	input.OrganizationName = strings.TrimSpace(input.OrganizationName)
	input.OrganizationAlias = strings.TrimSpace(input.OrganizationAlias)
	input.Timezone = strings.TrimSpace(input.Timezone)
	input.Qualification = strings.TrimSpace(input.Qualification)
	return input
}

func validPlatformSettings(input PlatformSettingsUpdateInput) bool {
	return input.TenantID != "" && input.OperatorID != "" && input.Version > 0 &&
		input.OrganizationName != "" && len(input.OrganizationName) <= 128 &&
		input.OrganizationAlias != "" && len(input.OrganizationAlias) <= 64 &&
		input.Timezone != "" && len(input.Timezone) <= 64 && len(input.Qualification) <= 500
}

func validReminderFrequency(value domain.ReminderFrequency) bool {
	switch value {
	case domain.ReminderFrequencyDaily, domain.ReminderFrequencyEveryFourHours, domain.ReminderFrequencyOnce:
		return true
	default:
		return false
	}
}
