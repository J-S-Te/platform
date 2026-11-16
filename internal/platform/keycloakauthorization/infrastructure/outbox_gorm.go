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

// Claim 锁定到期事件并领取，返回属主与递增代次作为后续状态转移凭据。
// 参数：ctx 为请求上下文，workerID 为处理者标识，now 为领取基准时间。
// 返回值：领取事件、是否领取成功及数据库错误；无事件时返回 false。
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
	return projectionworker.Event{
		ID: claimed.ID, TenantID: claimed.TenantID, IdentityID: claimed.IdentityID,
		ApplicationID: claimed.ApplicationID, EnvironmentID: claimed.EnvironmentID,
		EventType: claimed.EventType, Attempts: claimed.Attempts, WorkerID: workerID,
	}, true, nil
}

// Complete 完成当前领取，成功后在同事务内核对该环境投影就绪状态。
// 参数：ctx 为请求上下文，event 必须携带当前属主、代次和完整事件身份。
// 返回值：无有效租约或持久化失败时返回错误，迟到结果不修改事件与就绪状态。
func (queue *OutboxQueue) Complete(ctx context.Context, event projectionworker.Event) error {
	// 事件完成后才检查环境队列；确认无 PENDING/RUNNING 事件，才开放用户投影门禁。
	now := time.Now().UTC()
	return queue.database.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		owned, err := ownedRunningEvent(tx, event)
		if err != nil {
			return err
		}
		result := owned.Updates(map[string]any{
			"status": "SUCCEEDED", "completed_at": now, "locked_by": nil, "locked_at": nil,
		})
		if result.Error != nil {
			return fmt.Errorf("complete Keycloak authorization outbox: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return errors.New("Keycloak authorization outbox is not running")
		}
		return markProjectionReadiness(tx, event, now)
	})
}

// Retry 把当前领取退回待重试状态，过期属主或代次不能释放新领取。
// 参数：ctx 为上下文，event 为领取凭据，code/message 为原因，availableAt 为重试时间。
// 返回值：租约冲突或数据库错误；成功时返回 nil。
func (queue *OutboxQueue) Retry(ctx context.Context, event projectionworker.Event, code, message string, availableAt time.Time) error {
	// 重试释放锁并回到 PENDING，指数退避由 worker 决定，避免失败事件阻塞整个队列。
	owned, err := ownedRunningEvent(queue.database.WithContext(ctx), event)
	if err != nil {
		return err
	}
	result := owned.Updates(map[string]any{
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

// Fail 将当前领取转为可查询的失败状态，继续阻止对应环境切换。
// 参数：ctx 为上下文，event 为领取凭据，code/message 为持久化失败原因。
// 返回值：租约冲突或数据库错误；成功时返回 nil。
func (queue *OutboxQueue) Fail(ctx context.Context, event projectionworker.Event, code, message string) error {
	owned, err := ownedRunningEvent(queue.database.WithContext(ctx), event)
	if err != nil {
		return err
	}
	result := owned.Updates(map[string]any{
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

// ownedRunningEvent 构建当前事件属主与领取代次的条件更新范围。
// 参数：tx 为数据库会话，event 为 Claim 返回的完整领取凭据。
// 返回值：带严格范围的查询；缺少领取身份时拒绝操作。
func ownedRunningEvent(tx *gorm.DB, event projectionworker.Event) (*gorm.DB, error) {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.TenantID) == "" ||
		strings.TrimSpace(event.WorkerID) == "" || event.Attempts == 0 {
		return nil, errors.New("Keycloak authorization outbox lease is required")
	}
	return tx.Model(&authorizationOutboxRow{}).
		Where("id = ? AND tenant_id = ? AND identity_id = ? AND application_id = ?",
			event.ID, event.TenantID, event.IdentityID, event.ApplicationID).
		Where("COALESCE(environment_id, '') = ? AND event_type = ?", event.EnvironmentID, event.EventType).
		Where("status = ? AND locked_by = ? AND attempts = ?", "RUNNING", event.WorkerID, event.Attempts), nil
}

// markProjectionReadiness 在当前领取成功后核对整个环境是否仍有未完成事件。
// 参数：tx 为完成事件的同一事务，event 为已核验的事件身份，now 为完成时间。
// 返回值：统计或更新失败时返回错误，环境为空或仍有未完成事件时返回 nil。
func markProjectionReadiness(tx *gorm.DB, event projectionworker.Event, now time.Time) error {
	if strings.TrimSpace(event.EnvironmentID) == "" {
		return nil
	}
	var pending int64
	err := tx.Table("keycloak_authorization_outbox").
		Where("tenant_id=? AND application_id=? AND environment_id=?",
			event.TenantID, event.ApplicationID, event.EnvironmentID).
		Where("status IN ?", []string{"PENDING", "RUNNING", "FAILED"}).Count(&pending).Error
	if err != nil {
		return fmt.Errorf("count pending Keycloak projections: %w", err)
	}
	if pending != 0 {
		return nil
	}
	return tx.Table("keycloak_switch_readiness").
		Where("tenant_id=? AND application_id=? AND environment_id=?",
			event.TenantID, event.ApplicationID, event.EnvironmentID).
		Updates(map[string]any{"user_projection_completed": true, "updated_at": now}).Error
}

var _ projectionworker.Queue = (*OutboxQueue)(nil)
