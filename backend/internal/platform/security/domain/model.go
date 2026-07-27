// Package domain defines the login-security entities shared by the security module.
package domain

import "time"

// LoginPolicy controls password login failure handling for one tenant.
type LoginPolicy struct {
	TenantID                  string
	MaxFailedAttempts         uint
	LockoutDurationSeconds    uint
	FailureResetWindowSeconds uint
	IdleTimeoutSeconds        uint
	Version                   uint64
	UpdatedAt                 time.Time
}

// LockedAccount is the minimal account information exposed to security administrators.
type LockedAccount struct {
	AccountID    string
	AccountName  string
	UserID       string
	UserName     string
	LockedUntil  *time.Time
	LastFailedAt *time.Time
	FailureCount uint
}
