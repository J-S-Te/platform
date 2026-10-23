package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

// 用持久队列边界替身驱动真实 Service 和 Worker；不启动数据库或外部 Provider。
type workerRetryRepository struct {
	application.Repository
	ingestionErr   error
	ingestionCalls int
	claimErr       error
	claimCalls     int
	deliveries     map[string]domain.Delivery
}

func (r *workerRetryRepository) ClaimIngestionEvents(context.Context, int, time.Time, time.Time) ([]domain.IngestionReceipt, error) {
	r.ingestionCalls++
	return nil, r.ingestionErr
}

func (r *workerRetryRepository) ClaimDueDeliveries(_ context.Context, limit int, leaseUntil, now time.Time) ([]domain.Delivery, error) {
	r.claimCalls++
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	var claimed []domain.Delivery
	for id, d := range r.deliveries {
		if len(claimed) >= limit || d.Status != domain.DeliveryStatusFailed || d.NextRetryAt == nil || d.NextRetryAt.After(now) {
			continue
		}
		d.Status, d.LockedUntil = domain.DeliveryStatusProcessing, &leaseUntil
		r.deliveries[id] = d
		claimed = append(claimed, d)
	}
	return claimed, nil
}

func (r *workerRetryRepository) CompleteDelivery(_ context.Context, tenantID, deliveryID string, now time.Time) (domain.Delivery, error) {
	d := r.deliveries[deliveryID]
	if d.TenantID != tenantID || d.Status != domain.DeliveryStatusProcessing {
		return domain.Delivery{}, application.ErrNotFound
	}
	d.Status, d.DeliveredAt, d.LockedUntil, d.NextRetryAt = domain.DeliveryStatusDelivered, &now, nil, nil
	r.deliveries[deliveryID] = d
	return d, nil
}

type workerPolicy struct{}

func (workerPolicy) DeliveryVisibility(context.Context, string, time.Time) (bool, time.Time, error) {
	return false, time.Time{}, errors.New("existing delivery retries must not create or re-resolve recipients")
}

type workerResolver struct{}

func (workerResolver) ResolveRecipients(context.Context, string, []domain.RecipientTarget, time.Time) ([]string, error) {
	return nil, errors.New("unexpected recipient resolution")
}

type workerIDs struct{}

func (workerIDs) New(time.Time) (string, error) { return "unused", nil }

type workerClock struct{ now time.Time }

func (c workerClock) Now() time.Time { return c.now }

func newRetryTestWorker(t *testing.T, repository *workerRetryRepository) *IngestionWorker {
	t.Helper()
	service, err := application.NewService(repository, workerPolicy{}, workerResolver{}, workerIDs{}, workerClock{time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewIngestionWorker(service, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestWorkerRetriesPersistedDueDeliveryWithoutHTTP(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ingestionErr error
	}{{"empty ingestion", nil}, {"ingestion failure", errors.New("ingestion unavailable")}} {
		t.Run(tc.name, func(t *testing.T) {
			due := time.Date(2026, 10, 21, 23, 59, 0, 0, time.UTC)
			repository := &workerRetryRepository{ingestionErr: tc.ingestionErr, deliveries: map[string]domain.Delivery{
				"delivery-1": {ID: "delivery-1", TenantID: "tenant-1", Status: domain.DeliveryStatusFailed, NextRetryAt: &due},
			}}
			worker := newRetryTestWorker(t, repository)
			worker.Process(context.Background())
			if got := repository.deliveries["delivery-1"].Status; got != domain.DeliveryStatusDelivered {
				t.Fatalf("status=%s, want DELIVERED without operator retry", got)
			}
			worker.Process(context.Background())
			if repository.claimCalls != 2 || repository.deliveries["delivery-1"].Status != domain.DeliveryStatusDelivered {
				t.Fatal("later polling must preserve completed delivery")
			}
		})
	}
}

func TestWorkerContinuesPollingAfterDeliveryClaimFailure(t *testing.T) {
	due := time.Date(2026, 10, 21, 23, 59, 0, 0, time.UTC)
	repository := &workerRetryRepository{claimErr: errors.New("delivery claim unavailable"), deliveries: map[string]domain.Delivery{
		"delivery-1": {ID: "delivery-1", TenantID: "tenant-1", Status: domain.DeliveryStatusFailed, NextRetryAt: &due},
	}}
	worker := newRetryTestWorker(t, repository)
	worker.Process(context.Background())
	if repository.deliveries["delivery-1"].Status != domain.DeliveryStatusFailed {
		t.Fatal("failed claim must not advance delivery state")
	}
	repository.claimErr = nil
	worker.Process(context.Background())
	if repository.deliveries["delivery-1"].Status != domain.DeliveryStatusDelivered {
		t.Fatal("next poll must retry the persisted delivery after a claim error")
	}
	if repository.ingestionCalls != 2 {
		t.Fatal("delivery claim failure must not starve the ingestion queue")
	}
}
