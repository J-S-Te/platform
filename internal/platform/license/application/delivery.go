package application

import (
	"context"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	core "github.com/J-S-Te/license-core"
)

// DeliveryImportResult reports persistence, not runtime enforcement. The
// coordinator must still register and collect acknowledgements from components.
type DeliveryImportResult struct {
	State      domain.State `json:"state"`
	Status     string       `json:"status"`
	Idempotent bool         `json:"idempotent"`
}

// ImportDelivery is exclusively for a controlled offline deployment bootstrap.
// A signed customer installation identity may initialize an empty installation;
// normal management Initialize remains server-generated. Only monotonic term
// extensions can renew automatically; other changes require Preview/Commit.
func (s *Service) ImportDelivery(ctx context.Context, raw, expectedEnvironment string, actor domain.Actor) (DeliveryImportResult, error) {
	var out DeliveryImportResult
	if !validActor(actor) || expectedEnvironment == "" {
		return out, domain.ErrInvalid
	}
	if len(s.trusted) == 0 {
		return out, domain.ErrTrustNotConfigured
	}
	l, err := core.Verify(raw, s.trusted)
	if err != nil {
		return out, err
	}
	if l.Environment != expectedEnvironment {
		return out, core.ErrDenied
	}
	digest, err := core.Digest(raw, s.trusted)
	if err != nil {
		return out, err
	}
	var blocked bool
	renewal := false
	err = s.repo.Lock(ctx, func(tx Transaction) error {
		now := s.now().UTC()
		d := tx.Deployment()
		if d != nil {
			if !bound(l, d) {
				return core.ErrDenied
			}
			st, e := s.state(tx)
			if e != nil {
				return e
			}
			if st.CurrentDigest != "" || st.PendingDigest != "" || st.HighestVersion != 0 {
				if digest != st.CurrentDigest && digest != st.PendingDigest {
					if st.Pending != nil || st.Current == nil || l.Version <= st.HighestVersion || !safeDeliveryRenewal(l, *st.Current) {
						return domain.ErrConfirmation
					}
					renewal = true
				} else {
					blocked, e = s.observe(tx, now)
					if e != nil {
						return e
					}
					if blocked {
						out.State, e = s.state(tx)
					} else {
						// A pending package must become current on bootstrap retry,
						// without relying on an unrelated management Read request.
						out.State, e = s.activate(tx, st, now)
						out.State.HighestObservedAt = tx.Deployment().HighestObservedAt
					}
					out.Idempotent = true
					return e
				}
			}
			blocked, e = s.observe(tx, now)
			if e != nil || blocked {
				return e
			}
		}
		// Never auto-install a partially expired entitlement set. Expiry is
		// exclusive and fixed, irrespective of the customer deployment date.
		for _, app := range l.Applications {
			if now.Unix() >= app.ExpiresAt {
				return fmt.Errorf("%w: delivery entitlement %s has expired", core.ErrDenied, app.Code)
			}
		}
		if d == nil {
			d = &domain.Deployment{ID: 1, InstanceID: l.InstanceID, CustomerID: l.CustomerID, Environment: l.Environment, HighestObservedAt: now.Unix(), CreatedAt: now, UpdatedAt: now}
			if e := tx.SaveDeployment(d); e != nil {
				return e
			}
			if e := tx.AddEvent(event("DELIVERY_INITIALIZED", d, digest, l.Version, actor, now)); e != nil {
				return e
			}
		}
		if e := tx.AddArtifact(domain.Artifact{Digest: digest, RawJWS: raw, Version: l.Version, CreatedAt: now}); e != nil {
			return e
		}
		d.HighestVersion = l.Version
		d.Revision++
		d.UpdatedAt = now
		kind := "DELIVERY_IMPORTED_CURRENT"
		if renewal {
			kind = "DELIVERY_RENEWED_CURRENT"
		}
		if now.Unix() < l.NotBefore {
			d.PendingDigest = digest
			kind = "DELIVERY_IMPORTED_PENDING"
		} else {
			d.CurrentDigest = digest
		}
		if e := tx.SaveDeployment(d); e != nil {
			return e
		}
		if e := tx.AddEvent(event(kind, d, digest, l.Version, actor, now)); e != nil {
			return e
		}
		var e error
		out.State, e = s.state(tx)
		return e
	})
	if err != nil {
		return DeliveryImportResult{}, err
	}
	if blocked {
		return out, domain.ErrClock
	}
	out.Status = "IMPORTED"
	now := s.now().UTC().Unix()
	if now < l.NotBefore {
		out.Status = "PENDING_EFFECTIVE_DATE"
	}
	for _, app := range l.Applications {
		if now < app.NotBefore {
			out.Status = "PENDING_EFFECTIVE_DATE"
		}
	}
	for _, app := range l.Applications {
		if now >= app.ExpiresAt {
			out.Status = "EXPIRED"
		}
	}
	return out, nil
}

func safeDeliveryRenewal(next, current core.License) bool {
	if next.NotBefore > current.NotBefore || len(next.Applications) != len(current.Applications) {
		return false
	}
	previous := make(map[string]core.Application, len(current.Applications))
	for _, app := range current.Applications {
		previous[app.Code] = app
	}
	for _, app := range next.Applications {
		old, ok := previous[app.Code]
		if !ok || app.NotBefore > old.NotBefore || app.ExpiresAt < old.ExpiresAt || (old.Kind == "FULL" && app.Kind != "FULL") {
			return false
		}
	}
	return true
}
