package migrations_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/infrastructure"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// This test writes only to an explicitly supplied, empty disposable database.
func TestBootstrapAfterFullMigrationChainOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_BOOTSTRAP_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_BOOTSTRAP_TEST_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	var tables int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("bootstrap integration test requires an empty disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db = db.WithContext(ctx)

	// Reproduce the old complete migration chain, not a hand-built approximation.
	historical := fstest.MapFS{}
	items, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	const repairName = "000103_restore_uninitialized_bootstrap_position.sql"
	for _, item := range items {
		if item.IsDir() || item.Name() == repairName {
			continue
		}
		data, err := migrations.Files.ReadFile(item.Name())
		if err != nil {
			t.Fatal(err)
		}
		historical[item.Name()] = &fstest.MapFile{Data: data}
	}
	if _, err := migration.Run(ctx, db, historical); err != nil {
		t.Fatal(err)
	}
	repo, err := infrastructure.NewGORMRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewBootstrapService(repo, security.Argon2idPasswordHasher{}, ulid.Generator{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	input := application.BootstrapInput{DisplayName: "Integration Admin", AccountName: "bootstrap.integration", Password: "Disposable-Test-Password9!"}
	if _, err := service.InitializeFirstSuperAdmin(ctx, input); !errors.Is(err, application.ErrBootstrapUnavailable) {
		t.Fatalf("historical migrations should reproduce unavailable bootstrap, got %v", err)
	}
	assertBootstrapCount(t, db, "iam_user", 0)
	assertBootstrapCount(t, db, "iam_bootstrap_state", 0)
	if applied, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	} else if len(applied) != 1 {
		t.Fatalf("repair applied %d migrations, want 1", len(applied))
	}
	if applied, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	} else if len(applied) != 0 {
		t.Fatal("migration rerun was not a no-op")
	}
	assertPositionStatus(t, db, "01J00000000000000000000400", "ACTIVE")
	for _, id := range []string{"01J00000000000000000000401", "01J00000000000000000000402", "01J00000000000000000000403", "01J00000000000000000000404", "01J00000000000000000000405"} {
		assertPositionStatus(t, db, id, "DISABLED")
	}
	repair, err := migrations.Files.ReadFile(repairName)
	if err != nil {
		t.Fatal(err)
	}
	if replay := db.Exec(string(repair)); replay.Error != nil {
		t.Fatal(replay.Error)
	} else if replay.RowsAffected != 0 {
		t.Fatal("repair SQL replay changed an already active position")
	}
	result, err := service.InitializeFirstSuperAdmin(ctx, input)
	if err != nil {
		t.Fatalf("bootstrap after repair: %v", err)
	}
	if result.RoleCode != application.BootstrapSuperAdminRoleCode {
		t.Fatalf("unexpected role %q", result.RoleCode)
	}
	assertBootstrapCount(t, db, "iam_user", 1)
	assertBootstrapCount(t, db, "iam_account", 1)
	assertBootstrapCount(t, db, "iam_membership", 1)
	assertBootstrapCount(t, db, "iam_bootstrap_state", 1)
	if _, err := service.InitializeFirstSuperAdmin(ctx, input); !errors.Is(err, application.ErrBootstrapAlreadyInitialized) {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	assertBootstrapCount(t, db, "iam_user", 1)

	// Even manual SQL replay must preserve an initialized administrator's decision.
	if err := db.Exec("UPDATE iam_position SET status = 'DISABLED' WHERE id = ?", "01J00000000000000000000400").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(repair)).Error; err != nil {
		t.Fatal(err)
	}
	assertPositionStatus(t, db, "01J00000000000000000000400", "DISABLED")
}

func assertBootstrapCount(t *testing.T, db *gorm.DB, table string, want int64) {
	t.Helper()
	var got int64
	if err := db.Table(table).Count(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}

func assertPositionStatus(t *testing.T, db *gorm.DB, id, want string) {
	t.Helper()
	var status string
	if err := db.Table("iam_position").Select("status").Where("id = ?", id).Scan(&status).Error; err != nil {
		t.Fatal(err)
	}
	if status != want {
		t.Fatalf("position %s status = %q, want %q", id, status, want)
	}
}
