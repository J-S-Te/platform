package infrastructure

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/identity/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/security"
	"github.com/J-S-Te/Basic-Platform/internal/shared/ulid"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestOfflineBootstrapPrincipalMySQL(t *testing.T) {
	dsn := os.Getenv("LICENSE_TEST_DSN")
	if dsn == "" {
		t.Skip("explicit isolated LICENSE_TEST_DSN not configured")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.Net != "tcp" || (!strings.HasPrefix(parsed.Addr, "127.0.0.1:") && !strings.HasPrefix(parsed.Addr, "localhost:")) || parsed.DBName != "platform_license_test" {
		t.Fatal("test requires dedicated loopback platform_license_test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("open isolated test database failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// All first-admin creation and negative mutations stay in this transaction;
	// rollback restores the migration-only installation for other acceptance work.
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var initialized int64
	if err := tx.Table("iam_bootstrap_state").Count(&initialized).Error; err != nil {
		t.Fatal(err)
	}
	if initialized != 0 {
		t.Fatal("isolated bootstrap fixture is not empty; refusing to modify existing administrator")
	}
	repo, err := NewGORMRepository(tx)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewBootstrapService(repo, security.Argon2idPasswordHasher{}, ulid.Generator{}, application.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.InitializeFirstSuperAdmin(ctx, application.BootstrapInput{DisplayName: "Offline isolated administrator", AccountName: "offline.test.admin", Password: "Offline-Test-Only-42!"})
	if err != nil {
		t.Fatal("controlled bootstrap failed", err)
	}
	var before int64
	if err := tx.Table("iam_session").Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p, err := repo.OfflineBootstrapPrincipal(ctx, now)
	if err != nil || p.User.ID != result.UserID || p.Account.ID != result.AccountID || p.SessionID != "" || p.Tenant.Code != "default" {
		t.Fatal("persisted bootstrap resolution failed", err)
	}
	grant := false
	for _, code := range p.PermissionCodes {
		if code == "platform:license:manage" {
			grant = true
		}
	}
	if !grant {
		t.Fatal("migration-owned administrator licensing permissions missing")
	}
	for _, tc := range []struct {
		name, table, column, idColumn, id string
		value, restore                    any
	}{
		{"locked", "iam_account", "locked_until", "id", result.AccountID, now.Add(time.Hour), nil},
		{"account expired", "iam_account", "valid_until", "id", result.AccountID, now.Add(-time.Hour), nil},
		{"user expired", "iam_user", "valid_until", "id", result.UserID, now.Add(-time.Hour), nil},
		{"user deleted", "iam_user", "deleted_at", "id", result.UserID, now, nil},
		{"role revoked", "authz_role", "status", "id", result.RoleID, "DISABLED", "ACTIVE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := tx.Table(tc.table).Where(tc.idColumn+" = ?", tc.id).Update(tc.column, tc.value)
			if update.Error != nil || update.RowsAffected != 1 {
				t.Fatal("fixture mutation failed", update.Error)
			}
			if _, err := repo.OfflineBootstrapPrincipal(ctx, now); err == nil {
				t.Fatal("invalid administrator accepted")
			}
			if err := tx.Table(tc.table).Where(tc.idColumn+" = ?", tc.id).Update(tc.column, tc.restore).Error; err != nil {
				t.Fatal("fixture restore failed", err)
			}
		})
	}
	var after int64
	if err := tx.Table("iam_session").Count(&after).Error; err != nil || after != before {
		t.Fatal("maintenance principal created browser sessions", err)
	}
}
