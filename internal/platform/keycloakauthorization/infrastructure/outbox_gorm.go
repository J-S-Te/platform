package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type authorizationOutboxRow struct {
	ID, TenantID, IdentityID, ApplicationID, EnvironmentID, EventType, Status string
	Attempts                                                                  uint
}

// TableName 绑定 GORM 模型到 keycloak_authorization_outbox 表。
func (authorizationOutboxRow) TableName() string { return "keycloak_authorization_outbox" }

// OutboxQueue 是投影 worker 使用的持久化 MySQL 适配器。
// Claim 通过行锁保证并发的 worker 副本不会同时同步同一 identity/application 对。
type OutboxQueue struct{ database *gorm.DB }

// NewOutboxQueue 构造 outbox 队列适配器；database 为空时返回错误。
func NewOutboxQueue(database *gorm.DB) (*OutboxQueue, error) {
	if database == nil {
		return nil, errors.New("Keycloak authorization outbox database must not be nil")
	}
	return &OutboxQueue{database: database}, nil
}

// RecoverStale 把崩溃 worker 遗留的 RUNNING 事件退回 PENDING。条件更新使多副本
// 竞争下的恢复是安全的：一个副本改掉行状态后，其余副本的条件不再匹配。
// Attempts 刻意保持不变——恢复并未开始一次新的处理尝试；事件被再次领取时
// 由 Claim 递增。
func (queue *OutboxQueue) RecoverStale(ctx context.Context, staleBefore, availableAt time.Time) error {
	result := queue.database.WithContext(ctx).Model(&authorizationOutboxRow{}).
		Where("status = ? AND locked_at < ?", "RUNNING", staleBefore.UTC()).
		Updates(staleRecoveryUpdates(availableAt))
	if result.Error != nil {
		return fmt.Errorf("recover stale Keycloak authorization outbox: %w", result.Error)
	}
	return nil
}

func staleRecoveryUpdates(availableAt time.Time) map[string]any {
	return map[string]any{
		"status":             "PENDING",
		"available_at":       availableAt.UTC(),
		"locked_by":          nil,
		"locked_at":          nil,
		"last_error_code":    "KEYCLOAK_SYNC_INTERRUPTED",
		"last_error_message": "Recovered after worker lock timeout",
	}
}

// Claim 在事务内锁定并领取一条到期 PENDING 事件：SKIP LOCKED 行锁保证多副本
// 只有一个 worker 处理同一条事件，领取时递增 Attempts 并置为 RUNNING。
// 无可领取事件时返回 claimed=false。
func (queue *OutboxQueue) Claim(ctx context.Context, workerID string, now time.Time) (projectionworker.Event, bool, error) {
	// 先锁定再标记 RUNNING，保证多副本只会有一个 worker 处理同一条事件。
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return projectionworker.Event{}, false, errors.New("Keycloak authorization worker ID is required")
	}
	var claimed authorizationOutboxRow
	err := queue.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND available_at <= ?", "PENDING", now.UTC()).
			Order("available_at ASC, created_at ASC").Limit(1).Find(&claimed)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		updated := tx.Model(&authorizationOutboxRow{}).Where("id = ? AND status = ?", claimed.ID, "PENDING").Updates(map[string]any{
			"status": "RUNNING", "locked_by": workerID, "locked_at": now.UTC(), "attempts": gorm.Expr("attempts + 1"),
		})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return fmt.Errorf("claim Keycloak authorization outbox %s lost concurrency race", claimed.ID)
		}
		claimed.Attempts++
		return nil
	})
	if err != nil {
		return projectionworker.Event{}, false, fmt.Errorf("claim Keycloak authorization outbox: %w", err)
	}
	if claimed.ID == "" {
		return projectionworker.Event{}, false, nil
	}
	return projectionworker.Event{ID: claimed.ID, TenantID: claimed.TenantID, IdentityID: claimed.IdentityID, ApplicationID: claimed.ApplicationID, EnvironmentID: claimed.EnvironmentID, EventType: claimed.EventType, Attempts: claimed.Attempts}, true, nil
}

