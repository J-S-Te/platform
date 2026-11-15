package securityevent

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Query struct {
	ApplicationCode string
	Result          string
	OccurredFrom    *time.Time
	OccurredTo      *time.Time
	Page            int
	PageSize        int
}

type Store struct{ DB *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{DB: db} }

// Record 幂等追加安全事件；同一应用重复 event_id 不覆盖已有事实。
func (s *Store) Record(ctx context.Context, event Event) error {
	if s == nil || s.DB == nil {
		return gorm.ErrInvalidDB
	}
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = event.ReceivedAt
	}
	return s.DB.WithContext(ctx).Table(event.TableName()).
		Clauses(clause.OnConflict{DoNothing: true}).Create(&event).Error
}

func (s *Store) List(ctx context.Context, query Query) ([]Event, int64, error) {
	if s == nil || s.DB == nil {
		return nil, 0, gorm.ErrInvalidDB
	}
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 || query.PageSize > 100 {
		query.PageSize = 20
	}
	db := s.DB.WithContext(ctx).Table(Event{}.TableName())
	if query.ApplicationCode != "" {
		db = db.Where("application_code = ?", query.ApplicationCode)
	}
	if query.Result != "" {
		db = db.Where("result = ?", query.Result)
	}
	if query.OccurredFrom != nil {
		db = db.Where("occurred_at >= ?", *query.OccurredFrom)
	}
	if query.OccurredTo != nil {
		db = db.Where("occurred_at <= ?", *query.OccurredTo)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []Event
	err := db.Order("occurred_at DESC, id DESC").
		Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&items).Error
	return items, total, err
}

// NormalizeApplicationCode 只做轻量规范化；是否允许该来源由应用令牌决定，不由请求体决定。
func NormalizeApplicationCode(value string) string { return strings.TrimSpace(value) }
