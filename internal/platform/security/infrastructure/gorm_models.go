// Package infrastructure 提供 login security 的 GORM 持久化实现。
package infrastructure

import "time"

// loginPolicyModel 映射租户级登录策略表，该表结构由手工 SQL 迁移维护，不走 GORM 自动迁移。
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

// loginAttemptModel 存储不含机密的登录失败事实。ClearedAt 保留历史证据，
// 同时把该次失败排除出有效锁定窗口的计数。
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
