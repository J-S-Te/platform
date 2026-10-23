package application

import (
	"context"
	"errors"
	"testing"
	"time"

	projectionworker "github.com/J-S-Te/Basic-Platform/internal/platform/keycloakauthorization/worker"
)

type watermarkClock struct{ now time.Time }

func (c *watermarkClock) Now() time.Time { return c.now }

type watermarkSource struct {
	clock *watermarkClock
	fail  bool
}

func (s watermarkSource) LoadAuthorizationProjection(context.Context, projectionworker.Event) (Snapshot, error) {
	s.clock.now = s.clock.now.Add(time.Minute)
	if s.fail {
		return Snapshot{}, errors.New("source unavailable")
	}
	return Snapshot{TenantID: "tenant", IdentityID: "user", ApplicationID: "app", EnvironmentID: "prod", ApplicationCode: "orders", KeycloakClientID: "orders-prod", OrganizationIDs: []string{}, Roles: []string{}, Permissions: []string{}}, nil
}

type watermarkAdmin struct {
	clock *watermarkClock
	fail  bool
}

func (a watermarkAdmin) EnsureUser(context.Context, Snapshot) error {
	a.clock.now = a.clock.now.Add(time.Minute)
	if a.fail {
		return errors.New("admin unavailable")
	}
	return nil
}
func (watermarkAdmin) EnsureOrganizationGroups(context.Context, Snapshot) error         { return nil }
func (watermarkAdmin) AssignClientRoles(context.Context, Snapshot) error                { return nil }
func (watermarkAdmin) SetClientAuthorizationAttributes(context.Context, Snapshot) error { return nil }

type watermarkStore struct {
	at             time.Time
	synced, failed int
}

func (s *watermarkStore) MarkSynchronized(_ context.Context, _ Snapshot, at time.Time) error {
	s.synced++
	s.at = at
	return nil
}
func (s *watermarkStore) MarkFailed(context.Context, projectionworker.Event, string, string, time.Time) error {
	s.failed++
	return nil
}

func TestSynchronizerWatermarkPrecedesSourceAndExternalWrites(t *testing.T) {
	start := time.Date(2026, 10, 21, 12, 0, 0, 123700000, time.UTC)
	for _, tc := range []struct {
		name                  string
		sourceFail, adminFail bool
	}{{"success", false, false}, {"source failure", true, false}, {"admin failure", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &watermarkClock{start}
			store := &watermarkStore{}
			sync, err := NewSynchronizer(watermarkSource{clock, tc.sourceFail}, watermarkAdmin{clock, tc.adminFail}, store, clock)
			if err != nil {
				t.Fatal(err)
			}
			err = sync.SyncAuthorization(context.Background(), projectionworker.Event{TenantID: "tenant", IdentityID: "user", ApplicationID: "app", EnvironmentID: "prod"})
			if tc.sourceFail || tc.adminFail {
				if err == nil || store.synced != 0 || store.failed != 1 {
					t.Fatalf("failure advanced watermark: err=%v store=%+v", err, store)
				}
				return
			}
			if err != nil || store.synced != 1 || !store.at.Equal(start.Truncate(time.Millisecond)) || !clock.now.After(store.at) {
				t.Fatalf("watermark must precede boundaries crossed while loading/writing: err=%v store=%+v now=%v", err, store, clock.now)
			}
		})
	}
}
