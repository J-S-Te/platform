package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/notification/domain"
)

type deliveryRetryRepository struct {
	Repository
	rows           []domain.Delivery
	completeErrors map[string]error
	persistErrors  map[string]error
	claimError     error
	claimTenant    string
	claimLimit     int
	claimLease     time.Time
	claimNow       time.Time
	claimCalls     int
	completed      []string
	failed         []string
}

func (r *deliveryRetryRepository) claim(limit int, lease, now time.Time) ([]domain.Delivery, error) {
	r.claimCalls++
	r.claimLimit, r.claimLease, r.claimNow = limit, lease, now
	return r.rows, r.claimError
}

func (r *deliveryRetryRepository) ClaimDueDeliveries(_ context.Context, limit int, lease, now time.Time) ([]domain.Delivery, error) {
	return r.claim(limit, lease, now)
}

func (r *deliveryRetryRepository) ClaimFailedDeliveries(_ context.Context, tenantID string, limit int, lease, now time.Time) ([]domain.Delivery, error) {
	r.claimTenant = tenantID
	return r.claim(limit, lease, now)
}

func (r *deliveryRetryRepository) CompleteDelivery(_ context.Context, tenantID, id string, now time.Time) (domain.Delivery, error) {
	for i, d := range r.rows {
		if d.ID != id || d.TenantID != tenantID || d.Status != domain.DeliveryStatusProcessing {
			continue
		}
		if err := r.completeErrors[id]; err != nil {
			return domain.Delivery{}, err
		}
		d.Status, d.DeliveredAt, d.LockedUntil, d.NextRetryAt = domain.DeliveryStatusDelivered, &now, nil, nil
		r.rows[i] = d
		r.completed = append(r.completed, tenantID+"/"+id)
		return d, nil
	}
	return domain.Delivery{}, ErrNotFound
}

func (r *deliveryRetryRepository) FailDelivery(_ context.Context, tenantID, id, _ string, retry, now time.Time) error {
	if err := r.persistErrors[id]; err != nil {
		return err
	}
	for i, d := range r.rows {
		if d.ID == id && d.TenantID == tenantID && d.Status == domain.DeliveryStatusProcessing {
			d.Status, d.NextRetryAt, d.LockedUntil, d.UpdatedAt = domain.DeliveryStatusFailed, &retry, nil, now
			d.AttemptCount++
			r.rows[i] = d
			r.failed = append(r.failed, tenantID+"/"+id)
			return nil
		}
	}
	return ErrNotFound
}

// 已持久化收件人与消息不因重试重新解析策略或创建消息。
type retryRejectPolicy struct{}

func (retryRejectPolicy) DeliveryVisibility(context.Context, string, time.Time) (bool, time.Time, error) {
	return false, time.Time{}, errors.New("policy must not be re-evaluated for an existing delivery")
}

