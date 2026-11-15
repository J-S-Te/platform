// Package firstrun 提供新手导览的最小遥测与功能开关端点。
// 只记录租户/用户标识和导览事件，不存姓名、账号、邮箱等 PII。
package firstrun

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Event 是一条导览/清单事件。
type Event struct {
	TenantID      string
	UserID        string
	Event         string
	GuideKey      string
	Step          int
	Outcome       string
	ClientVersion string
	TaskKey       string
	OccurredAt    time.Time
	CreatedAt     time.Time
}

// EventStore 追加写入导览事件。
type EventStore interface {
	Insert(context.Context, Event) error
}

// GORMStore 是 EventStore 的 MySQL 实现。
type GORMStore struct {
	db *gorm.DB
}

// NewGORMStore 构造事件仓储；db 为空时返回 nil，由调用方按未配置处理。
func NewGORMStore(db *gorm.DB) *GORMStore {
	if db == nil {
		return nil
	}
	return &GORMStore{db: db}
}

// Insert 追加写入一行；事件按发生时间排序，created_at 使用服务端时间。
func (store *GORMStore) Insert(ctx context.Context, event Event) error {
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return store.db.WithContext(ctx).Exec(
		`INSERT INTO first_run_event (tenant_id, user_id, event_name, guide_key, step_index, outcome, client_version, task_key, occurred_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.TenantID, event.UserID, event.Event, event.GuideKey, event.Step, event.Outcome, event.ClientVersion, event.TaskKey, event.OccurredAt.UTC(), createdAt.UTC(),
	).Error
}
