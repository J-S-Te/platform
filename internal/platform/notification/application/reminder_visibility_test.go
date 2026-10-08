package application

import (
	"context"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

type reminderTestRepository struct {
	Repository
	template      domain.Template
	version       domain.TemplateVersion
	deliveries    []domain.Delivery
	createCalls   int
	completeCalls int
}

func (r *reminderTestRepository) GetActiveTemplateByCode(context.Context, string, string) (domain.Template, domain.TemplateVersion, error) {
	return r.template, r.version, nil
}

func (r *reminderTestRepository) CreateMessage(_ context.Context, message domain.Message, deliveries []domain.Delivery) (MessageCreation, error) {
	r.createCalls++
	r.deliveries = deliveries
	return MessageCreation{Message: message, Deliveries: deliveries}, nil
}

func (r *reminderTestRepository) CompleteDelivery(context.Context, string, string, time.Time) (domain.Delivery, error) {
	r.completeCalls++
	return domain.Delivery{ID: "delivery-1", Status: domain.DeliveryStatusDelivered}, nil
}

type reminderTestPolicy struct {
	enabled  bool
	remindAt time.Time
}

func (p reminderTestPolicy) DeliveryVisibility(_ context.Context, _ string, now time.Time) (bool, time.Time, error) {
	remindAt := p.remindAt
	if remindAt.IsZero() {
		remindAt = now
	}
	return p.enabled, remindAt, nil
}

type reminderTestResolver struct{}

func (reminderTestResolver) ResolveRecipients(context.Context, string, []domain.RecipientTarget, time.Time) ([]string, error) {
	return []string{"user-1"}, nil
}

type reminderTestIDs struct{}

func (reminderTestIDs) New(time.Time) (string, error) { return "01H00000000000000000000002", nil }

type reminderTestClock struct{}

func (reminderTestClock) Now() time.Time { return time.Date(2026, 10, 7, 3, 30, 0, 0, time.UTC) }

func reminderTestInput() CreateInput {
	return CreateInput{
		TenantID: "tenant-1", OperatorID: "operator-1", TemplateCode: "ALERT_X", Category: "OPS",
		Recipients:     []domain.RecipientTarget{{Type: domain.RecipientTypeUser, ID: "user-1"}},
		IdempotencyKey: "idem-1",
	}
}

func newReminderTestService(t *testing.T, repo *reminderTestRepository, policy reminderTestPolicy) *Service {
	t.Helper()
	service, err := NewService(repo, policy, reminderTestResolver{}, reminderTestIDs{}, reminderTestClock{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service
}

func reminderTestTemplate() (domain.Template, domain.TemplateVersion) {
	return domain.Template{ID: "tpl-1", Code: "ALERT_X"}, domain.TemplateVersion{ID: "tplv-1", TitleTemplate: "标题", BodyTemplate: "内容"}
}

// 站内信关闭或 NEVER：创建被抑制，不落任何消息与投递。
func TestCreateSuppressesWhenVisibilityDisabled(t *testing.T) {
	repo := &reminderTestRepository{}
	template, version := reminderTestTemplate()
	repo.template, repo.version = template, version
	service := newReminderTestService(t, repo, reminderTestPolicy{enabled: false})

	result, err := service.Create(context.Background(), reminderTestInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !result.Suppressed {
		t.Fatalf("result = %+v, want suppressed", result)
	}
	if repo.createCalls != 0 {
		t.Fatalf("suppressed create persisted %d messages", repo.createCalls)
	}
}

// DAILY/WEEKLY：投递携带策略给出的延迟可见点，由查询侧过滤实现“到点汇总”。
func TestCreateStampsDelayedRemindAt(t *testing.T) {
	release := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	repo := &reminderTestRepository{}
	template, version := reminderTestTemplate()
	repo.template, repo.version = template, version
	service := newReminderTestService(t, repo, reminderTestPolicy{enabled: true, remindAt: release})

	result, err := service.Create(context.Background(), reminderTestInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.Suppressed {
		t.Fatal("enabled tenant must not be suppressed")
	}
	if len(repo.deliveries) != 1 || repo.deliveries[0].RemindAt == nil || !repo.deliveries[0].RemindAt.Equal(release) {
		t.Fatalf("deliveries = %+v, want remind_at=%v", repo.deliveries, release)
	}
	if repo.completeCalls != 1 {
		t.Fatalf("complete calls = %d, want 1", repo.completeCalls)
	}
}

// IMMEDIATE：可见点等于创建时刻，行为与历史版本一致。
func TestCreateStampsImmediateRemindAt(t *testing.T) {
	repo := &reminderTestRepository{}
	template, version := reminderTestTemplate()
	repo.template, repo.version = template, version
	service := newReminderTestService(t, repo, reminderTestPolicy{enabled: true})

	if _, err := service.Create(context.Background(), reminderTestInput()); err != nil {
		t.Fatalf("create: %v", err)
	}
	now := reminderTestClock{}.Now()
	if len(repo.deliveries) != 1 || repo.deliveries[0].RemindAt == nil || !repo.deliveries[0].RemindAt.Equal(now) {
		t.Fatalf("deliveries = %+v, want remind_at=%v", repo.deliveries, now)
	}
}
