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
