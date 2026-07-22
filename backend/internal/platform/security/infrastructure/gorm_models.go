// Package infrastructure contains GORM-backed persistence for login security.
package infrastructure

import "time"

// loginPolicyModel maps the tenant-specific login policy maintained through manual SQL migrations.
type loginPolicyModel struct {
	TenantID                  string    `gorm:"column:tenant_id;primaryKey"`
	MaxFailedAttempts         uint      `gorm:"column:max_failed_attempts"`
	LockoutDurationSeconds    uint      `gorm:"column:lockout_duration_seconds"`
	FailureResetWindowSeconds uint      `gorm:"column:failure_reset_window_seconds"`
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
	RiskScore     uint       `gorm:"column:risk_score"`
	ClearedAt     *time.Time `gorm:"column:cleared_at"`
}

func (loginAttemptModel) TableName() string { return "sec_login_attempt" }

// riskEventModel is the persisted representation of an operator-visible security risk event.
type riskEventModel struct {
	ID                string     `gorm:"column:id;primaryKey"`
	TenantID          string     `gorm:"column:tenant_id"`
	AccountID         *string    `gorm:"column:account_id"`
	EventType         string     `gorm:"column:event_type"`
	SubjectType       string     `gorm:"column:subject_type"`
	SubjectID         string     `gorm:"column:subject_id"`
	RiskLevel         string     `gorm:"column:risk_level"`
	RiskScore         uint       `gorm:"column:risk_score"`
	SourceIP          []byte     `gorm:"column:source_ip"`
	DetectionRule     string     `gorm:"column:detection_rule"`
	Status            string     `gorm:"column:status"`
	OccurredAt        time.Time  `gorm:"column:occurred_at"`
	ResolvedAt        *time.Time `gorm:"column:resolved_at"`
	ResolvedBy        *string    `gorm:"column:resolved_by"`
	ResolutionComment *string    `gorm:"column:resolution_comment"`
	Metadata          []byte     `gorm:"column:metadata"`
	Version           uint64     `gorm:"column:version"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

func (riskEventModel) TableName() string { return "sec_risk_event" }
