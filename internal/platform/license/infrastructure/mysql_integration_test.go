package infrastructure_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
	core "github.com/J-S-Te/license-core"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// Only an explicitly supplied disposable local license test database is eligible.
// No application DSN/config/environment fallback is permitted.
func isolatedDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("LICENSE_TEST_DSN")
	if dsn == "" {
		t.Skip("LICENSE_TEST_DSN is not configured")
	}
	cfg, e := mysqldriver.ParseDSN(dsn)
	if e != nil {
		t.Fatal("invalid LICENSE_TEST_DSN")
	}
	host, _, e := net.SplitHostPort(cfg.Addr)
	if e != nil || cfg.Net != "tcp" || (host != "127.0.0.1" && host != "localhost") || cfg.DBName != "platform_license_test" {
		t.Fatal("LICENSE_TEST_DSN must target local TCP platform_license_test")
	}
	db, e := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sql, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	sql.SetMaxOpenConns(8)
	t.Cleanup(func() {
		if e := sql.Close(); e != nil {
			t.Error(e)
		}
	})
	return db
}
func TestLicenseMySQLMigrationAndLifecycle(t *testing.T) {
	db := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	applied, e := migration.Run(ctx, db, migrations.Files)
	if e != nil {
		t.Fatal("full migration:", e)
	}
	if len(applied) != 117 && len(applied) != 0 {
		t.Fatalf("expected complete initial migration or an already migrated disposable database: applied=%d want117/0", len(applied))
	}
	t.Logf("initial migration applied=%d; expecting all 117 registered versions", len(applied))
	again, e := migration.Run(ctx, db, migrations.Files)
	if e != nil || len(again) != 0 {
		t.Fatalf("migration replay %d %v", len(again), e)
	}
	var migrationsCount int64
	if e = db.Table("platform_schema_migration").Count(&migrationsCount).Error; e != nil || migrationsCount != 117 {
		t.Fatalf("registered migrations %d %v", migrationsCount, e)
	}
	var versions []uint64
	if e = db.Table("platform_schema_migration").Order("version ASC").Pluck("version", &versions).Error; e != nil {
		t.Fatal(e)
	}
	for index, version := range versions {
		if version != uint64(index+1) {
			t.Fatalf("missing migration version: index=%d version=%d", index, version)
		}
	}
	repo, e := infrastructure.NewRepository(db)
	if e != nil {
		t.Fatal(e)
	}
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Unix(1900000000, 0)
	service, e := application.NewService(repo, map[string]ed25519.PublicKey{"test-vendor": pub}, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	actor := domain.Actor{TenantID: "license-test-tenant", UserID: "license-test-operator"}
	in := application.InitializeInput{CustomerID: "license-test-customer", Environment: "production"}
	initial, e := service.Initialize(ctx, in, actor)
	if e != nil || initial.CurrentVersion != 0 {
		t.Fatalf("init %+v %v", initial, e)
	}
	digests := []string{}
	recoveryID := "license-test-recovery-1"
	t.Cleanup(func() {
		// Delete only this lifecycle's explicit rows; migration seed data remains.
		if e := db.Where("id = ?", recoveryID).Delete(&domain.ClockRecovery{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("digest IN ? OR (tenant_id = ? AND user_id = ?)", digests, actor.TenantID, actor.UserID).Delete(&domain.Event{}).Error; e != nil {
			t.Error(e)
		}
		if e := db.Where("id = ? AND instance_id = ?", 1, initial.InstanceID).Delete(&domain.Deployment{}).Error; e != nil {
			t.Error(e)
		}
		if len(digests) > 0 {
			if e := db.Where("digest IN ?", digests).Delete(&domain.Artifact{}).Error; e != nil {
				t.Error(e)
			}
		}
	})
	repeated, e := service.Initialize(ctx, in, actor)
	if e != nil || repeated.InstanceID != initial.InstanceID {
		t.Fatalf("unstable init %+v %v", repeated, e)
	}
	_, e = service.Initialize(ctx, application.InitializeInput{CustomerID: "wrong", Environment: "production"}, actor)
	if !errors.Is(e, domain.ErrConflict) {
		t.Fatal("binding overwrite accepted", e)
	}
	sign := func(version uint64, nbf int64, apps []core.Application) string {
		t.Helper()
		if apps == nil {
			apps = []core.Application{{Code: "contract_management", NotBefore: nbf, ExpiresAt: nbf + 86400, Kind: "FULL"}, {Code: "settlement", NotBefore: nbf, ExpiresAt: nbf + 86400, Kind: "FULL"}}
		}
		raw, e := core.Sign(core.License{ProtocolVersion: 1, Issuer: core.Issuer, ID: "license-mysql", Version: version, CustomerID: in.CustomerID, ProductID: core.Product, Environment: in.Environment, InstanceID: initial.InstanceID, IssuedAt: nbf, NotBefore: nbf, Applications: apps}, "test-vendor", priv)
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	previewInput := func(raw string) application.CommitInput {
		t.Helper()
		p, e := service.Preview(ctx, raw)
		if e != nil {
			t.Fatal(e)
		}
		digests = append(digests, p.Digest)
		return application.CommitInput{RawJWS: raw, Digest: p.Digest, ExpectedRevision: p.Revision, ExpectedCurrentVersion: p.CurrentVersion, ExpectedPendingDigest: p.PendingDigest, ConfirmChanges: true, ConfirmReplacePending: true}
	}
	count := func(model any) int64 {
		t.Helper()
		var n int64
		if e := db.Model(model).Count(&n).Error; e != nil {
			t.Fatal(e)
		}
		return n
	}
	first := previewInput(sign(1, now.Unix(), nil))
	if count(&domain.Artifact{}) != 0 || count(&domain.Event{}) != 1 {
		t.Fatal("preview wrote")
	}
	if _, e = service.Preview(ctx, "invalid"); e == nil || count(&domain.Artifact{}) != 0 {
		t.Fatal("invalid preview wrote/accepted", e)
	}
	if _, e = service.Commit(ctx, first, domain.Actor{}); !errors.Is(e, domain.ErrInvalid) {
		t.Fatal("blank actor accepted", e)
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, e := service.Commit(ctx, first, actor); results <- e }()
	}
	close(start)
	wg.Wait()
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, domain.ErrConflict)) || (b == nil && errors.Is(a, domain.ErrConflict))) {
		t.Fatalf("concurrent commit %v %v", a, b)
	}
	if count(&domain.Artifact{}) != 1 || count(&domain.Event{}) != 2 {
		t.Fatal("non-atomic concurrent persistence")
	}
	page, e := service.Events(ctx, 1, 1)
	if e != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].UserID != actor.UserID || page.Items[0].Version != 1 {
		t.Fatalf("audit pagination %+v %v", page, e)
	}
	reduced := []core.Application{{Code: "contract_management", NotBefore: now.Unix() + 3600, ExpiresAt: now.Unix() + 86400, Kind: "FULL"}}
	second := previewInput(sign(2, now.Unix()+3600, reduced))
	second.ConfirmChanges = false
	if _, e = service.Commit(ctx, second, actor); !errors.Is(e, domain.ErrConfirmation) {
		t.Fatal("reduction accepted without confirmation", e)
	}
	second.ConfirmChanges = true
	st, e := service.Commit(ctx, second, actor)
	if e != nil || st.CurrentVersion != 1 || st.Pending == nil || st.HighestVersion != 2 {
		t.Fatalf("pending %+v %v", st, e)
	}
	st, e = service.Read(ctx)
	if e != nil || st.CurrentVersion != 1 || len(st.Current.Applications) != 2 {
		t.Fatal("pending activated early", e)
	}
	if _, e = service.Preview(ctx, sign(2, now.Unix(), nil)); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("highest rollback accepted", e)
	}
	third := previewInput(sign(3, now.Unix()+3600, reduced))
	third.ConfirmReplacePending = false
	if _, e = service.Commit(ctx, third, actor); !errors.Is(e, domain.ErrConfirmation) {
		t.Fatal("pending replacement without confirmation", e)
	}
	third.ConfirmReplacePending = true
	if _, e = service.Commit(ctx, third, actor); e != nil {
		t.Fatal(e)
	}
	now = now.Add(time.Hour)
	st, e = service.Read(ctx)
	if e != nil || st.CurrentVersion != 3 || st.Pending != nil || len(st.Current.Applications) != 1 || st.HighestVersion != 3 {
		t.Fatalf("whole activation %+v %v", st, e)
	}
	now = now.Add(-time.Hour)
	if _, e = service.Read(ctx); !errors.Is(e, domain.ErrClock) {
		t.Fatal("rollback not rejected", e)
	}
	var persisted domain.Deployment
	if e = db.Take(&persisted, 1).Error; e != nil || !persisted.ClockBlocked {
		t.Fatal("rollback latch not persisted", e)
	}
	if _, e = service.Request(ctx); e != nil {
		t.Fatal("request blocked by clock", e)
	}
	recovery := core.Recovery{ProtocolVersion: 1, Issuer: core.Issuer, ID: recoveryID, ProductID: core.Product, InstanceID: initial.InstanceID, Environment: in.Environment, LicenseID: "license-mysql", LicenseVersion: 3, IssuedAt: now.Unix(), ExpiresAt: now.Unix() + 600, AnchorAt: now.Unix()}
	raw, e := core.SignRecovery(recovery, "test-vendor", priv)
	if e != nil {
		t.Fatal(e)
	}
	st, e = service.RestoreClock(ctx, raw, actor)
	if e != nil || st.ClockBlocked || st.HighestObservedAt != now.Unix() {
		t.Fatalf("recovery %+v %v", st, e)
	}
	if _, e = service.RestoreClock(ctx, raw, actor); !errors.Is(e, domain.ErrConflict) {
		t.Fatal("recovery replay accepted", e)
	}
	if count(&domain.ClockRecovery{}) != 1 {
		t.Fatal("recovery consumed incorrectly")
	}
	if e = db.Model(&domain.Artifact{}).Where("digest = ?", st.CurrentDigest).Update("raw_jws", "corrupt").Error; e != nil {
		t.Fatal(e)
	}
	if _, e = service.Read(ctx); !errors.Is(e, domain.ErrCorrupt) {
		t.Fatal("tampered raw accepted", e)
	}
}
