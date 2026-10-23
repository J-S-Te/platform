// Package application 协调平台设置与通知设置用例。
package application

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/settings/domain"
)

var (
	// ErrNotFound 表示仓储中没有已持久化的设置行。
	ErrNotFound = errors.New("settings not found")
	// ErrConflict 表示唯一性或生命周期冲突。
	ErrConflict = errors.New("settings conflict")
	// ErrVersionConflict 表示乐观锁版本已过期。
	ErrVersionConflict = errors.New("settings version conflict")
	// ErrValidation 表示应用层输入非法。
	ErrValidation = errors.New("invalid settings input")
)

// IdentifierGenerator 提供可排序的聚合标识符。
type IdentifierGenerator interface {
	New(at time.Time) (string, error)
}

// Clock 为确定性测试提供当前时间。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产环境时钟。
type SystemClock struct{}

// Now 返回当前墙钟时间。
func (SystemClock) Now() time.Time { return time.Now() }

// PlatformSettingsUpdateInput 整体替换单个租户的强类型平台设置。
type PlatformSettingsUpdateInput struct {
	TenantID          string
	OperatorID        string
	OrganizationName  string
	OrganizationAlias string
	Timezone          string
	Qualification     string
	Version           uint64
}

// AccessSettingsUpdateInput 整体替换单个租户的强类型对外访问设置。
type AccessSettingsUpdateInput struct {
	TenantID                  string
	OperatorID                string
	PublicOrigin              string
	AllowInsecureHTTPRedirect bool
	Version                   uint64
}

// AccessApplyInput 携带部署执行器应用配置所需的中立运行时值。
type AccessApplyInput struct {
	PublicOrigin              string
	AllowInsecureHTTPRedirect bool
}

// AccessApplier 把对外访问配置应用到本地统一编排（改写覆盖环境文件并重建受影响容器）。
type AccessApplier interface {
	ApplyAccess(context.Context, AccessApplyInput) error
}

// NotificationSettingsUpdateInput 整体替换单个租户的强类型通知设置。
type NotificationSettingsUpdateInput struct {
	TenantID          string
	OperatorID        string
	InboxEnabled      bool
	EmailEnabled      bool
	ReminderFrequency domain.ReminderFrequency
	Version           uint64
}

// Repository 持久化设置聚合。
type Repository interface {
	GetPlatformSettings(ctx context.Context, tenantID string) (domain.PlatformSettings, error)
	SavePlatformSettings(ctx context.Context, input PlatformSettingsUpdateInput, settingsID string, now time.Time) (domain.PlatformSettings, error)
	GetNotificationSettings(ctx context.Context, tenantID string) (domain.NotificationSettings, error)
	SaveNotificationSettings(ctx context.Context, input NotificationSettingsUpdateInput, settingsID string, now time.Time) (domain.NotificationSettings, error)
	GetAccessSettings(ctx context.Context, tenantID string) (domain.AccessSettings, error)
	SaveAccessSettings(ctx context.Context, input AccessSettingsUpdateInput, settingsID string, now time.Time) (domain.AccessSettings, error)
}

// Service 暴露设置应用用例。
type Service struct {
	repository Repository
	ids        IdentifierGenerator
	clock      Clock
}

// NewService 校验依赖并构建设置应用服务；任一依赖为 nil 即拒绝启动。
func NewService(repository Repository, ids IdentifierGenerator, clock Clock) (*Service, error) {
	if repository == nil || ids == nil || clock == nil {
		return nil, errors.New("settings service dependencies must not be nil")
	}
	return &Service{repository: repository, ids: ids, clock: clock}, nil
}

// GetPlatformSettings 返回已保存设置；租户首次自定义前返回有文档依据的安全默认值。
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

// UpdatePlatformSettings 校验并保存单个租户的平台设置。
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

// GetNotificationSettings 返回已保存的通知设置或文档化的默认值。
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

// UpdateNotificationSettings 校验并保存实际可用的通知渠道。在邮件提供方与投递 worker
// 就绪前有意拒绝启用邮件投递：持久化一个没有发送方的启用开关会制造虚假运营信号，
// 并让业务通知无声丢失。
func (service *Service) UpdateNotificationSettings(ctx context.Context, input NotificationSettingsUpdateInput) (domain.NotificationSettings, error) {
	input.TenantID = strings.TrimSpace(input.TenantID)
	input.OperatorID = strings.TrimSpace(input.OperatorID)
	if input.TenantID == "" || input.OperatorID == "" || input.Version == 0 || input.EmailEnabled || !input.ReminderFrequency.Valid() {
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
	return domain.NotificationSettings{TenantID: tenantID, InboxEnabled: true, EmailEnabled: false, ReminderFrequency: domain.ReminderFrequencyImmediate, Version: 1}
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

// GetAccessSettings 返回已保存的对外访问设置；未保存时返回仅本机默认值。
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

// UpdateAccessSettings 校验并保存单个租户的对外访问设置。
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
	if len(input.PublicOrigin) > 512 {
		return false
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
