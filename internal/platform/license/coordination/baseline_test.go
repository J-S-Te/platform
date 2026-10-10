package coordination

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
	runtime "github.com/J-S-Te/license-core/runtime"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func baselineFixture(now time.Time, business bool) evidence.Report {
	r := evidence.Report{Scope: "MIGRATION_INSTALLATION", BoundarySupported: true, Complete: true, Project: "baseline-test", CollectedAt: now}
	if business {
		r.Facts = []evidence.Fact{{Application: "contract_management", Environment: "prod", Service: "contract-api", ContainerID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ImageDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Running: true, StartedAt: now.Add(-time.Hour)}}
	}
	return r
}

func TestBaselineFourScenariosAndBoundaries(t *testing.T) {
	now := time.Unix(1900000000, 0)
	for _, scenario := range []string{"fresh", "platform-only", "migrate", "expand"} {
		business := scenario == "migrate" || scenario == "expand"
		if err := validateBaselineReport(scenario, baselineFixture(now, business), "prod", now, true); err != nil {
			t.Fatal(scenario, err)
		}
		if err := validateBaselineReport(scenario, baselineFixture(now, !business), "prod", now, true); !errors.Is(err, ErrNotReady) {
			t.Fatal("wrong presence accepted", scenario)
		}
	}
	for _, change := range []func(*evidence.Report){
		func(r *evidence.Report) { r.Scope = "INSTALLATION" },
		func(r *evidence.Report) { r.Complete = false },
		func(r *evidence.Report) { r.BoundarySupported = false },
		func(r *evidence.Report) { r.Problems = []string{"unknown execution unit"} },
		func(r *evidence.Report) { r.Project = "../project" },
		func(r *evidence.Report) { r.CollectedAt = now.Add(-61 * time.Second) },
		func(r *evidence.Report) { r.Facts[0].Application = "platform" },
		func(r *evidence.Report) { r.Facts[0].Environment = "production" },
		func(r *evidence.Report) { r.Facts[0].ContainerID = "short-id" },
		func(r *evidence.Report) { r.Facts[0].ImageDigest = "latest" },
		func(r *evidence.Report) { r.Facts[0].StartedAt = now.Add(time.Second) },
		func(r *evidence.Report) { r.Facts[0].Running = false },
		func(r *evidence.Report) { r.Facts = append(r.Facts, r.Facts[0]) },
	} {
		r := baselineFixture(now, true)
		change(&r)
		if err := validateBaselineReport("migrate", r, "prod", now, true); !errors.Is(err, ErrNotReady) {
			t.Fatal("unsafe baseline accepted", r)
		}
	}
	// Old image evidence legitimately has neither version nor protocol labels.
	s := &Service{now: func() time.Time { return now }, applicationEnvironment: "prod"}
	r := baselineFixture(now, true)
	raw, err := baselineJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	b := InstallationBaseline{ID: 1, Scenario: "migrate", Project: r.Project, InstanceID: "instance", Environment: "production", EvidenceJSON: raw, EvidenceDigest: hash(raw), CollectedAt: now}
	d := &domain.Deployment{InstanceID: "instance", Environment: "production"}
	if _, err = s.baselineReport(b, d); err != nil {
		t.Fatal(err)
	}
	b.EvidenceJSON += " "
	if _, err = s.baselineReport(b, d); !errors.Is(err, domain.ErrCorrupt) {
		t.Fatal("modified evidence accepted")
	}
}

