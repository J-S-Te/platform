// Package domain defines tenant-scoped platform and notification settings.
package domain

import "time"

// PlatformSettings contains the platform identity values displayed by the management console.
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

// ReminderFrequency identifies how frequently eligible reminders are aggregated.
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

// AccessSettings configures the public origin and OAuth HTTP callback policy of the local
// unified orchestration. Empty PublicOrigin means local-only (127.0.0.1 / localhost).
type AccessSettings struct {
	ID                        string
	TenantID                  string
	PublicOrigin              string
	AllowInsecureHTTPRedirect bool
	Version                   uint64
	UpdatedAt                 time.Time
}

// IsPublic reports whether the configuration exposes the unified frontend beyond loopback.
func (settings AccessSettings) IsPublic() bool {
	return settings.PublicOrigin != ""
}

// NotificationSettings configures the notification channels currently supported by the platform.
// Message delivery, SMTP configuration, SMS, webhook and templates are intentionally out of scope.
type NotificationSettings struct {
	ID                string
	TenantID          string
	InboxEnabled      bool
	EmailEnabled      bool
	ReminderFrequency ReminderFrequency
	Version           uint64
	UpdatedAt         time.Time
}
