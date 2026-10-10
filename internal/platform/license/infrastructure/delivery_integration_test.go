package infrastructure_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
	core "github.com/J-S-Te/license-core"
	"gorm.io/gorm"
)

func TestDeliveryMySQLAtomicImportRestartRenewalRace(t *testing.T) {
	db := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&domain.Deployment{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("requires empty isolated installation", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1900000000, 0)
	actor := domain.Actor{TenantID: "delivery-test-tenant", UserID: "delivery-test-operator"}
	const instance = "isolated-delivery-instance"
	newService := func() *application.Service {
		t.Helper()
		repo, err := infrastructure.NewRepository(db)
		if err != nil {
			t.Fatal(err)
		}
		s, err := application.NewService(repo, map[string]ed25519.PublicKey{"delivery-test": pub}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	sign := func(version uint64, expires int64) string {
		t.Helper()
		raw, err := core.Sign(core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "delivery-test-license", ProductID: core.Product, CustomerID: "delivery-test-customer", Environment: "production", InstanceID: instance, Version: version, IssuedAt: now.Unix() - 10, NotBefore: now.Unix() - 10, Applications: []core.Application{{Code: "contract_management", NotBefore: now.Unix() - 10, ExpiresAt: expires, Kind: "FULL"}}}, "delivery-test", priv)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	raw := sign(1, now.Unix()+1000)
	digest, err := core.Digest(raw, map[string]ed25519.PublicKey{"delivery-test": pub})
	if err != nil {
		t.Fatal(err)
	}
	digests := []string{digest}
	t.Cleanup(func() {
		for _, target := range []struct {
			model interface{}
			query string
			args  []interface{}
		}{
			{&domain.Event{}, "tenant_id = ? AND user_id = ?", []interface{}{actor.TenantID, actor.UserID}},
			{&domain.Deployment{}, "id = ? AND instance_id = ?", []interface{}{1, instance}},
			{&domain.Artifact{}, "digest IN ?", []interface{}{digests}},
		} {
			if err := db.Where(target.query, target.args...).Delete(target.model).Error; err != nil {
				t.Error(err)
			}
		}
	})
	// Fail after identity and artifact writes: real MySQL must roll back all rows.
	const callback = "test:delivery_audit_failure"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if event, ok := tx.Statement.Dest.(*domain.Event); ok && event.Kind == "DELIVERY_IMPORTED_CURRENT" {
			tx.AddError(errors.New("isolated audit failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, importErr := newService().ImportDelivery(ctx, raw, "production", actor)
	if err := db.Callback().Create().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if importErr == nil {
		t.Fatal("injected audit error ignored")
	}
	for _, model := range []interface{}{&domain.Deployment{}, &domain.Artifact{}, &domain.Event{}} {
		if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("partial import persisted: %T %d %v", model, count, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := newService()
			for attempt := 0; attempt < 10; attempt++ {
				r, err := s.ImportDelivery(ctx, raw, "production", actor)
				if errors.Is(err, domain.ErrConflict) {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				if err != nil || r.State.InstanceID != instance || r.State.CurrentVersion != 1 {
					t.Errorf("concurrent import: %+v %v", r, err)
				}
				return
			}
			t.Error("import retry exhausted")
		}()
	}
	wg.Wait()
	restarted := newService()
	r, err := restarted.ImportDelivery(ctx, raw, "production", actor)
	if err != nil || !r.Idempotent || r.State.Revision != 1 {
		t.Fatalf("restart: %+v %v", r, err)
	}
	renewal := sign(2, now.Unix()+2000)
	renewDigest, err := core.Digest(renewal, map[string]ed25519.PublicKey{"delivery-test": pub})
	if err != nil {
		t.Fatal(err)
	}
	digests = append(digests, renewDigest)
	r, err = restarted.ImportDelivery(ctx, renewal, "production", actor)
	if err != nil || r.State.CurrentVersion != 2 || r.State.Revision != 2 || r.State.InstanceID != instance {
		t.Fatalf("safe renewal: %+v %v", r, err)
	}
	if _, err = restarted.ImportDelivery(ctx, sign(3, now.Unix()+500), "production", actor); !errors.Is(err, domain.ErrConfirmation) {
		t.Fatalf("shortening accepted: %v", err)
	}
	if err := db.Model(&domain.Artifact{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("artifact count: %d %v", count, err)
	}
}

func TestDeliveryMySQLFutureDateRetryActivation(t *testing.T) {
	db := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1900000000, 0)
	start := now.Unix() + 60
	actor := domain.Actor{TenantID: "delivery-future-tenant", UserID: "delivery-future-operator"}
	const instance = "isolated-delivery-future-instance"
	raw, err := core.Sign(core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "delivery-future-license", ProductID: core.Product, CustomerID: "delivery-future-customer", Environment: "production", InstanceID: instance, Version: 1, IssuedAt: now.Unix(), NotBefore: start, Applications: []core.Application{{Code: "contract_management", NotBefore: start, ExpiresAt: start + 1000, Kind: "FULL"}}}, "delivery-future", priv)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"delivery-future": pub}
	digest, err := core.Digest(raw, keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Where("tenant_id = ? AND user_id = ?", actor.TenantID, actor.UserID).Delete(&domain.Event{}).Error; err != nil {
			t.Error(err)
		}
		if err := db.Where("digest = ? AND kind = ?", digest, "ACTIVATED").Delete(&domain.Event{}).Error; err != nil {
			t.Error(err)
		}
		if err := db.Where("id = ? AND instance_id = ?", 1, instance).Delete(&domain.Deployment{}).Error; err != nil {
			t.Error(err)
		}
		if err := db.Where("digest = ?", digest).Delete(&domain.Artifact{}).Error; err != nil {
			t.Error(err)
		}
	})
	service := func() *application.Service {
		t.Helper()
		repo, err := infrastructure.NewRepository(db)
		if err != nil {
			t.Fatal(err)
		}
		s, err := application.NewService(repo, keys, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	r, err := service().ImportDelivery(ctx, raw, "production", actor)
	if err != nil || r.Status != "PENDING_EFFECTIVE_DATE" || r.State.Current != nil {
		t.Fatalf("pending import: %+v %v", r, err)
	}
	now = now.Add(time.Minute)
	r, err = service().ImportDelivery(ctx, raw, "production", actor)
	if err != nil || r.Status != "IMPORTED" || r.State.CurrentVersion != 1 || r.State.Pending != nil || r.State.Current.Applications[0].ExpiresAt != start+1000 {
		t.Fatalf("persisted pending retry: %+v %v", r, err)
	}
	var count int64
	if err := db.Model(&domain.Event{}).Where("digest = ? AND kind = ?", digest, "ACTIVATED").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("activation audit: %d %v", count, err)
	}
}
