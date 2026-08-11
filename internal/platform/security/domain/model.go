// Package domain defines the login-security entities shared by the security module.
package domain

import "time"

// LoginPolicy 是单租户密码登录防护策略；Version 用于管理端乐观锁，避免并发策略覆盖。
type LoginPolicy struct {
	TenantID                  string
	MaxFailedAttempts         uint
	LockoutDurationSeconds    uint
	FailureResetWindowSeconds uint
	IdleTimeoutSeconds        uint
	Version                   uint64
	UpdatedAt                 time.Time
}

// LockedAccount 是安全管理员可见的最小账号投影，不包含凭据、会话令牌或失败请求正文。
type LockedAccount struct {
	AccountID    string
	AccountName  string
	UserID       string
	UserName     string
	LockedUntil  *time.Time
	LastFailedAt *time.Time
	FailureCount uint
}
