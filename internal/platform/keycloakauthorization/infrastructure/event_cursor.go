package infrastructure

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// EventCursor 是某个 realm 的采集进度。空时间表示从未采集过。
type EventCursor struct {
	Realm               string
	LastEventTime       time.Time
	LastEventID         string
	ConsecutiveFailures uint
	LastError           string
}

// EventCursorStore 持久化 Keycloak 事件采集游标与连续失败计数。
type EventCursorStore interface {
	LoadEventCursor(ctx context.Context, realm string) (EventCursor, error)
	SaveEventCursor(ctx context.Context, cursor EventCursor, now time.Time) error
	RecordEventCursorFailure(ctx context.Context, realm, message string, now time.Time) (uint, error)
}

type eventCursorModel struct {
	Realm               string    `gorm:"column:realm;primaryKey"`
	LastEventTime       time.Time `gorm:"column:last_event_time"`
	LastEventID         *string   `gorm:"column:last_event_id"`
	ConsecutiveFailures uint      `gorm:"column:consecutive_failures"`
	LastError           *string   `gorm:"column:last_error"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
}

// TableName 绑定 GORM 模型到 keycloak_event_cursor 表。
func (eventCursorModel) TableName() string { return "keycloak_event_cursor" }

// EventCursorRepository 用数据库保存游标，与 Admin API 客户端解耦，便于测试。
type EventCursorRepository struct {
	db *gorm.DB
}

// NewEventCursorRepository 构造游标仓储。
func NewEventCursorRepository(db *gorm.DB) *EventCursorRepository {
	return &EventCursorRepository{db: db}
}

// LoadEventCursor 读取游标；没有记录时返回零游标（从配置的回看窗口开始）。
func (admin *EventCursorRepository) LoadEventCursor(ctx context.Context, realm string) (EventCursor, error) {
	var row eventCursorModel
	err := admin.db.WithContext(ctx).Where("realm = ?", strings.TrimSpace(realm)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return EventCursor{Realm: strings.TrimSpace(realm)}, nil
	}
	if err != nil {
		return EventCursor{}, err
	}
	cursor := EventCursor{
		Realm: row.Realm, LastEventTime: row.LastEventTime.UTC(),
		ConsecutiveFailures: row.ConsecutiveFailures, LastError: derefString(row.LastError),
	}
	if row.LastEventID != nil {
		cursor.LastEventID = strings.TrimSpace(*row.LastEventID)
	}
	return cursor, nil
}

// SaveEventCursor 成功采集后推进游标并清零失败计数。
func (admin *EventCursorRepository) SaveEventCursor(ctx context.Context, cursor EventCursor, now time.Time) error {
	var lastEventID *string
	if value := strings.TrimSpace(cursor.LastEventID); value != "" {
		lastEventID = &value
	}
	return admin.db.WithContext(ctx).Save(&eventCursorModel{
		Realm: strings.TrimSpace(cursor.Realm), LastEventTime: cursor.LastEventTime.UTC(), LastEventID: lastEventID,
		ConsecutiveFailures: 0, LastError: nil, UpdatedAt: now.UTC(),
	}).Error
}

// RecordEventCursorFailure 记录一次采集失败并返回累计连续失败次数（用于异常告警）。
func (admin *EventCursorRepository) RecordEventCursorFailure(ctx context.Context, realm, message string, now time.Time) (uint, error) {
	value := strings.TrimSpace(message)
	if len(value) > 500 {
		value = value[:500]
	}
	var row eventCursorModel
	err := admin.db.WithContext(ctx).Where("realm = ?", strings.TrimSpace(realm)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		row = eventCursorModel{Realm: strings.TrimSpace(realm), LastEventTime: now.UTC()}
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	row.ConsecutiveFailures++
	row.LastError = &value
	row.UpdatedAt = now.UTC()
	if err := admin.db.WithContext(ctx).Save(&row).Error; err != nil {
		return 0, err
	}
	return row.ConsecutiveFailures, nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
