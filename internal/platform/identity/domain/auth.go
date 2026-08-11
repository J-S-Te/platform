// Package domain contains identity concepts without transport or database dependencies.
package domain

import "time"

// LoginAccount combines the identity and credential state required by a password-login use case.
type LoginAccount struct {
	TenantID         string
	TenantName       string
	TenantCode       string
	TenantStatus     string
	UserID           string
	UserName         string
	UserStatus       string
	AccountID        string
	AccountName      string
	AccountStatus    string
	LockedUntil      *time.Time
	PasswordHash     []byte
	HashAlgorithm    string
	AlgorithmParams  []byte
	CredentialStatus string
	CredentialExpiry *time.Time
}

// Session is the persisted browser-session state used to build the API response and cookie.
type Session struct {
	ID        string
	TenantID  string
	AccountID string
	CreatedAt time.Time
	ExpiresAt time.Time
	IPAddress []byte
	UserAgent string
}

// ReferenceName is the compact API representation for a tenant, user, account or role.
type ReferenceName struct {
	ID   string
	Name string
	Code string
}

// Principal is the authenticated server-side identity returned by session verification.
type Principal struct {
	SessionID       string
	Tenant          ReferenceName
	User            ReferenceName
	Account         ReferenceName
	Roles           []ReferenceName
	PermissionCodes []string
}
