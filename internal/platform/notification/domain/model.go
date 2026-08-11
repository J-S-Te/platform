// Package domain defines tenant-isolated in-app notification aggregates.
package domain

import "time"

// TemplateStatus controls whether the current published version may be used to create messages.
type TemplateStatus string

const (
	TemplateStatusActive   TemplateStatus = "ACTIVE"
	TemplateStatusDisabled TemplateStatus = "DISABLED"
)

// TemplateVersionStatus describes the immutable lifecycle of a template version.
type TemplateVersionStatus string

const (
	TemplateVersionPublished TemplateVersionStatus = "PUBLISHED"
)

// RecipientType declares how an intended notification audience is resolved.
type RecipientType string

const (
	RecipientTypeUser         RecipientType = "USER"
	RecipientTypeRole         RecipientType = "ROLE"
	RecipientTypeOrganization RecipientType = "ORGANIZATION"
)

// DeliveryStatus records the in-app inbox delivery lifecycle.
type DeliveryStatus string

const (
	DeliveryStatusPending    DeliveryStatus = "PENDING"
	DeliveryStatusProcessing DeliveryStatus = "PROCESSING"
	DeliveryStatusDelivered  DeliveryStatus = "DELIVERED"
	DeliveryStatusFailed     DeliveryStatus = "FAILED"
)

// VariableDefinition declares a permitted plain-text template variable. Values are escaped before
// persistence so clients must render notification content as plain text rather than executable HTML.
type VariableDefinition struct {
	Name      string `json:"name"`
	Required  bool   `json:"required"`
	MaxLength int    `json:"max_length"`
}

// Template is the mutable template aggregate. Its published versions remain immutable.
type Template struct {
	ID             string
	TenantID       string
	Code           string
	Name           string
	Status         TemplateStatus
	CurrentVersion uint64
	Version        uint64
	CreatedAt      time.Time
	CreatedBy      string
	UpdatedAt      time.Time
	UpdatedBy      string
}

// TemplateVersion holds one immutable, published rendering definition.
type TemplateVersion struct {
	ID            string
	TemplateID    string
	TenantID      string
	Version       uint64
	Status        TemplateVersionStatus
	TitleTemplate string
	BodyTemplate  string
	Variables     []VariableDefinition
	PublishedAt   time.Time
	CreatedAt     time.Time
	CreatedBy     string
}

// RecipientTarget is a business-facing audience reference. It is resolved to active users before
// a notification is persisted; notification records never grant access to their linked resource.
type RecipientTarget struct {
	Type RecipientType
	ID   string
}

// Message is the rendered business notification shared by one or more user deliveries.
type Message struct {
	ID                string
	TenantID          string
	TemplateID        string
	TemplateVersionID string
	Category          string
	Title             string
	Content           string
	TargetURL         string
	ReferenceType     string
	ReferenceID       string
	IdempotencyKey    string
	CreatedAt         time.Time
	CreatedBy         string
}

// Delivery is one recipient's inbox item and delivery retry state.
type Delivery struct {
	ID              string
	TenantID        string
	MessageID       string
	RecipientUserID string
	Status          DeliveryStatus
	AttemptCount    uint
	LastError       string
	NextRetryAt     *time.Time
	LockedUntil     *time.Time
	DeliveredAt     *time.Time
	ReadAt          *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// InboxItem is the recipient-safe inbox projection. Linked targets require authorization again
// when a client navigates to them.
type InboxItem struct {
	DeliveryID    string
	MessageID     string
	Category      string
	Title         string
	Content       string
	TargetURL     string
	ReferenceType string
	ReferenceID   string
	DeliveredAt   time.Time
	ReadAt        *time.Time
}
