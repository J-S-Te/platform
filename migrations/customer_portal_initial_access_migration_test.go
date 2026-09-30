package migrations_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/migration"
	"github.com/J-S-Te/Basic-Platform/migrations"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestCustomerPortalInitialAdministratorRepairIsNarrowAndDoesNotGrantDirectly(t *testing.T) {
	content, err := migrations.Files.ReadFile("000112_repair_customer_portal_initial_administrator.sql")
	if err != nil {
		t.Fatalf("read customer portal initial access migration: %v", err)
	}
	sql := string(content)
	for _, fragment := range []string{
		"application.code = 'customer_portal'",
		"deployment.application_code = 'customer_portal'",
		"deployment.environment_code = 'prod'",
		"deployment.initial_access_assigned_at IS NOT NULL",
		"SET deployment.initial_access_assigned_at = NULL",
		"role.code = 'portal_super_admin'",
		"binding.subject_type = 'USER'",
		"binding.scope_type = 'TENANT'",
		"binding.status = 'ACTIVE'",
		"role.role_type = 'APPLICATION'",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("customer portal initial access migration is missing %q", fragment)
		}
	}
	upper := strings.ToUpper(sql)
	if strings.Contains(upper, "INSERT INTO AUTHZ_ROLE_BINDING") || strings.Contains(upper, "UPDATE AUTHZ_ROLE_BINDING") {
		t.Fatal("migration must not grant or reactivate authorization directly")
	}
}

