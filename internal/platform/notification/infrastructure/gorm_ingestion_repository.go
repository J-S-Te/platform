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

// AcceptIngestion 幂等接收外部通知事件：首次写入返回新回执；同一
// (tenant, source_application, source_environment, source_event_id) 重复提交时
// 返回既有事件并标记 Duplicate，不产生第二条投递。
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

// GetIngestionReceipt 按 ID 查询接收回执，条件必须完整匹配租户与来源三元组。
func (repository *Repository) GetIngestionReceipt(ctx context.Context, tenantID, sourceApplication, sourceEnvironment, receiptID string) (domain.IngestionReceipt, error) {
	var row ingestionModel
	if err := repository.database.WithContext(ctx).Where("id = ? AND tenant_id = ? AND source_application = ? AND source_environment = ?", receiptID, tenantID, sourceApplication, sourceEnvironment).Take(&row).Error; err != nil {
		return domain.IngestionReceipt{}, mapError(err)
	}
	return ingestionToReceipt(row, false), nil
}

// ClaimIngestionEvents 以 SKIP LOCKED 事务认领一批可处理事件并写入租约。
// 租约过期的 PROCESSING 事件也会被接管，防止 worker 崩溃导致事件永久滞留。
func (repository *Repository) ClaimIngestionEvents(ctx context.Context, limit int, leaseUntil, now time.Time) ([]domain.IngestionReceipt, error) {
	var rows []ingestionModel
	// 可领取集合 = 待处理/待重试，或"租约已过期的 PROCESSING"。领取后进程崩溃会留下
	// PROCESSING + 过期 locked_until；若不接管，这些事件永远不再被处理，界面也没有入口。
	claimable := "((status IN ? AND (next_retry_at IS NULL OR next_retry_at <= ?)) OR (status = ? AND locked_until IS NOT NULL AND locked_until < ?))"
	claimableArgs := []any{[]string{string(domain.IngestionStatusAccepted), string(domain.IngestionStatusRetry)}, now, string(domain.IngestionStatusProcessing), now}
	err := repository.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where(claimable, claimableArgs...).Order("received_at ASC,id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return fmt.Errorf("claim notification ingestion events: %w", err)
		}
		for index := range rows {
			// 条件更新与领取条件保持一致：并发实例不能重复接管同一条事件。
			result := tx.Model(&ingestionModel{}).Where("id = ? AND "+claimable, append([]any{rows[index].ID}, claimableArgs...)...).Updates(map[string]any{"status": string(domain.IngestionStatusProcessing), "locked_until": leaseUntil, "updated_at": now})
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

// LoadIngestionEvent 仅当事件处于 PROCESSING 时解码并返回其租户、来源与载荷，
// 保证只有持租约的 worker 能读到事件内容。
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

// CompleteIngestion 把 PROCESSING 事件标记为 COMPLETED 并记录生成的消息 ID；
// 状态不匹配时返回冲突，防止过期租约覆盖新状态。
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

// RetryIngestion 记录失败原因、递增尝试计数并安排下次重试时间；
// 错误消息先经安全截断，避免上游错误原文进入存储。
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

// DeadLetterIngestion 把重试耗尽的事件置为 DEAD 终态并保留错误码与安全消息，
// 仅供人工排查与重放，不再自动处理。
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