func TestBaselineCollectionTimestampSQLPrecision(t *testing.T) {
	base := time.Unix(1900000000, 0).UTC()
	now := base.Add(time.Second)
	s := &Service{now: func() time.Time { return now }, applicationEnvironment: "prod"}
	d := &domain.Deployment{InstanceID: "instance", Environment: "production"}
	for _, tt := range []struct {
		name       string
		nanos      int
		projection func(time.Time) time.Time
		valid      bool
	}{
		{"rounded-up", 177605754, func(v time.Time) time.Time { return v.Round(time.Millisecond) }, true},
		{"rounded-down", 177405754, func(v time.Time) time.Time { return v.Round(time.Millisecond) }, true},
		{"truncated", 177605754, func(v time.Time) time.Time { return v.Truncate(time.Millisecond) }, true},
		{"second-carry", 999605754, func(v time.Time) time.Time { return v.Round(time.Millisecond) }, true},
		{"pre-insert", 177605754, func(v time.Time) time.Time { return v }, true},
		{"extra-millisecond", 177605754, func(v time.Time) time.Time { return v.Round(time.Millisecond).Add(time.Millisecond) }, false},
		{"wrong-submillisecond", 177605754, func(v time.Time) time.Time { return v.Add(time.Nanosecond) }, false},
		{"future-evidence", 1000000001, func(v time.Time) time.Time { return v.Round(time.Millisecond) }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := baselineFixture(base.Add(time.Duration(tt.nanos)), true)
			raw, err := baselineJSON(r)
			if err != nil {
				t.Fatal(err)
			}
			b := InstallationBaseline{ID: 1, Scenario: "migrate", Project: r.Project, InstanceID: d.InstanceID, Environment: d.Environment, EvidenceJSON: raw, EvidenceDigest: hash(raw), CollectedAt: tt.projection(r.CollectedAt)}
			if _, err := s.baselineReport(b, d); (err == nil) != tt.valid {
				t.Fatalf("timestamp precision accepted incorrectly: %v", err)
			}
			b.EvidenceJSON += " "
			if _, err := s.baselineReport(b, d); !errors.Is(err, domain.ErrCorrupt) {
				t.Fatal("timestamp compatibility weakened digest check")
			}
		})
	}
}

