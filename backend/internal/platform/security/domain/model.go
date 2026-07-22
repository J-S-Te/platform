// Package domain defines the login-security entities shared by the security module.
package domain

import "time"

const (
	// RiskEventStatusOpen means that a risk event still needs a security operator decision.
	RiskEventStatusOpen = "OPEN"
	// RiskEventStatusResolved means an operator has completed the risk-event disposition.
	RiskEventStatusResolved = "RESOLVED"
)

// LoginPolicy controls password login failure handling for one tenant.
type LoginPolicy struct {
	TenantID                  string
	MaxFailedAttempts         uint
	LockoutDurationSeconds    uint
	FailureResetWindowSeconds uint
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

// RiskEvent is a tenant-scoped security event. Metadata must never contain passwords, cookies,
// authorization headers, or other authentication secrets.
type RiskEvent struct {
	RiskEventID       string
	TenantID          string
	AccountID         *string
	AccountName       *string
	EventType         string
	SubjectType       string
	SubjectID         string
	RiskLevel         string
	RiskScore         uint
	SourceIP          *string
	DetectionRule     string
	Status            string
	OccurredAt        time.Time
	ResolvedAt        *time.Time
	ResolvedBy        *string
	ResolutionComment *string
	Metadata          map[string]any
	Version           uint64
}
