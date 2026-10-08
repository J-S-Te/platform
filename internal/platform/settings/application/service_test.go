package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/settings/domain"
)

type settingsTestRepository struct {
	Repository
	saved    NotificationSettingsUpdateInput
	existing domain.NotificationSettings
	getErr   error
}

func (r *settingsTestRepository) GetNotificationSettings(context.Context, string) (domain.NotificationSettings, error) {
	return r.existing, r.getErr
}
func (r *settingsTestRepository) SaveNotificationSettings(_ context.Context, input NotificationSettingsUpdateInput, id string, _ time.Time) (domain.NotificationSettings, error) {
	r.saved = input
	return domain.NotificationSettings{ID: id, TenantID: input.TenantID, InboxEnabled: input.InboxEnabled, EmailEnabled: input.EmailEnabled, ReminderFrequency: input.ReminderFrequency, Version: input.Version + 1}, nil
}

type settingsTestID struct{}

func (settingsTestID) New(time.Time) (string, error) { return "settings-1", nil }

type settingsTestClock struct{}

func (settingsTestClock) Now() time.Time { return time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC) }

func TestNotificationDefaultsDisableUnavailableEmail(t *testing.T) {
	service, _ := NewService(&settingsTestRepository{getErr: ErrNotFound}, settingsTestID{}, settingsTestClock{})
	got, err := service.GetNotificationSettings(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if !got.InboxEnabled || got.EmailEnabled {
		t.Fatalf("unsafe defaults: %+v", got)
	}
}
func TestNotificationSettingsRejectEmailUntilDeliveryWorkerExists(t *testing.T) {
	service, _ := NewService(&settingsTestRepository{}, settingsTestID{}, settingsTestClock{})
	_, err := service.UpdateNotificationSettings(context.Background(), NotificationSettingsUpdateInput{TenantID: "tenant", OperatorID: "user", InboxEnabled: true, EmailEnabled: true, ReminderFrequency: domain.ReminderFrequencyDaily, Version: 1})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error=%v, want validation", err)
	}
}

// 新词表四档全部可保存；退役词表（EVERY_FOUR_HOURS/ONCE）必须被拒绝。
func TestNotificationSettingsAcceptsNewReminderVocabulary(t *testing.T) {
	service, _ := NewService(&settingsTestRepository{}, settingsTestID{}, settingsTestClock{})
	for _, frequency := range []domain.ReminderFrequency{
		domain.ReminderFrequencyImmediate, domain.ReminderFrequencyDaily,
		domain.ReminderFrequencyWeekly, domain.ReminderFrequencyNever,
	} {
		if _, err := service.UpdateNotificationSettings(context.Background(), NotificationSettingsUpdateInput{TenantID: "tenant", OperatorID: "user", InboxEnabled: true, ReminderFrequency: frequency, Version: 1}); err != nil {
			t.Fatalf("frequency %q rejected: %v", frequency, err)
		}
	}
}
func TestNotificationSettingsRejectsRetiredReminderVocabulary(t *testing.T) {
	service, _ := NewService(&settingsTestRepository{}, settingsTestID{}, settingsTestClock{})
	for _, frequency := range []domain.ReminderFrequency{"EVERY_FOUR_HOURS", "ONCE", "HOURLY", ""} {
		if _, err := service.UpdateNotificationSettings(context.Background(), NotificationSettingsUpdateInput{TenantID: "tenant", OperatorID: "user", InboxEnabled: true, ReminderFrequency: frequency, Version: 1}); !errors.Is(err, ErrValidation) {
			t.Fatalf("frequency %q error=%v, want validation", frequency, err)
		}
	}
}
func TestNotificationDefaultsUseImmediateReminder(t *testing.T) {
	service, _ := NewService(&settingsTestRepository{getErr: ErrNotFound}, settingsTestID{}, settingsTestClock{})
	got, err := service.GetNotificationSettings(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if got.ReminderFrequency != domain.ReminderFrequencyImmediate {
		t.Fatalf("default reminder = %q, want IMMEDIATE（保持未设置租户的现状行为）", got.ReminderFrequency)
	}
}
