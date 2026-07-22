// Package infrastructure provides GORM-backed authorization persistence.
package infrastructure

import "time"

type applicationModel struct {
	ID     string `gorm:"column:id"`
	Code   string `gorm:"column:code"`
	Name   string `gorm:"column:name"`
	Status string `gorm:"column:status"`
}

func (applicationModel) TableName() string { return "platform_application" }

type resourceModel struct {
	ID            string    `gorm:"column:id"`
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	Code          string    `gorm:"column:code"`
	Name          string    `gorm:"column:name"`
	ResourceType  string    `gorm:"column:resource_type"`
	Status        string    `gorm:"column:status"`
	Version       uint64    `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	CreatedBy     *string   `gorm:"column:created_by"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
	UpdatedBy     *string   `gorm:"column:updated_by"`
}

func (resourceModel) TableName() string { return "authz_resource" }

type permissionModel struct {
	ID            string    `gorm:"column:id"`
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	ResourceID    string    `gorm:"column:resource_id"`
	Code          string    `gorm:"column:code"`
	Action        string    `gorm:"column:action"`
	Name          string    `gorm:"column:name"`
	Status        string    `gorm:"column:status"`
	Version       uint64    `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	CreatedBy     *string   `gorm:"column:created_by"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
	UpdatedBy     *string   `gorm:"column:updated_by"`
}

func (permissionModel) TableName() string { return "authz_permission" }

type roleModel struct {
	ID            string    `gorm:"column:id"`
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	Code          string    `gorm:"column:code"`
	Name          string    `gorm:"column:name"`
	RoleType      string    `gorm:"column:role_type"`
	Description   *string   `gorm:"column:description"`
	BuiltIn       bool      `gorm:"column:built_in"`
	Status        string    `gorm:"column:status"`
	Version       uint64    `gorm:"column:version"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	CreatedBy     *string   `gorm:"column:created_by"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
	UpdatedBy     *string   `gorm:"column:updated_by"`
}

func (roleModel) TableName() string { return "authz_role" }

type rolePermissionModel struct {
	RoleID       string    `gorm:"column:role_id"`
	PermissionID string    `gorm:"column:permission_id"`
	Effect       string    `gorm:"column:effect"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	CreatedBy    *string   `gorm:"column:created_by"`
}

func (rolePermissionModel) TableName() string { return "authz_role_permission" }

type roleBindingModel struct {
	ID            string     `gorm:"column:id"`
	TenantID      string     `gorm:"column:tenant_id"`
	ApplicationID string     `gorm:"column:application_id"`
	RoleID        string     `gorm:"column:role_id"`
	SubjectType   string     `gorm:"column:subject_type"`
	SubjectID     string     `gorm:"column:subject_id"`
	ScopeType     string     `gorm:"column:scope_type"`
	ScopeID       string     `gorm:"column:scope_id"`
	ValidUntil    *time.Time `gorm:"column:valid_until"`
	Status        string     `gorm:"column:status"`
	Version       uint64     `gorm:"column:version"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	CreatedBy     *string    `gorm:"column:created_by"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
	UpdatedBy     *string    `gorm:"column:updated_by"`
}

func (roleBindingModel) TableName() string { return "authz_role_binding" }

type policyRevisionModel struct {
	TenantID      string    `gorm:"column:tenant_id"`
	ApplicationID string    `gorm:"column:application_id"`
	Revision      uint64    `gorm:"column:revision"`
	ChangedAt     time.Time `gorm:"column:changed_at"`
	ChangeReason  string    `gorm:"column:change_reason"`
}

func (policyRevisionModel) TableName() string { return "authz_policy_revision" }
