// Package infrastructure contains GORM-backed persistence for login security.
package infrastructure

import "time"

// loginPolicyModel maps the tenant-specific login policy maintained through manual SQL migrations.
type loginPolicyModel struct {
	TenantID                  string    `gorm:"column:tenant_id;primaryKey"`
	MaxFailedAttempts         uint      `gorm:"column:max_failed_attempts"`
	LockoutDurationSeconds    uint      `gorm:"column:lockout_duration_seconds"`
	FailureResetWindowSeconds uint      `gorm:"column:failure_reset_window_seconds"`
	IdleTimeoutSeconds        uint      `gorm:"column:idle_timeout_seconds"`
	Version                   uint64    `gorm:"column:version"`
	CreatedAt                 time.Time `gorm:"column:created_at"`
	CreatedBy                 *string   `gorm:"column:created_by"`
	UpdatedAt                 time.Time `gorm:"column:updated_at"`
	UpdatedBy                 *string   `gorm:"column:updated_by"`
}

func (loginPolicyModel) TableName() string { return "sec_login_policy" }

// loginAttemptModel stores non-secret login failures. ClearedAt preserves historical evidence while
// excluding a failure from the active lockout window.
type loginAttemptModel struct {
	ID            uint64     `gorm:"column:id;primaryKey"`
	OccurredAt    time.Time  `gorm:"column:occurred_at"`
	TenantID      string     `gorm:"column:tenant_id"`
	AccountID     string     `gorm:"column:account_id"`
	Username      string     `gorm:"column:username_snapshot"`
	IPAddress     []byte     `gorm:"column:ip_address"`
	UserAgent     string     `gorm:"column:user_agent"`
	Result        string     `gorm:"column:result"`
	FailureReason string     `gorm:"column:failure_reason"`
	ClearedAt     *time.Time `gorm:"column:cleared_at"`
}

func (loginAttemptModel) TableName() string { return "sec_login_attempt" }
