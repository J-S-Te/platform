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
	ReminderFrequencyDaily          ReminderFrequency = "DAILY"
	ReminderFrequencyEveryFourHours ReminderFrequency = "EVERY_FOUR_HOURS"
	ReminderFrequencyOnce           ReminderFrequency = "ONCE"
)

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
