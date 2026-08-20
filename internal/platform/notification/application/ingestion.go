package application

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

const (
	maximumIngestionRecipients = 500
	ingestionRetryDelay        = time.Minute
)

// Ingest accepts a cross-system notification without synchronously expanding deliveries.
func (service *Service) Ingest(ctx context.Context, input IngestInput) (domain.IngestionReceipt, error) {
	if err := validateIngestInput(input); err != nil {
		return domain.IngestionReceipt{}, err
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	receiptID, err := service.ids.New(now)
	if err != nil {
		return domain.IngestionReceipt{}, err
	}
	return service.repository.AcceptIngestion(ctx, strings.TrimSpace(input.TenantID), strings.TrimSpace(input.SourceApplication), strings.TrimSpace(input.SourceEnvironment), normalizeIngestionEvent(input.Event), receiptID, now)
}

func (service *Service) GetIngestionReceipt(ctx context.Context, tenantID, receiptID string) (domain.IngestionReceipt, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(receiptID) == "" {
		return domain.IngestionReceipt{}, ErrValidation
	}
	return service.repository.GetIngestionReceipt(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(receiptID))
}

// ProcessIngestionBatch projects accepted events to the user inbox. Callers supply the worker
// lease; events are independently idempotent through the source-event unique key.
func (service *Service) ProcessIngestionBatch(ctx context.Context, limit int, leaseUntil time.Time) (int, error) {
	if limit < 1 || limit > 200 || !leaseUntil.After(service.clock.Now()) {
		return 0, ErrValidation
	}
	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	receipts, err := service.repository.ClaimIngestionEvents(ctx, limit, leaseUntil.UTC(), now)
	if err != nil {
		return 0, err
	}
	for _, receipt := range receipts {
		tenantID, sourceApplication, sourceEnvironment, event, loadErr := service.repository.LoadIngestionEvent(ctx, receipt.ReceiptID)
		if loadErr != nil {
			_ = service.repository.RetryIngestion(ctx, receipt.ReceiptID, "NOTIFICATION_EVENT_LOAD_FAILED", "notification event could not be loaded", now.Add(ingestionRetryDelay), now)
			continue
		}
		messageID, processErr := service.projectIngestionEvent(ctx, tenantID, sourceApplication, sourceEnvironment, event, now)
		if processErr != nil {
			if errorsPermanent(processErr) {
				_ = service.repository.DeadLetterIngestion(ctx, receipt.ReceiptID, "NOTIFICATION_EVENT_INVALID", "notification event is not deliverable", now)
			} else {
				_ = service.repository.RetryIngestion(ctx, receipt.ReceiptID, "NOTIFICATION_EVENT_PROCESSING_FAILED", "notification event processing failed", now.Add(ingestionRetryDelay), now)
			}
			continue
		}
		if err := service.repository.CompleteIngestion(ctx, receipt.ReceiptID, messageID, now); err != nil {
			return len(receipts), err
		}
	}
	return len(receipts), nil
}

func (service *Service) projectIngestionEvent(ctx context.Context, tenantID, sourceApplication, sourceEnvironment string, event domain.IngestionEvent, now time.Time) (string, error) {
	enabled, err := service.policy.InboxEnabled(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", nil
	}
	targets := make([]domain.RecipientTarget, 0, len(event.Recipients))
	for _, userID := range event.Recipients {
		targets = append(targets, domain.RecipientTarget{Type: domain.RecipientTypeUser, ID: userID})
	}
	users, err := service.resolver.ResolveRecipients(ctx, tenantID, targets, now)
	if err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", ErrNoRecipients
	}
	messageID, err := service.ids.New(now)
	if err != nil {
		return "", err
	}
	message := domain.Message{ID: messageID, TenantID: tenantID, SourceApplication: sourceApplication, SourceEnvironment: sourceEnvironment, SourceEventID: event.EventID, EventType: event.EventType, NotificationScope: event.NotificationScope, Priority: event.Priority, Category: event.EventType, Title: event.Title, Content: event.Content, TargetURL: event.TargetURL, ReferenceType: event.ReferenceType, ReferenceID: event.ReferenceID, IdempotencyKey: canonicalIdempotencyKey(sourceApplication, sourceEnvironment, event.IdempotencyKey), OccurredAt: timePointer(event.OccurredAt), ExpiresAt: event.ExpiresAt, CreatedAt: now}
	deliveries := make([]domain.Delivery, 0, len(users))
	for _, userID := range users {
		deliveryID, idErr := service.ids.New(now)
		if idErr != nil {
			return "", idErr
		}
		deliveries = append(deliveries, domain.Delivery{ID: deliveryID, TenantID: tenantID, MessageID: messageID, RecipientUserID: userID, Status: domain.DeliveryStatusPending, CreatedAt: now, UpdatedAt: now})
	}
	created, err := service.repository.CreateMessage(ctx, message, deliveries)
	if err != nil {
		return "", err
	}
	if created.Replayed {
		return created.Message.ID, nil
	}
	for _, delivery := range created.Deliveries {
		if _, err := service.repository.CompleteDelivery(ctx, tenantID, delivery.ID, now); err != nil {
			return "", err
		}
	}
	return created.Message.ID, nil
}

