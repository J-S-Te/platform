// Package domain defines the authorization aggregates without infrastructure annotations.
package domain

import "time"

const (
	StatusActive   = "ACTIVE"
	StatusDisabled = "DISABLED"
)

// Reference identifies a related aggregate in an API-safe form.
type Reference struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Code string `json:"code,omitempty"`
}

// Resource is an application-owned protected resource.
type Resource struct {
	ID              string
	ApplicationCode string
	Code            string
	Name            string
	ResourceType    string
	Version         uint64
}

// Permission is an action permitted on a resource.
type Permission struct {
	ID       string
	Code     string
	Name     string
	Resource Reference
	Action   string
	Version  uint64
}

// Role is a tenant-scoped collection of allow permissions.
type Role struct {
	ID          string
	Code        string
	Name        string
	Description *string
	Status      string
	BuiltIn     bool
	Permissions []Reference
	Version     uint64
}

// RoleBinding assigns a role to a subject within a scope.
type RoleBinding struct {
	ID          string
	Role        Reference
	SubjectType string
	Subject     Reference
	ScopeType   string
	ScopeID     *string
	Status      string
	ExpiresAt   *time.Time
	Version     uint64
}

// Decision is the outcome of a permission evaluation.
type Decision struct {
	Allowed        bool
	PermissionCode string
	PolicyVersion  uint64
	ReasonCode     string
}

// AccessSource describes the active role-binding path that grants a role or permission.
// Scope is returned as metadata: callers must still evaluate a concrete resource or organization
// context before treating a scoped permission as allowed.
type AccessSource struct {
	BindingID   string
	SubjectType string
	Subject     Reference
	ScopeType   string
	ScopeID     *string
}

// EffectiveRole is a currently active role with every binding source that makes it effective.
type EffectiveRole struct {
	Role    Reference
	Sources []AccessSource
}

// EffectivePermission is a currently active permission with role-binding provenance.
type EffectivePermission struct {
	Permission Permission
	Sources    []AccessSource
}

// EffectiveAccessPreview exposes the same active binding paths used by authorization checks for
// a selected account. It is an administrative explanation surface, not a client-side decision API.
type EffectiveAccessPreview struct {
	User                      Reference
	Account                   Reference
	LoginEligible             bool
	PolicyVersion             uint64
	GeneratedAt               time.Time
	Roles                     []EffectiveRole
	Permissions               []EffectivePermission
	ExternalIdentityProviders []Reference
}

// RoleBindingImpactPreview describes the active users that would immediately receive a proposed
// binding. User samples are capped by the repository while TotalAffectedUsers remains exact.
type RoleBindingImpactPreview struct {
	Role               Reference
	Permissions        []Reference
	SubjectType        string
	Subject            Reference
	ScopeType          string
	ScopeID            *string
	ExpiresAt          *time.Time
	TotalAffectedUsers int64
	Users              []Reference
	Truncated          bool
	GeneratedAt        time.Time
}