func newDeliveryRetryService(t *testing.T, r *deliveryRetryRepository) *Service {
	t.Helper()
	s, err := NewService(r, retryRejectPolicy{}, reminderTestResolver{}, reminderTestIDs{}, reminderTestClock{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestManualRetryContinuesAfterFailureBookkeepingError(t *testing.T) {
	completeErr, persistErr := errors.New("complete failed"), errors.New("failure state unavailable")
	r := &deliveryRetryRepository{
		rows: []domain.Delivery{
			{ID: "first", TenantID: "tenant-a", Status: domain.DeliveryStatusProcessing},
			{ID: "second", TenantID: "tenant-a", Status: domain.DeliveryStatusProcessing},
		},
		completeErrors: map[string]error{"first": completeErr},
		persistErrors:  map[string]error{"first": persistErr},
	}
	result, err := newDeliveryRetryService(t, r).RetryFailedDeliveries(context.Background(), "tenant-a", 10)
	if !errors.Is(err, completeErr) || !errors.Is(err, persistErr) {
		t.Fatalf("completion and failure bookkeeping errors must both be observable: %v", err)
	}
	if r.rows[1].Status != domain.DeliveryStatusDelivered || result.Delivered != 1 || result.Failed != 1 || result.Claimed != 2 {
		t.Fatalf("one failure must not discard the rest of the claimed batch: rows=%+v result=%+v", r.rows, result)
	}
}

func TestManualDeliveryRetryCannotWriteOutsideRequestedTenant(t *testing.T) {
	r := &deliveryRetryRepository{rows: []domain.Delivery{
		{ID: "foreign", TenantID: "tenant-b", Status: domain.DeliveryStatusProcessing},
	}}
	result, err := newDeliveryRetryService(t, r).RetryFailedDeliveries(context.Background(), "tenant-a", 10)
	if err == nil || result.Delivered != 0 || r.rows[0].Status != domain.DeliveryStatusProcessing {
		t.Fatalf("manual request must retain its tenant scope even for an inconsistent claim result: result=%+v error=%v rows=%+v", result, err, r.rows)
	}
}

func TestBackgroundDeliveryRetryKeepsTenantAndBackoff(t *testing.T) {
	r := &deliveryRetryRepository{
		rows: []domain.Delivery{
			{ID: "success", TenantID: "tenant-a", Status: domain.DeliveryStatusProcessing},
			{ID: "retry", TenantID: "tenant-b", Status: domain.DeliveryStatusProcessing, AttemptCount: 2},
			{ID: "many-attempts", TenantID: "tenant-c", Status: domain.DeliveryStatusProcessing, AttemptCount: 99},
		},
		completeErrors: map[string]error{"retry": errors.New("try later"), "many-attempts": errors.New("try later")},
	}
	s := newDeliveryRetryService(t, r)
	result, err := s.RetryDueDeliveries(context.Background(), 500)
	if err != nil || result != (RetryResult{Claimed: 3, Delivered: 1, Failed: 2}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	now := time.Date(2026, 10, 7, 3, 30, 0, 0, time.UTC)
	if r.claimLimit != 100 || !r.claimLease.Equal(time.Date(2026, 10, 7, 3, 32, 0, 0, time.UTC)) || !r.claimNow.Equal(now) {
		t.Fatalf("invalid bounded claim limit=%d lease=%v now=%v", r.claimLimit, r.claimLease, r.claimNow)
	}
	if r.rows[0].Status != domain.DeliveryStatusDelivered || r.rows[1].Status != domain.DeliveryStatusFailed || r.rows[1].AttemptCount != 3 || r.rows[1].NextRetryAt == nil || !r.rows[1].NextRetryAt.Equal(time.Date(2026, 10, 7, 3, 38, 0, 0, time.UTC)) {
		t.Fatalf("tenant-scoped retry outcome=%+v", r.rows)
	}
	if r.rows[2].AttemptCount != 100 || r.rows[2].NextRetryAt == nil || !r.rows[2].NextRetryAt.Equal(time.Date(2026, 10, 7, 4, 34, 0, 0, time.UTC)) {
		t.Fatalf("current retry policy caps delay at 64 minutes without introducing an attempt cutoff: %+v", r.rows[2])
	}
}

func TestDeliveryRetryRejectsUnscopedManualRequestBeforeClaim(t *testing.T) {
	r := &deliveryRetryRepository{}
	s := newDeliveryRetryService(t, r)
	for _, tenant := range []string{"", "  "} {
		if _, err := s.RetryFailedDeliveries(context.Background(), tenant, 10); !errors.Is(err, ErrValidation) {
			t.Fatalf("tenant=%q error=%v, want validation", tenant, err)
		}
	}
	if r.claimCalls != 0 {
		t.Fatal("blank tenant must never become a system claim")
	}
	if _, err := s.RetryFailedDeliveries(context.Background(), "  tenant-a  ", 200); err != nil || r.claimTenant != "tenant-a" || r.claimLimit != 100 {
		t.Fatalf("manual tenant/limit normalization changed: tenant=%q limit=%d error=%v", r.claimTenant, r.claimLimit, err)
	}
}

func TestBackgroundDeliveryRetryRejectsInvalidLimitAndReturnsClaimFailure(t *testing.T) {
	r := &deliveryRetryRepository{claimError: errors.New("claim unavailable")}
	s := newDeliveryRetryService(t, r)
	if _, err := s.RetryDueDeliveries(context.Background(), 0); !errors.Is(err, ErrValidation) || r.claimCalls != 0 {
		t.Fatal("invalid limit must not claim work")
	}
	result, err := s.RetryDueDeliveries(context.Background(), 10)
	if !errors.Is(err, r.claimError) || result != (RetryResult{}) || len(r.completed) != 0 {
		t.Fatalf("claim error must remain observable without delivery writes: result=%+v err=%v", result, err)
	}
}
