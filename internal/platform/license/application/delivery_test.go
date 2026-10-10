package application

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	core "github.com/J-S-Te/license-core"
)

func deliveryFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)
	f.repo = newMemory()
	f.s.repo = f.repo
	f.instance = "customer-installation"
	return f
}

func TestDeliveryFirstImportAndConcurrentRetry(t *testing.T) {
	f := deliveryFixture(t)
	raw := f.sign(t, 4, f.now.Unix()-10)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor)
			if err != nil || r.Status != "IMPORTED" || r.State.HighestVersion != 4 || r.State.InstanceID != f.instance {
				t.Errorf("import: %+v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if len(f.repo.artifacts) != 1 || len(f.repo.events) != 2 || f.repo.d.Revision != 1 {
		t.Fatalf("duplicate persisted rows: %+v", f.repo.d)
	}
	_, err := f.s.ImportDelivery(context.Background(), f.sign(t, 5, f.now.Unix()), "production", testActor)
	if !errors.Is(err, domain.ErrConfirmation) || f.repo.d.HighestVersion != 4 {
		t.Fatalf("renewal bypass: %v", err)
	}
}

func TestDeliveryInvalidInputsDoNotInitialize(t *testing.T) {
	for _, scenario := range []string{"signature", "environment", "actor", "expired", "expiry_boundary", "untrusted"} {
		t.Run(scenario, func(t *testing.T) {
			f := deliveryFixture(t)
			n := f.now.Unix()
			raw := f.sign(t, 1, n-10)
			env, actor := "production", testActor
			switch scenario {
			case "signature":
				_, other, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				f.private = other
				raw = f.sign(t, 1, n-10)
			case "environment":
				env = "dev"
			case "actor":
				actor.UserID = ""
			case "expired":
				raw = f.sign(t, 1, n-100, core.Application{Code: "contract_management", NotBefore: n - 100, ExpiresAt: n - 1, Kind: "FULL"})
			case "expiry_boundary":
				raw = f.sign(t, 1, n-100, core.Application{Code: "contract_management", NotBefore: n - 100, ExpiresAt: n, Kind: "FULL"})
			case "untrusted":
				f.s.trusted = nil
			}
			if _, err := f.s.ImportDelivery(context.Background(), raw, env, actor); err == nil {
				t.Fatal("invalid input accepted")
			}
			if f.repo.d != nil || len(f.repo.artifacts) != 0 || len(f.repo.events) != 0 {
				t.Fatal("invalid import mutated installation")
			}
		})
	}
}

func TestDeliveryPendingAndExistingIdentity(t *testing.T) {
	f := deliveryFixture(t)
	n := f.now.Unix()
	r, err := f.s.ImportDelivery(context.Background(), f.sign(t, 1, n+100), "production", testActor)
	if err != nil || r.Status != "PENDING_EFFECTIVE_DATE" || r.State.Current != nil || r.State.Pending == nil {
		t.Fatalf("future license: %+v %v", r, err)
	}
	f.instance = "other-installation"
	if _, err = f.s.ImportDelivery(context.Background(), f.sign(t, 1, n), "production", testActor); !errors.Is(err, core.ErrDenied) {
		t.Fatalf("identity override: %v", err)
	}
	// An ordinary server-generated identity is never replaced by package identity.
	f = setup(t)
	f.instance = "packaged-instance"
	if _, err = f.s.ImportDelivery(context.Background(), f.sign(t, 1, n), "production", testActor); !errors.Is(err, core.ErrDenied) {
		t.Fatalf("normal initialization override: %v", err)
	}
}

type deliveryFailRepo struct {
	*memoryRepo
	stage string
}
type deliveryFailTx struct {
	Transaction
	stage string
}

func (r *deliveryFailRepo) Lock(ctx context.Context, fn func(Transaction) error) error {
	return r.memoryRepo.Lock(ctx, func(tx Transaction) error { return fn(deliveryFailTx{tx, r.stage}) })
}
func (tx deliveryFailTx) AddArtifact(a domain.Artifact) error {
	if tx.stage == "artifact" {
		return errors.New("artifact unavailable")
	}
	return tx.Transaction.AddArtifact(a)
}
func (tx deliveryFailTx) AddEvent(e domain.Event) error {
	if tx.stage == "audit" && e.Kind == "DELIVERY_IMPORTED_CURRENT" {
		return errors.New("audit unavailable")
	}
	return tx.Transaction.AddEvent(e)
}
func TestDeliveryTransactionRollback(t *testing.T) {
	for _, stage := range []string{"artifact", "audit"} {
		t.Run(stage, func(t *testing.T) {
			f := deliveryFixture(t)
			f.s.repo = &deliveryFailRepo{f.repo, stage}
			if _, err := f.s.ImportDelivery(context.Background(), f.sign(t, 1, f.now.Unix()), "production", testActor); err == nil {
				t.Fatal("injected error ignored")
			}
			if f.repo.d != nil || len(f.repo.artifacts) != 0 || len(f.repo.events) != 0 {
				t.Fatal("partial first import persisted")
			}
		})
	}
}