// This uses a separate database from every production and other fixture DB.
func TestMySQLInstallationBaseline(t *testing.T) {
	dsn := os.Getenv("LICENSE_BASELINE_TEST_DSN")
	if dsn == "" {
		t.Skip("LICENSE_BASELINE_TEST_DSN not configured")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid isolated DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || (host != "localhost" && host != "127.0.0.1") || cfg.DBName != "platform_license_baseline_test" {
		t.Fatal("isolated loopback platform_license_baseline_test required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	if _, err = migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1900000000, 123605754)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"test-vendor": pub}
	repo, err := infrastructure.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	lic, err := application.NewService(repo, keys, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewService(db, lic, keys, testSigner{priv: priv, pub: pub, vendor: keys}, func() time.Time { return now }, WithApplicationEnvironment("prod"))
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{TenantID: "baseline-tenant", UserID: "baseline-user"}
	reset := func() {
		for _, model := range []any{&InstallationBaseline{}, &Inventory{}, &Member{}, &Application{}, &LifecycleEvent{}, &RetiredClient{}, &domain.Event{}, &domain.Artifact{}, &domain.Deployment{}} {
			if err := db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(model).Error; err != nil {
				t.Fatal(err)
			}
		}
		if _, err := lic.Initialize(ctx, application.InitializeInput{CustomerID: "baseline-customer", Environment: "production"}, actor); err != nil {
			t.Fatal(err)
		}
	}
	for _, scenario := range []string{"fresh", "platform-only", "migrate", "expand"} {
		t.Run(scenario, func(t *testing.T) {
			reset()
			legacy := scenario == "migrate" || scenario == "expand"
			b, err := s.PrepareInstallation(ctx, scenario, baselineFixture(now, legacy), actor)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 4)
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repeat, e := s.PrepareInstallation(ctx, scenario, baselineFixture(now, legacy), actor)
					if e == nil && repeat.EvidenceDigest != b.EvidenceDigest {
						e = ErrConflict
					}
					errs <- e
				}()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				if e != nil {
					t.Fatal(e)
				}
			}
			if _, err = s.ReadInstallationBaseline(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario != "expand" {
				added := baselineFixture(now, true)
				added.Facts[0].Application = "settlement"
				if _, err = s.PrepareInstallation(ctx, "expand", added, actor); err != nil {
					t.Fatal(err)
				}
			}
			for _, app := range []string{"contract_management", "settlement"} {
				spec := ServiceSpec{Application: app, Environment: "prod", ServiceID: app + "-api", OAuthClientID: app + "-client", ImageDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", CoverageDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}
				if err = s.Register(ctx, spec); err != nil {
					t.Fatal(err)
				}
				st, err := s.Status(ctx, app)
				if err != nil {
					t.Fatal(err)
				}
				if st.MigrationEligible != (legacy && app == "contract_management") {
					t.Fatal("eligibility leaked", app)
				}
				if app == "contract_management" && legacy {
					spec.OAuthClientID += "-replacement"
					spec.ImageDigest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
					s.lifecycleApproval = fixtureApproval{LifecycleApproval{Specs: []ServiceSpec{spec}, RequiredServiceIDs: []string{spec.ServiceID}}}
					in := LifecycleInput{Application: app, Environment: "prod", ExpectedRevision: st.Revision, OperationID: "baseline-first-replacement", ReleaseDigest: spec.ImageDigest}
					if err = s.ReconcileComponents(ctx, in, actor); err != nil {
						t.Fatal(err)
					}
					st, err = s.Status(ctx, app)
					if err != nil || !st.MigrationEligible || st.State != runtime.Pending {
						t.Fatal("first rollout erased qualification", err)
					}
					if err = db.Model(&Application{}).Where("application = ?", app).Updates(map[string]any{"state": runtime.Enforced, "migration_eligible": false}).Error; err != nil {
						t.Fatal(err)
					}
					spec.OAuthClientID += "-v3"
					spec.ImageDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
					s.lifecycleApproval = fixtureApproval{LifecycleApproval{Specs: []ServiceSpec{spec}, RequiredServiceIDs: []string{spec.ServiceID}}}
					in.ExpectedRevision = st.Revision
					in.OperationID = "baseline-enforced-replacement"
					in.ReleaseDigest = spec.ImageDigest
					if err = s.ReconcileComponents(ctx, in, actor); err != nil {
						t.Fatal(err)
					}
					st, err = s.Status(ctx, app)
					if err != nil || st.MigrationEligible || st.State != runtime.Applying {
						t.Fatal("enforced qualification restored", err)
					}
				}
			}
			old := baselineFixture(now, true)
			old.Scope = "INSTALLATION"
			old.Facts[0].Protocol = "1"
			old.Facts[0].Version = "v1"
			f := old.Facts[0]
			oldSpec := ServiceSpec{Application: f.Application, Environment: f.Environment, ServiceID: f.Service, OAuthClientID: "old-client", ImageDigest: f.ImageDigest, CoverageDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}
			if err := s.FreezeInventory(ctx, old, []ServiceSpec{oldSpec}); !errors.Is(err, ErrConflict) {
				t.Fatal("old freeze replaced baseline")
			}
		})
	}
	reset()
	var wg sync.WaitGroup
	firstErrors := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PrepareInstallation(ctx, "migrate", baselineFixture(now, true), actor)
			firstErrors <- err
		}()
	}
	wg.Wait()
	close(firstErrors)
	for err := range firstErrors {
		if err != nil {
			t.Fatal("concurrent first freeze", err)
		}
	}
	var count int64
	if err := db.Model(&InstallationBaseline{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("freeze not singleton", err)
	}
	reset()
	old := baselineFixture(now, true)
	old.Scope = "INSTALLATION"
	old.Facts[0].Protocol = "1"
	old.Facts[0].Version = "v1"
	f := old.Facts[0]
	spec := ServiceSpec{Application: f.Application, Environment: f.Environment, ServiceID: f.Service, OAuthClientID: "legacy-client", ImageDigest: f.ImageDigest, CoverageDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}
	if err := s.FreezeInventory(ctx, old, []ServiceSpec{spec}); err != nil {
		t.Fatal("old freeze compatibility", err)
	}
	if _, err := s.PrepareInstallation(ctx, "migrate", baselineFixture(now, true), actor); !errors.Is(err, ErrConflict) {
		t.Fatal("existing old inventory replaced", err)
	}
	reset()
}