// Complete 把 RUNNING 事件置为 SUCCEEDED；当该事件所属环境再无 PENDING/RUNNING/FAILED
// 事件时，同事务开放 keycloak_switch_readiness 的 user_projection_completed 门禁。
func (queue *OutboxQueue) Complete(ctx context.Context, event projectionworker.Event) error {
	// 事件完成后才检查环境队列；确认无 PENDING/RUNNING 事件，才开放用户投影门禁。
	now := time.Now().UTC()
	return queue.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&authorizationOutboxRow{}).Where("id = ? AND status = ?", strings.TrimSpace(event.ID), "RUNNING").Updates(map[string]any{
			"status": "SUCCEEDED", "completed_at": now, "locked_by": nil, "locked_at": nil,
		})
		if result.Error != nil {
			return fmt.Errorf("complete Keycloak authorization outbox: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return errors.New("Keycloak authorization outbox is not running")
		}
		if strings.TrimSpace(event.EnvironmentID) == "" {
			return nil
		}
		var pending int64
		if err := tx.Table("keycloak_authorization_outbox").Where("tenant_id=? AND application_id=? AND environment_id=? AND status IN ?", event.TenantID, event.ApplicationID, event.EnvironmentID, []string{"PENDING", "RUNNING", "FAILED"}).Count(&pending).Error; err != nil {
			return fmt.Errorf("count pending Keycloak projections: %w", err)
		}
		if pending == 0 {
			if err := tx.Table("keycloak_switch_readiness").Where("tenant_id=? AND application_id=? AND environment_id=?", event.TenantID, event.ApplicationID, event.EnvironmentID).Updates(map[string]any{"user_projection_completed": true, "updated_at": now}).Error; err != nil {
				return fmt.Errorf("mark Keycloak user projection complete: %w", err)
			}
		}
		return nil
	})
}

// Retry 把 RUNNING 事件退回 PENDING 并记录错误码/消息，availableAt 由 worker
// 按指数退避计算；事件不是 RUNNING 状态时返回错误，防止并发转移。
func (queue *OutboxQueue) Retry(ctx context.Context, event projectionworker.Event, code, message string, availableAt time.Time) error {
	// 重试释放锁并回到 PENDING，指数退避由 worker 决定，避免失败事件阻塞整个队列。
	result := queue.database.WithContext(ctx).Model(&authorizationOutboxRow{}).Where("id = ? AND status = ?", strings.TrimSpace(event.ID), "RUNNING").Updates(map[string]any{
		"status": "PENDING", "available_at": availableAt.UTC(), "locked_by": nil, "locked_at": nil,
		"last_error_code": strings.TrimSpace(code), "last_error_message": strings.TrimSpace(message),
	})
	if result.Error != nil {
		return fmt.Errorf("retry Keycloak authorization outbox: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("Keycloak authorization outbox is not running")
	}
	return nil
}

// Fail 把重试耗尽的投影事件转入持久的死信状态。事件刻意保持可查询而非删除；
// FAILED 事件会阻塞对应环境的 Keycloak 切换门禁。
func (queue *OutboxQueue) Fail(ctx context.Context, event projectionworker.Event, code, message string) error {
	result := queue.database.WithContext(ctx).Model(&authorizationOutboxRow{}).Where("id = ? AND status = ?", strings.TrimSpace(event.ID), "RUNNING").Updates(map[string]any{
		"status": "FAILED", "completed_at": time.Now().UTC(), "locked_by": nil, "locked_at": nil,
		"last_error_code": strings.TrimSpace(code), "last_error_message": strings.TrimSpace(message),
	})
	if result.Error != nil {
		return fmt.Errorf("dead-letter Keycloak authorization outbox: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("Keycloak authorization outbox is not running")
	}
	return nil
}

var _ projectionworker.Queue = (*OutboxQueue)(nil)
