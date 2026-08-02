// Package domain contains the platform-owned external identity projection.
package domain

import "time"

const (
	// IdentityPendingActivation means the stable OIDC subject has been reserved, but the
	// platform has not completed its own credential or upstream-identity activation flow.
	IdentityPendingActivation = "PENDING_ACTIVATION"
	IdentityActive            = "ACTIVE"
	IdentityDisabled          = "DISABLED"
)

// Identity is the non-secret external-customer identity projection. PlatformUserID is the
// authoritative OIDC subject (iam_user.id). AccountNo is an operator-facing reference only and
// is never accepted as a login credential.
type Identity struct {
	ID             string
	TenantID       string
	PlatformUserID string
	AccountNo      string
	EmailDigest    []byte
	MobileDigest   []byte
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RoleResult is the stable response returned by Portal role convergence operations.
type RoleResult struct {
	PlatformUserID  string `json:"platform_user_id"`
	ApplicationCode string `json:"application_code"`
	RoleCode        string `json:"role_code"`
	Status          string `json:"status"`
}
