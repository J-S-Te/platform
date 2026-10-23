// Package domain 定义租户范围内的平台与通知设置。
package domain

import "time"

// PlatformSettings 承载管理控制台展示的平台身份值。
type PlatformSettings struct {
	ID                string
	TenantID          string
	OrganizationName  string
	OrganizationAlias string
	Timezone          string
	Qualification     string
	Version           uint64
	UpdatedAt         time.Time
}

// ReminderFrequency 标识符合条件的提醒按什么频率聚合。
type ReminderFrequency string

const (
	// ReminderFrequencyImmediate 创建即可见（保持未设置租户的现状行为，也是默认值）。
	ReminderFrequencyImmediate ReminderFrequency = "IMMEDIATE"
	// ReminderFrequencyDaily 投递延迟到下一个每日释放点（09:00 UTC）可见。
	ReminderFrequencyDaily ReminderFrequency = "DAILY"
	// ReminderFrequencyWeekly 投递延迟到下一个每周释放点（周一 09:00 UTC）可见。
	ReminderFrequencyWeekly ReminderFrequency = "WEEKLY"
	// ReminderFrequencyNever 不进入站内信（通知创建被抑制）。
	ReminderFrequencyNever ReminderFrequency = "NEVER"
)

// Valid 报告该提醒频率是否在当前支持词表内。旧词表 EVERY_FOUR_HOURS/ONCE 从未具备
// 投递语义，已随迁移 000113 退役并归一化为 IMMEDIATE。
func (frequency ReminderFrequency) Valid() bool {
	switch frequency {
	case ReminderFrequencyImmediate, ReminderFrequencyDaily, ReminderFrequencyWeekly, ReminderFrequencyNever:
		return true
	default:
		return false
	}
}

// AccessSettings 配置本地统一编排的对外源地址与 OAuth HTTP 回调策略。
// PublicOrigin 为空表示仅本机可用（127.0.0.1 / localhost）。
type AccessSettings struct {
	ID                        string
	TenantID                  string
	PublicOrigin              string
	AllowInsecureHTTPRedirect bool
	Version                   uint64
	UpdatedAt                 time.Time
}

// IsPublic 报告该配置是否把统一前端暴露到回环地址之外。
func (settings AccessSettings) IsPublic() bool {
	return settings.PublicOrigin != ""
}

// NotificationSettings 配置平台当前支持的通知渠道。消息投递、SMTP 配置、短信、
// webhook 与模板有意不在本聚合范围内。
type NotificationSettings struct {
	ID                string
	TenantID          string
	InboxEnabled      bool
	EmailEnabled      bool
	ReminderFrequency ReminderFrequency
	Version           uint64
	UpdatedAt         time.Time
}