func TestDeliveryRetryPreservesClockLatch(t *testing.T) {
	f := deliveryFixture(t)
	raw := f.sign(t, 1, f.now.Unix())
	if _, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(-600 * time.Second)
	if _, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor); !errors.Is(err, domain.ErrClock) {
		t.Fatalf("rollback bypass: %v", err)
	}
	if !f.repo.d.ClockBlocked || f.repo.d.HighestVersion != 1 {
		t.Fatal("clock/version reset")
	}
}

func TestDeliveryRetryAfterExpiryDoesNotExtendTerm(t *testing.T) {
	f := deliveryFixture(t)
	n := f.now.Unix()
	raw := f.sign(t, 1, n, core.Application{Code: "contract_management", NotBefore: n, ExpiresAt: n + 60, Kind: "FULL"})
	if _, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	r, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor)
	if err != nil || r.Status != "EXPIRED" || !r.Idempotent || r.State.Current.Applications[0].ExpiresAt != n+60 {
		t.Fatalf("restart changed expiry: %+v %v", r, err)
	}
}

func TestDeliveryPendingRetryActivatesAtFixedStart(t *testing.T) {
	f := deliveryFixture(t)
	n := f.now.Unix()
	raw := f.sign(t, 7, n+100)
	first, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor)
	if err != nil || first.Status != "PENDING_EFFECTIVE_DATE" || first.State.Current != nil {
		t.Fatalf("first future import: %+v %v", first, err)
	}
	f.now = f.now.Add(100 * time.Second)
	r, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor)
	if err != nil || r.Status != "IMPORTED" || !r.Idempotent || r.State.Pending != nil || r.State.CurrentVersion != 7 || r.State.Current.NotBefore != n+100 || r.State.Current.Applications[0].ExpiresAt != n+100+86400 {
		t.Fatalf("retry did not activate unchanged term: %+v %v", r, err)
	}
	if len(f.repo.artifacts) != 1 || len(f.repo.events) != 3 || f.repo.events[2].Kind != "ACTIVATED" || r.State.Revision != 2 {
		t.Fatal("retry duplicated import instead of existing activation")
	}
	if _, err := f.s.ImportDelivery(context.Background(), raw, "production", testActor); err != nil || len(f.repo.events) != 3 {
		t.Fatalf("activation not idempotent: %v", err)
	}
}

func TestDeliveryMonotonicRenewalOnly(t *testing.T) {
	for _, scenario := range []string{"extend", "shorten", "delay", "downgrade", "replace", "rollback_version", "pending"} {
		t.Run(scenario, func(t *testing.T) {
			f := deliveryFixture(t)
			n := f.now.Unix()
			original := core.Application{Code: "contract_management", NotBefore: n - 10, ExpiresAt: n + 1000, Kind: "FULL"}
			if _, err := f.s.ImportDelivery(context.Background(), f.sign(t, 2, n-10, original), "production", testActor); err != nil {
				t.Fatal(err)
			}
			next, version, nbf := original, uint64(3), n-10
			next.ExpiresAt += 1000
			switch scenario {
			case "shorten":
				next.ExpiresAt = n + 500
			case "delay":
				next.NotBefore = n
				nbf = n
			case "downgrade":
				next.Kind = "TRIAL"
			case "replace":
				next.Code = "settlement"
			case "rollback_version":
				version = 1
			case "pending":
				pending := f.sign(t, 3, n+100)
				if _, err := f.s.Commit(context.Background(), f.input(t, pending), testActor); err != nil {
					t.Fatal(err)
				}
				version = 4
			}
			r, err := f.s.ImportDelivery(context.Background(), f.sign(t, version, nbf, next), "production", testActor)
			if scenario == "extend" {
				if err != nil || r.State.CurrentVersion != 3 || r.State.Current.Applications[0].ExpiresAt != n+2000 || len(f.repo.events) != 3 {
					t.Fatalf("renewal: %+v %v", r, err)
				}
			} else if !errors.Is(err, domain.ErrConfirmation) || f.repo.d.CurrentDigest == "" || len(f.repo.artifacts) > 2 {
				t.Fatalf("unsafe renewal: %+v %v", r, err)
			}
		})
	}
}
