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
