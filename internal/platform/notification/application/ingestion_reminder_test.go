package application

import (
	"context"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

type reminderCaptureRepository struct {
	Repository
	deliveries []domain.Delivery
}

func (r *reminderCaptureRepository) CreateMessage(_ context.Context, message domain.Message, deliveries []domain.Delivery) (MessageCreation, error) {
	r.deliveries = deliveries
	return MessageCreation{Message: message, Deliveries: deliveries}, nil
}

func (r *reminderCaptureRepository) CompleteDelivery(context.Context, string, string, time.Time) (domain.Delivery, error) {
	return domain.Delivery{}, nil
}

// 跨系统事件曾经丢弃策略返回的 remindAt：租户设置"每日 / 每周提醒"后，只有平台内部
// 通知按频率可见，子系统事件仍然立即出现在收件箱。两条创建路径必须使用同一份策略结果。
func TestProjectIngestionEventAppliesTenantReminderVisibility(t *testing.T) {
	now := time.Date(2026, time.October, 17, 3, 0, 0, 0, time.UTC)
	remindAt := now.Add(6 * time.Hour)
	repository := &reminderCaptureRepository{}
	service, err := NewService(repository, ingestionTestPolicy{enabled: true, remindAt: remindAt}, ingestionTestResolver{users: []string{"user-1"}}, ingestionTestIDs{}, ingestionTestClock{})
	if err != nil {
		t.Fatalf("construct notification service: %v", err)
	}

	if _, err := service.projectIngestionEvent(context.Background(), "tenant-1", "crm", "prod", domain.IngestionEvent{
		EventID: "event-1", EventType: "CUSTOMER_ASSIGNED", NotificationScope: "CROSS_SYSTEM",
		Title: "标题", Content: "内容", Recipients: []string{"user-1"}, OccurredAt: now, IdempotencyKey: "idem-1",
	}, now); err != nil {
		t.Fatalf("project ingestion event: %v", err)
	}

	if len(repository.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(repository.deliveries))
	}
	delivery := repository.deliveries[0]
	if delivery.RemindAt == nil {
		t.Fatal("跨系统通知投递缺少 remind_at：租户提醒频率被丢弃")
	}
	if !delivery.RemindAt.Equal(remindAt) {
		t.Fatalf("remind_at = %s, want %s", delivery.RemindAt.UTC(), remindAt.UTC())
	}
}
