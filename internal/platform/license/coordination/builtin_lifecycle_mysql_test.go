package coordination

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	licenseapplication "github.com/J-S-Te/Basic-Platform/internal/platform/license/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/infrastructure"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"github.com/J-S-Te/license-core/catalog"
	runtime "github.com/J-S-Te/license-core/runtime"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type builtinMySQLSource struct{}

func (builtinMySQLSource) BuiltinComponents(context.Context, string, string) ([]catalog.Component, error) {
	return []catalog.Component{
		{Application: "contract_management", ServiceID: "contract-api", Role: catalog.RoleAPI, RegistrationProtocol: catalog.RegistrationProtocolV2},
		{Application: "contract_management", ServiceID: "contract-worker", Role: catalog.RoleWorker, RegistrationProtocol: catalog.RegistrationProtocolV2},
	}, nil
}

func TestMySQLReconcileBuiltinComponentsPreservesEnforcement(t *testing.T) {
	dsn := os.Getenv("LICENSE_TEST_DSN")
	if dsn == "" {
		t.Skip("LICENSE_TEST_DSN is not configured")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || (host != "127.0.0.1" && host != "localhost") || cfg.DBName != "platform_license_test" {
		t.Fatal("isolated loopback platform_license_test required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx := context.Background()
	if _, err = migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1900000000, 0).UTC()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	spub, spriv, _ := ed25519.GenerateKey(rand.Reader)
	keys := map[string]ed25519.PublicKey{"test-vendor": pub}
	repo, err := infrastructure.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	lic, err := licenseapplication.NewService(repo, keys, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{TenantID: "builtin-test-tenant", UserID: "builtin-test-user"}
	state, err := lic.Initialize(ctx, licenseapplication.InitializeInput{CustomerID: "builtin-test-customer", Environment: "production"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Where("application = ?", "contract_management").Delete(&LifecycleEvent{}).Error
	_ = db.Where("service_id IN ?", []string{"contract-api", "contract-worker"}).Delete(&RetiredClient{}).Error
	_ = db.Where("application = ?", "contract_management").Delete(&Member{}).Error
	_ = db.Where("application = ?", "contract_management").Delete(&Application{}).Error
	t.Cleanup(func() {
		_ = db.Where("service_id IN ?", []string{"contract-api", "contract-worker"}).Delete(&RetiredClient{}).Error
		_ = db.Where("application = ?", "contract_management").Delete(&Member{}).Error
		_ = db.Where("application = ?", "contract_management").Delete(&Application{}).Error
	})
	legacy := Application{Application: "contract_management", State: runtime.Enforced, RegistrationMode: catalog.LegacyImageV1, Revision: 7, MigrationEligible: true}
	if err = db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for _, serviceID := range []string{"contract-api", "contract-worker"} {
		if err = db.Create(&Member{ServiceID: serviceID, Application: "contract_management", Environment: "production", OAuthClientID: "old-" + serviceID, RegistrationProtocol: 1, CoverageDigest: "sha256:coverage", ImageDigest: "sha256:image", CreatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(db, lic, keys, testSigner{spriv, spub, keys}, func() time.Time { return now }, WithBuiltinComponentSource(builtinMySQLSource{}))
	if err != nil {
		t.Fatal(err)
	}
	if err = service.ReconcileBuiltinComponents(ctx, BuiltinReconcileInput{Application: "contract_management", Environment: "production", OperationID: "builtin-test-op-api", ExpectedRevision: 7, CatalogVersion: catalog.Version, CredentialGeneration: "catalog-1", ServiceIDs: []string{"contract-api"}}, actor); err != nil {
		t.Fatal(err)
	}
	var app Application
	if err = db.First(&app, "application = ?", "contract_management").Error; err != nil {
		t.Fatal(err)
	}
	if app.RegistrationMode != catalog.BridgingComponentV2 || app.CatalogVersion != catalog.Version || app.State != runtime.Enforced || !app.MigrationEligible {
		t.Fatalf("application after partial bridge = %+v", app)
	}
	var partial []Member
	if err = db.Where("application = ? AND retired_at IS NULL", "contract_management").Order("service_id").Find(&partial).Error; err != nil {
		t.Fatal(err)
	}
	for _, member := range partial {
		switch member.ServiceID {
		case "contract-api":
			if member.RegistrationProtocol != catalog.RegistrationProtocolV2 {
				t.Fatalf("selected member not bridged: %+v", member)
			}
		case "contract-worker":
			if member.RegistrationProtocol != 1 || member.OAuthClientID != "old-contract-worker" {
				t.Fatalf("unselected member changed: %+v", member)
			}
		}
	}
	if err = service.ReconcileBuiltinComponents(ctx, BuiltinReconcileInput{Application: "contract_management", Environment: "production", OperationID: "builtin-test-op-worker", ExpectedRevision: app.Revision, CatalogVersion: catalog.Version, CredentialGeneration: "catalog-1", ServiceIDs: []string{"contract-worker"}}, actor); err != nil {
		t.Fatal(err)
	}
	if err = db.First(&app, "application = ?", "contract_management").Error; err != nil {
		t.Fatal(err)
	}
	if app.RegistrationMode != catalog.BridgingComponentV2 || app.CatalogVersion != catalog.Version || app.State != runtime.Enforced || !app.MigrationEligible {
		t.Fatalf("application after full bridge = %+v", app)
	}
	var members []Member
	if err = db.Where("application = ? AND retired_at IS NULL", "contract_management").Order("service_id").Find(&members).Error; err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("member count = %d", len(members))
	}
	for _, member := range members {
		if member.RegistrationProtocol != catalog.RegistrationProtocolV2 || member.CatalogVersion != catalog.Version || member.CredentialGeneration != "catalog-1" || member.CoverageDigest != "" || member.ImageDigest != "" || member.ReadyAt != nil || member.AckAt != nil {
			t.Fatalf("member after bridge = %+v", member)
		}
		want := catalog.ClientIDV2(state.InstanceID, "contract_management", "production", member.ServiceID, "catalog-1")
		if member.OAuthClientID != want {
			t.Fatalf("client identity = %q want %q", member.OAuthClientID, want)
		}
	}
	var retired int64
	if err = db.Model(&RetiredClient{}).Where("service_id IN ?", []string{"contract-api", "contract-worker"}).Count(&retired).Error; err != nil || retired != 2 {
		t.Fatalf("retired clients = %d, err = %v", retired, err)
	}
	// The public status hides tombstones; internal deployment evidence must not.
	if safe, err := service.RuntimeRetirementNotRequired(ctx, "contract_management", app.Revision, []string{"contract-api", "contract-worker"}); err != nil || !safe {
		t.Fatalf("unchanged history: %v %v", safe, err)
	}
	if err := db.Model(&Member{}).Where("application = ? AND service_id = ?", "contract_management", "contract-worker").Update("retired_at", now).Error; err != nil {
		t.Fatal(err)
	}
	if safe, err := service.RuntimeRetirementNotRequired(ctx, "contract_management", app.Revision, []string{"contract-api"}); err != nil || safe {
		t.Fatalf("retired worker disappeared from deployment history: %v %v", safe, err)
	}
	if _, err := service.RuntimeRetirementNotRequired(ctx, "contract_management", app.Revision+1, []string{"contract-api"}); err != ErrConflict {
		t.Fatalf("stale revision accepted: %v", err)
	}

}
