package migrations_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestOperationalModulesOnIsolatedMySQL verifies the complete migration chain used by
// configuration, dictionary, notifications, settings, identity expiry and audit exports.
// The test refuses a non-empty database so it cannot be pointed at a local business or
// production schema by mistake.
func TestOperationalModulesOnIsolatedMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_OPERATIONS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_OPERATIONS_TEST_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var tables int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("operations integration test requires an empty disposable database, found %d tables", tables)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if applied, err := migration.Run(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	} else if len(applied) != 0 {
		t.Fatalf("migration replay applied %d versions", len(applied))
	}

	assertColumns(t, db, "iam_user", "valid_until", "expiry_processed_at")
	assertColumns(t, db, "iam_account", "valid_until", "expiry_processed_at")
	assertColumns(t, db, "iam_personnel_change_request", "handover_reference", "rejection_reason")
	assertColumns(t, db, "iam_personnel_handover_item", "completed_by", "completed_at")
	for _, table := range []string{"cfg_namespace", "cfg_item", "cfg_release", "dict_dictionary", "dict_item", "notification_setting", "notification_template", "notification_delivery", "async_job", "file_object", "file_version", "iam_personnel_handover_item", "iam_personnel_change_transition"} {
		var count int64
		if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("required table %s count=%d", table, count)
		}
	}
	var versions int64
	if err := db.Raw("SELECT COUNT(*) FROM platform_schema_migration WHERE version IN (104,105,107,108)").Scan(&versions).Error; err != nil {
		t.Fatal(err)
	}
	if versions != 4 {
		t.Fatalf("migration 104/105/107/108 count=%d, want 4", versions)
	}
}
