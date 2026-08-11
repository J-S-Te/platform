// Package application coordinates notification template, audience resolution and inbox use cases.
package application

import (
	"context"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

// IdentifierGenerator supplies sortable ULIDs for notification aggregates.
type IdentifierGenerator interface {
	New(time.Time) (string, error)
}

// Clock makes notification timestamps deterministic in tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the UTC production clock.
type SystemClock struct{}

// Now returns the current UTC wall-clock time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// InboxPolicy is the deliberately small contract used to read the existing tenant notification
// setting. The notification module does not own, duplicate or import the settings aggregate.
type InboxPolicy interface {
	InboxEnabled(ctx context.Context, tenantID string) (bool, error)
}

// RecipientResolver resolves business audience targets to active user IDs. Implementations must
// preserve tenant isolation and must not return disabled users or inactive memberships.
type RecipientResolver interface {
	ResolveRecipients(ctx context.Context, tenantID string, targets []domain.RecipientTarget, at time.Time) ([]string, error)
}

// NotificationCreator is the public application contract for audit alerts and other modules.
// Callers provide a template code and audience references; they never write notification tables.
type NotificationCreator interface {
	Create(ctx context.Context, input CreateInput) (CreateResult, error)
}

// PageRequest is a bounded inbox/template list query.
type PageRequest struct {
	Page     int
	PageSize int
}

// PageResult is the common paginated result returned by notification queries.
type PageResult[T any] struct {
	Items    []T
	Page     int
	PageSize int
	Total    int64
}

// CreateTemplateInput creates an enabled or disabled template with its first immutable version.
type CreateTemplateInput struct {
	TenantID      string
	OperatorID    string
	Code          string
	Name          string
	Status        domain.TemplateStatus
	TitleTemplate string
	BodyTemplate  string
	Variables     []domain.VariableDefinition
}

// CreateTemplateVersionInput appends and publishes a new immutable version for a template.
type CreateTemplateVersionInput struct {
	TenantID      string
	OperatorID    string
	TemplateID    string
	TitleTemplate string
	BodyTemplate  string
	Variables     []domain.VariableDefinition
}

// ChangeTemplateStatusInput enables or disables a template under optimistic locking.
type ChangeTemplateStatusInput struct {
	TenantID   string
	OperatorID string
	TemplateID string
	Status     domain.TemplateStatus
	Version    uint64
}

// CreateInput describes one business event rendered into an in-app notification. Variable values
// are plain text only and are escaped before storage. TargetURL is a relative application path;
// opening it must always perform authorization again.
type CreateInput struct {
	TenantID       string
	OperatorID     string
	TemplateCode   string
	Category       string
	Variables      map[string]string
	Recipients     []domain.RecipientTarget
	TargetURL      string
	ReferenceType  string
	ReferenceID    string
	IdempotencyKey string
}

// CreateResult reports the durable message and recipient delivery IDs. Suppressed is true when
// the tenant's existing inbox switch is disabled, in which case no message or delivery is stored.
type CreateResult struct {
	MessageID   string
	DeliveryIDs []string
	Replayed    bool
	Suppressed  bool
}

// RetryResult reports a bounded retry pass. In-app delivery is a database state transition only;
// this contract intentionally has no email, SMS or Webhook behavior.
type RetryResult struct {
	Claimed   int
	Delivered int
	Failed    int
}

// MessageCreation is the repository's idempotent message persistence result.
type MessageCreation struct {
	Message    domain.Message
	Deliveries []domain.Delivery
	Replayed   bool
}

// Repository owns all database mutations. Each method that changes more than one row is expected
// to run inside a GORM transaction in the infrastructure adapter.
type Repository interface {
	CreateTemplate(ctx context.Context, template domain.Template, version domain.TemplateVersion) (domain.Template, domain.TemplateVersion, error)
	AppendTemplateVersion(ctx context.Context, tenantID, templateID, operatorID string, versionID string, titleTemplate, bodyTemplate string, variables []domain.VariableDefinition, now time.Time) (domain.Template, domain.TemplateVersion, error)
	ChangeTemplateStatus(ctx context.Context, input ChangeTemplateStatusInput, now time.Time) (domain.Template, error)
	ListTemplates(ctx context.Context, tenantID string, page PageRequest) (PageResult[domain.Template], error)
	GetActiveTemplateByCode(ctx context.Context, tenantID, code string) (domain.Template, domain.TemplateVersion, error)

	CreateMessage(ctx context.Context, message domain.Message, deliveries []domain.Delivery) (MessageCreation, error)
	CompleteDelivery(ctx context.Context, tenantID, deliveryID string, now time.Time) (domain.Delivery, error)
	FailDelivery(ctx context.Context, tenantID, deliveryID, safeReason string, nextRetryAt time.Time, now time.Time) error
	ClaimFailedDeliveries(ctx context.Context, tenantID string, limit int, leaseUntil, now time.Time) ([]domain.Delivery, error)
	ListDeliveries(ctx context.Context, tenantID string, status domain.DeliveryStatus, page PageRequest) (PageResult[domain.Delivery], error)

	ListInbox(ctx context.Context, tenantID, userID string, page PageRequest) (PageResult[domain.InboxItem], error)
	GetInboxItem(ctx context.Context, tenantID, userID, deliveryID string) (domain.InboxItem, error)
	CountUnread(ctx context.Context, tenantID, userID string) (int64, error)
	MarkRead(ctx context.Context, tenantID, userID, deliveryID string, now time.Time) (domain.InboxItem, error)
	MarkAllRead(ctx context.Context, tenantID, userID string, now time.Time) (int64, error)
}
