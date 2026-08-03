// Package application coordinates platform and notification settings use cases.
package application

import (
	"context"
	"errors"
	"net"
	"net/url"
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

// AccessSettingsUpdateInput replaces the typed public-access settings for one tenant.
type AccessSettingsUpdateInput struct {
	TenantID                  string
	OperatorID                string
	PublicOrigin              string
	AllowInsecureHTTPRedirect bool
	Version                   uint64
}

// AccessApplyInput carries the neutral runtime values the deployment agent needs to apply.
type AccessApplyInput struct {
	PublicOrigin              string
	AllowInsecureHTTPRedirect bool
}

// AccessApplier applies the access configuration to the local unified orchestration
// (writes override environment files and recreates affected containers).
type AccessApplier interface {
	ApplyAccess(context.Context, AccessApplyInput) error
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
	GetAccessSettings(ctx context.Context, tenantID string) (domain.AccessSettings, error)
	SaveAccessSettings(ctx context.Context, input AccessSettingsUpdateInput, settingsID string, now time.Time) (domain.AccessSettings, error)
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

// GetAccessSettings returns saved access settings or the local-only default.
func (service *Service) GetAccessSettings(ctx context.Context, tenantID string) (domain.AccessSettings, error) {
	if strings.TrimSpace(tenantID) == "" {
		return domain.AccessSettings{}, ErrValidation
	}
	settings, err := service.repository.GetAccessSettings(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return defaultAccessSettings(tenantID), nil
	}
	return settings, err
}

// UpdateAccessSettings validates and saves one tenant's public-access settings.
func (service *Service) UpdateAccessSettings(ctx context.Context, input AccessSettingsUpdateInput) (domain.AccessSettings, error) {
	input = normalizeAccessSettings(input)
	if !validAccessSettings(input) {
		return domain.AccessSettings{}, ErrValidation
	}
	settingsID, err := service.ids.New(service.clock.Now().UTC())
	if err != nil {
		return domain.AccessSettings{}, err
	}
	return service.repository.SaveAccessSettings(ctx, input, settingsID, service.clock.Now().UTC())
}

// ApplyAccessSettings 只应用已经持久化的配置，不接受临时请求值。部署 Agent 会改写本地覆盖文件
// 并重建受影响容器，因此调用可能短暂中断统一前端和 API；生产/远程模式未注入 Agent 时必须失败。
func (service *Service) ApplyAccessSettings(ctx context.Context, tenantID string, applier AccessApplier) (domain.AccessSettings, error) {
	if strings.TrimSpace(tenantID) == "" || applier == nil {
		return domain.AccessSettings{}, ErrValidation
	}
	settings, err := service.GetAccessSettings(ctx, tenantID)
	if err != nil {
		return domain.AccessSettings{}, err
	}
	if err := applier.ApplyAccess(ctx, AccessApplyInput{
		PublicOrigin:              settings.PublicOrigin,
		AllowInsecureHTTPRedirect: settings.AllowInsecureHTTPRedirect,
	}); err != nil {
		return domain.AccessSettings{}, err
	}
	return settings, nil
}

func defaultAccessSettings(tenantID string) domain.AccessSettings {
	return domain.AccessSettings{TenantID: tenantID, Version: 1}
}

// normalizeAccessSettings 规范化 origin。非回环 HTTP 对外地址会强制启用不安全回调开关，
// 这是对当前局域网开发模式的显式兼容，不代表 HTTP 回调具备与 HTTPS 相同的安全性。
func normalizeAccessSettings(input AccessSettingsUpdateInput) AccessSettingsUpdateInput {
	input.PublicOrigin = strings.TrimRight(strings.TrimSpace(input.PublicOrigin), "/")
	if isHTTPPublicOrigin(input.PublicOrigin) {
		input.AllowInsecureHTTPRedirect = true
	}
	return input
}

func validAccessSettings(input AccessSettingsUpdateInput) bool {
	if strings.TrimSpace(input.TenantID) == "" || input.Version == 0 {
		return false
	}
	if input.PublicOrigin == "" {
		return true
	}
	parsed, err := url.Parse(input.PublicOrigin)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Path != "" {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	if isHTTPPublicOrigin(input.PublicOrigin) && !input.AllowInsecureHTTPRedirect {
		return false
	}
	return true
}

func isHTTPPublicOrigin(origin string) bool {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, "http") || parsed.Hostname() == "" {
		return false
	}
	host := parsed.Hostname()
	return !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback()
}