// TestCustomerPortalInitialAdministratorRepairOnMySQL writes only to an explicitly supplied,
// empty disposable schema. It exercises the real historical chain through 111, followed by the
// forward repair, because MySQL multi-table UPDATE and correlated NOT EXISTS semantics cannot be
// established by a parser-only test.
func TestCustomerPortalInitialAdministratorRepairOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_PORTAL_ACCESS_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_PORTAL_ACCESS_MIGRATION_TEST_DSN is not configured")
	}
	database, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	var tables int64
	if err := database.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("customer portal migration test requires an empty disposable database, found %d tables", tables)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database = database.WithContext(ctx)

	historical := fstest.MapFS{}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "000112_repair_customer_portal_initial_administrator.sql" {
			continue
		}
		data, readErr := migrations.Files.ReadFile(entry.Name())
		if readErr != nil {
			t.Fatal(readErr)
		}
		historical[entry.Name()] = &fstest.MapFile{Data: data}
	}
	if _, err := migration.Run(ctx, database, historical); err != nil {
		t.Fatalf("run historical migrations: %v", err)
	}

	const (
		tenantID        = "01J00000000000000000000000"
		adminUserID     = "01K00000000000000000000001"
		portalAppID     = "01K00000000000000000000002"
		portalProdEnvID = "01K00000000000000000000003"
		portalDevEnvID  = "01K00000000000000000000004"
		otherAppID      = "01K00000000000000000000005"
		otherProdEnvID  = "01K00000000000000000000006"
		portalRoleID    = "01K00000000000000000000007"
		portalBindingID = "01K00000000000000000000008"
	)
	if err := database.Exec(`INSERT INTO iam_user
		(id,tenant_id,display_name,employment_status,status,version,created_at,updated_at)
		VALUES (?,?,'Portal Migration Admin','ACTIVE','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`, adminUserID, tenantID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec(`INSERT INTO platform_application
		(id,tenant_id,code,name,application_type,status,version,created_at,updated_at) VALUES
		(?,?,'customer_portal','Portal','WEB','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)),
		(?,?,'migration_other','Other','WEB','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		portalAppID, tenantID, otherAppID, tenantID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec(`INSERT INTO platform_application_environment
		(id,tenant_id,application_id,environment,status,version,created_at,updated_at) VALUES
		(?,?,?,'prod','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)),
		(?,?,?,'dev','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)),
		(?,?,?,'prod','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		portalProdEnvID, tenantID, portalAppID,
		portalDevEnvID, tenantID, portalAppID,
		otherProdEnvID, tenantID, otherAppID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec(`INSERT INTO subsystem_deployment_state
		(tenant_id,application_id,environment_id,application_code,environment_code,initial_admin_user_id,
		 initial_access_assigned_at,status,operation,generation,attempt_count,created_at,updated_at) VALUES
		(?,?,?,'customer_portal','prod',?,UTC_TIMESTAMP(3),'READY','ADOPT',1,1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)),
		(?,?,?,'customer_portal','dev',?,UTC_TIMESTAMP(3),'READY','ADOPT',1,1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3)),
		(?,?,?,'migration_other','prod',?,UTC_TIMESTAMP(3),'READY','ADOPT',1,1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		tenantID, portalAppID, portalProdEnvID, adminUserID,
		tenantID, portalAppID, portalDevEnvID, adminUserID,
		tenantID, otherAppID, otherProdEnvID, adminUserID).Error; err != nil {
		t.Fatal(err)
	}

	if applied, err := migration.Run(ctx, database, migrations.Files); err != nil {
		t.Fatalf("apply portal repair migration: %v", err)
	} else if len(applied) != 1 || applied[0].Version != 112 {
		t.Fatalf("applied migrations = %+v, want only 112", applied)
	}
	assertInitialAccessMarker(t, database, tenantID, portalProdEnvID, false)
	assertInitialAccessMarker(t, database, tenantID, portalDevEnvID, true)
	assertInitialAccessMarker(t, database, tenantID, otherProdEnvID, true)
	if applied, err := migration.Run(ctx, database, migrations.Files); err != nil {
		t.Fatal(err)
	} else if len(applied) != 0 {
		t.Fatalf("migration replay applied %d versions", len(applied))
	}

	migrationSQL, err := migrations.Files.ReadFile("000112_repair_customer_portal_initial_administrator.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Exec(`INSERT INTO authz_role
		(id,tenant_id,application_id,code,name,role_type,built_in,status,version,created_at,updated_at)
		VALUES (?,?,?,'portal_super_admin','Portal Super Admin','APPLICATION',1,'ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		portalRoleID, tenantID, portalAppID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec(`INSERT INTO authz_role_binding
		(id,tenant_id,application_id,role_id,subject_type,subject_id,scope_type,scope_id,status,version,created_at,updated_at)
		VALUES (?,?,?,?,'USER',?,'TENANT','','ACTIVE',1,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))`,
		portalBindingID, tenantID, portalAppID, portalRoleID, adminUserID).Error; err != nil {
		t.Fatal(err)
	}
	setInitialAccessMarker(t, database, tenantID, portalProdEnvID, adminUserID)
	if err := database.Exec(string(migrationSQL)).Error; err != nil {
		t.Fatalf("replay repair with active binding: %v", err)
	}
	assertInitialAccessMarker(t, database, tenantID, portalProdEnvID, true)

	if err := database.Table("authz_role_binding").Where("id = ?", portalBindingID).Updates(map[string]any{"status": "DISABLED", "updated_at": time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Exec(string(migrationSQL)).Error; err != nil {
		t.Fatalf("replay repair with disabled binding: %v", err)
	}
	assertInitialAccessMarker(t, database, tenantID, portalProdEnvID, false)

	if err := database.Table("authz_role_binding").Where("id = ?", portalBindingID).Updates(map[string]any{
		"status": "ACTIVE", "valid_until": time.Now().UTC().Add(-time.Minute), "updated_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	setInitialAccessMarker(t, database, tenantID, portalProdEnvID, adminUserID)
	if err := database.Exec(string(migrationSQL)).Error; err != nil {
		t.Fatalf("replay repair with expired binding: %v", err)
	}
	assertInitialAccessMarker(t, database, tenantID, portalProdEnvID, false)
}

func setInitialAccessMarker(t *testing.T, database *gorm.DB, tenantID, environmentID, userID string) {
	t.Helper()
	result := database.Table("subsystem_deployment_state").Where("tenant_id = ? AND environment_id = ?", tenantID, environmentID).
		Updates(map[string]any{"initial_admin_user_id": userID, "initial_access_assigned_at": time.Now().UTC()})
	if result.Error != nil || result.RowsAffected != 1 {
		t.Fatalf("set initial access marker: rows=%d error=%v", result.RowsAffected, result.Error)
	}
}

func assertInitialAccessMarker(t *testing.T, database *gorm.DB, tenantID, environmentID string, wantPresent bool) {
	t.Helper()
	var row struct {
		AssignedAt *time.Time `gorm:"column:initial_access_assigned_at"`
	}
	if err := database.Table("subsystem_deployment_state").Select("initial_access_assigned_at").
		Where("tenant_id = ? AND environment_id = ?", tenantID, environmentID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if got := row.AssignedAt != nil; got != wantPresent {
		t.Fatalf("initial access marker for %s present=%t, want %t", environmentID, got, wantPresent)
	}
}
