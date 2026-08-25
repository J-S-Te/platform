package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ingestionModel struct {
	ID, TenantID, SourceApplication, SourceEnvironment, SourceEventID string
	Payload                                                           []byte
	Status                                                            string
	AttemptCount                                                      uint
	NextRetryAt, LockedUntil                                          *time.Time
	LastErrorCode, LastErrorMessage                                   string
	MessageID                                                         *string
	ReceivedAt, CreatedAt, UpdatedAt                                  time.Time
	ProcessedAt                                                       *time.Time
}

func (ingestionModel) TableName() string { return "notification_event_inbox" }

func (repository *Repository) AcceptIngestion(ctx context.Context, tenantID, sourceApplication, sourceEnvironment string, event domain.IngestionEvent, receiptID string, now time.Time) (domain.IngestionReceipt, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return domain.IngestionReceipt{}, fmt.Errorf("marshal notification ingestion payload: %w", err)
	}
	row := ingestionModel{ID: receiptID, TenantID: tenantID, SourceApplication: sourceApplication, SourceEnvironment: sourceEnvironment, SourceEventID: event.EventID, Payload: payload, Status: string(domain.IngestionStatusAccepted), ReceivedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := repository.database.WithContext(ctx).Create(&row).Error; err == nil {
		return ingestionToReceipt(row, false), nil
	} else if !isDuplicate(err) {
		return domain.IngestionReceipt{}, fmt.Errorf("accept notification ingestion: %w", err)
	}
	var existing ingestionModel
	if err := repository.database.WithContext(ctx).Where("tenant_id = ? AND source_application = ? AND source_environment = ? AND source_event_id = ?", tenantID, sourceApplication, sourceEnvironment, event.EventID).Take(&existing).Error; err != nil {
		return domain.IngestionReceipt{}, mapError(err)
	}
	return ingestionToReceipt(existing, true), nil
}

func (repository *Repository) GetIngestionReceipt(ctx context.Context, tenantID, sourceApplication, sourceEnvironment, receiptID string) (domain.IngestionReceipt, error) {
	var row ingestionModel
	if err := repository.database.WithContext(ctx).Where("id = ? AND tenant_id = ? AND source_application = ? AND source_environment = ?", receiptID, tenantID, sourceApplication, sourceEnvironment).Take(&row).Error; err != nil {
		return domain.IngestionReceipt{}, mapError(err)
	}
	return ingestionToReceipt(row, false), nil
}

func (repository *Repository) ClaimIngestionEvents(ctx context.Context, limit int, leaseUntil, now time.Time) ([]domain.IngestionReceipt, error) {
	var rows []ingestionModel
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status IN ? AND (next_retry_at IS NULL OR next_retry_at <= ?) AND (locked_until IS NULL OR locked_until < ?)", []string{string(domain.IngestionStatusAccepted), string(domain.IngestionStatusRetry)}, now, now).Order("received_at ASC,id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return fmt.Errorf("claim notification ingestion events: %w", err)
		}
		for index := range rows {
			result := tx.Model(&ingestionModel{}).Where("id = ? AND status IN ?", rows[index].ID, []string{string(domain.IngestionStatusAccepted), string(domain.IngestionStatusRetry)}).Updates(map[string]any{"status": string(domain.IngestionStatusProcessing), "locked_until": leaseUntil, "updated_at": now})
			if result.Error != nil {
				return fmt.Errorf("lock notification ingestion event: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return application.ErrConflict
			}
			rows[index].Status = string(domain.IngestionStatusProcessing)
			rows[index].LockedUntil = &leaseUntil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]domain.IngestionReceipt, 0, len(rows))
	for _, row := range rows {
		result = append(result, ingestionToReceipt(row, false))
	}
	return result, nil
}

func (repository *Repository) LoadIngestionEvent(ctx context.Context, receiptID string) (string, string, string, domain.IngestionEvent, error) {
	var row ingestionModel
	if err := repository.database.WithContext(ctx).Where("id = ? AND status = ?", receiptID, domain.IngestionStatusProcessing).Take(&row).Error; err != nil {
		return "", "", "", domain.IngestionEvent{}, mapError(err)
	}
	var event domain.IngestionEvent
	if err := json.Unmarshal(row.Payload, &event); err != nil {
		return "", "", "", domain.IngestionEvent{}, fmt.Errorf("decode notification ingestion payload: %w", err)
	}
	return row.TenantID, row.SourceApplication, row.SourceEnvironment, event, nil
}

func (repository *Repository) CompleteIngestion(ctx context.Context, receiptID, messageID string, now time.Time) error {
	values := map[string]any{"status": string(domain.IngestionStatusCompleted), "message_id": optional(messageID), "processed_at": now, "locked_until": nil, "last_error_code": "", "last_error_message": "", "updated_at": now}
	result := repository.database.WithContext(ctx).Model(&ingestionModel{}).Where("id = ? AND status = ?", receiptID, domain.IngestionStatusProcessing).Updates(values)
	if result.Error != nil {
		return fmt.Errorf("complete notification ingestion: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (repository *Repository) RetryIngestion(ctx context.Context, receiptID, errorCode, safeMessage string, nextRetryAt, now time.Time) error {
	result := repository.database.WithContext(ctx).Model(&ingestionModel{}).Where("id = ? AND status = ?", receiptID, domain.IngestionStatusProcessing).Updates(map[string]any{"status": string(domain.IngestionStatusRetry), "attempt_count": gorm.Expr("attempt_count + 1"), "next_retry_at": nextRetryAt, "locked_until": nil, "last_error_code": strings.TrimSpace(errorCode), "last_error_message": truncateNotificationError(safeMessage), "updated_at": now})
	if result.Error != nil {
		return fmt.Errorf("retry notification ingestion: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func (repository *Repository) DeadLetterIngestion(ctx context.Context, receiptID, errorCode, safeMessage string, now time.Time) error {
	result := repository.database.WithContext(ctx).Model(&ingestionModel{}).Where("id = ? AND status = ?", receiptID, domain.IngestionStatusProcessing).Updates(map[string]any{"status": string(domain.IngestionStatusDead), "processed_at": now, "locked_until": nil, "last_error_code": strings.TrimSpace(errorCode), "last_error_message": truncateNotificationError(safeMessage), "updated_at": now})
	if result.Error != nil {
		return fmt.Errorf("dead-letter notification ingestion: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return application.ErrConflict
	}
	return nil
}

func ingestionToReceipt(row ingestionModel, duplicate bool) domain.IngestionReceipt {
	processedAt := time.Time{}
	if row.ProcessedAt != nil {
		processedAt = *row.ProcessedAt
	}
	return domain.IngestionReceipt{ReceiptID: row.ID, EventID: row.SourceEventID, MessageID: value(row.MessageID), Status: domain.IngestionStatus(row.Status), Duplicate: duplicate, ErrorCode: row.LastErrorCode, ReceivedAt: row.ReceivedAt, ProcessedAt: processedAt}
}

func truncateNotificationError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[:500]
	}
	return value
}