func validateIngestInput(input IngestInput) error {
	event := input.Event
	if strings.TrimSpace(input.TenantID) == "" || !validSourceCode(input.SourceApplication, 128) || !validSourceCode(input.SourceEnvironment, 64) || !validSourceCode(event.EventID, 128) || !validSourceCode(event.EventType, 128) || !oneOf(strings.ToUpper(strings.TrimSpace(event.NotificationScope)), "CROSS_SYSTEM", "PLATFORM") || !oneOf(strings.ToUpper(strings.TrimSpace(event.Priority)), "LOW", "NORMAL", "HIGH", "CRITICAL") || strings.TrimSpace(event.Title) == "" || utf8.RuneCountInString(event.Title) > 500 || strings.TrimSpace(event.Content) == "" || utf8.RuneCountInString(event.Content) > 16*1024 || !validTargetURL(event.TargetURL) || !optionalCode(event.ReferenceType, 64) || utf8.RuneCountInString(event.ReferenceID) > 128 || strings.TrimSpace(event.IdempotencyKey) == "" || utf8.RuneCountInString(event.IdempotencyKey) > 128 || len(canonicalIdempotencyKey(input.SourceApplication, input.SourceEnvironment, event.IdempotencyKey)) > 128 || len(event.Recipients) == 0 || len(event.Recipients) > maximumIngestionRecipients || event.OccurredAt.IsZero() {
		return ErrValidation
	}
	for _, recipient := range event.Recipients {
		if strings.TrimSpace(recipient) == "" || utf8.RuneCountInString(recipient) > 26 {
			return ErrValidation
		}
	}
	return nil
}

func normalizeIngestionEvent(event domain.IngestionEvent) domain.IngestionEvent {
	event.EventID = strings.TrimSpace(event.EventID)
	event.EventType = strings.ToUpper(strings.TrimSpace(event.EventType))
	event.NotificationScope = strings.ToUpper(strings.TrimSpace(event.NotificationScope))
	event.Priority = strings.ToUpper(strings.TrimSpace(event.Priority))
	event.Title = strings.TrimSpace(event.Title)
	event.Content = strings.TrimSpace(event.Content)
	event.TargetURL = strings.TrimSpace(event.TargetURL)
	event.ReferenceType = normalizeCode(event.ReferenceType)
	event.ReferenceID = strings.TrimSpace(event.ReferenceID)
	event.IdempotencyKey = strings.TrimSpace(event.IdempotencyKey)
	event.Recipients = uniqueNonEmpty(event.Recipients)
	event.OccurredAt = event.OccurredAt.UTC().Truncate(time.Millisecond)
	if event.ExpiresAt != nil {
		value := event.ExpiresAt.UTC().Truncate(time.Millisecond)
		event.ExpiresAt = &value
	}
	return event
}

func canonicalIdempotencyKey(application, environment, key string) string {
	return strings.TrimSpace(application) + ":" + strings.TrimSpace(environment) + ":" + strings.TrimSpace(key)
}

func validSourceCode(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value != "" && utf8.RuneCountInString(value) <= maximum && codePattern.MatchString(normalizeCode(value))
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC().Truncate(time.Millisecond)
	return &value
}

func errorsPermanent(err error) bool {
	return err == ErrValidation || err == ErrNoRecipients || err == ErrNotFound || err == ErrConflict
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
