package infrastructure

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type accessCatalogStub struct {
	capabilities application.SubsystemProvisioningCapabilities
	err          error
}

func (stub accessCatalogStub) AvailableCapabilities(context.Context) (application.SubsystemProvisioningCapabilities, error) {
	return stub.capabilities, stub.err
}

type accessReadinessStub struct {
	readiness SubsystemAccessReadiness
	err       error
}

func (stub accessReadinessStub) ReadSubsystemAccessReadiness(context.Context, string, string, string) (SubsystemAccessReadiness, error) {
	return stub.readiness, stub.err
}

// 真实数据库验证事实源：范围来自清单、登记与受控部署来自真实表、预览版本随事实变化。
func TestSubsystemAccessFactsAdapterOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	statements := []string{
		"DROP TABLE IF EXISTS subsystem_deployment_state",
		"DROP TABLE IF EXISTS platform_application_environment",
		"DROP TABLE IF EXISTS platform_application",
		`CREATE TABLE platform_application (
			id CHAR(26) NOT NULL, tenant_id CHAR(26) NOT NULL, code VARCHAR(64) NOT NULL, status VARCHAR(16) NOT NULL,
			PRIMARY KEY (id), UNIQUE KEY uq_platform_application (tenant_id, code)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE platform_application_environment (
			id CHAR(26) NOT NULL, tenant_id CHAR(26) NOT NULL, application_id CHAR(26) NOT NULL,
			environment VARCHAR(16) NOT NULL, status VARCHAR(16) NOT NULL,
			PRIMARY KEY (id), UNIQUE KEY uq_platform_environment (tenant_id, application_id, environment)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE subsystem_deployment_state (
			tenant_id CHAR(26) NOT NULL, application_id CHAR(26) NOT NULL, environment_id CHAR(26) NOT NULL,
			application_code VARCHAR(64) NOT NULL, environment_code VARCHAR(16) NOT NULL,
			status VARCHAR(32) NOT NULL, operation VARCHAR(32) NOT NULL, generation BIGINT UNSIGNED NOT NULL DEFAULT 1,
			attempt_count INT UNSIGNED NOT NULL DEFAULT 0, created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
			PRIMARY KEY (tenant_id, environment_id), UNIQUE KEY uk_subsystem_deployment_environment (tenant_id, application_id, environment_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	now := time.Now().UTC()
	if err := db.Exec("INSERT INTO platform_application (id, tenant_id, code, status) VALUES (?,?,?,?)",
		"01APP000000000000000000001", "tenant-1", "settlement", "ACTIVE").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO platform_application_environment (id, tenant_id, application_id, environment, status) VALUES (?,?,?,?,?)",
		"01ENV000000000000000000001", "tenant-1", "01APP000000000000000000001", "dev", "ACTIVE").Error; err != nil {
		t.Fatal(err)
	}

	catalog := accessCatalogStub{capabilities: application.SubsystemProvisioningCapabilities{
		Mode: "local", SupportedApplicationCodes: []string{"settlement", "contract"}, SupportedEnvironments: []string{"dev", "prod"}}}
	readiness := accessReadinessStub{readiness: SubsystemAccessReadiness{ApprovalReady: true, LicenseReady: true, ImagesReady: true, Version: "ready-1"}}
	registryReader, err := NewSubsystemAccessRegistryGORMReader(db)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewSubsystemAccessFactsAdapter(db, catalog, readiness, registryReader)
	if err != nil {
		t.Fatal(err)
	}

	facts, version, err := adapter.ReadSubsystemAccessFacts(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !facts.InScope || !facts.Registered || facts.ControlledDeployment {
		t.Fatalf("facts = %+v", facts)
	}
	if facts.ApplicationID != "01APP000000000000000000001" || facts.EnvironmentID != "01ENV000000000000000000001" {
		t.Fatalf("ids = %s/%s", facts.ApplicationID, facts.EnvironmentID)
	}
	// 清单外目标：InScope 为假（但登记事实仍照实读取）。
	outOfScope, _, err := adapter.ReadSubsystemAccessFacts(context.Background(), "tenant-1", "settlement", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if outOfScope.InScope {
		t.Fatalf("staging must be out of scope: %+v", outOfScope)
	}

	// 预览版本稳定：同一事实重复读取必须一致。
	_, again, err := adapter.ReadSubsystemAccessFacts(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil || again.Version != version.Version {
		t.Fatalf("preview version must be stable: %s vs %s (%v)", version.Version, again.Version, err)
	}

	// 受控部署（失败）→ ControlledDeployment + LastOperationFailed；预览版本必须变化。
	if err := db.Exec("INSERT INTO subsystem_deployment_state (tenant_id, application_id, environment_id, application_code, environment_code, status, operation, generation, attempt_count, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"tenant-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "PROVISION_FAILED", "ONBOARD", 3, 2, now, now).Error; err != nil {
		t.Fatal(err)
	}
	deployed, changed, err := adapter.ReadSubsystemAccessFacts(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !deployed.ControlledDeployment || !deployed.LastOperationFailed {
		t.Fatalf("deployment facts = %+v", deployed)
	}
	if changed.Version == version.Version {
		t.Fatal("preview version must change when deployment facts change")
	}
	// 决策随之从 ADOPT 变为 RETRY（事实驱动，页面不自行判断）。
	decision := application.DecideSubsystemAccessAction(deployed)
	if decision.Action != application.SubsystemAccessActionRetry {
		t.Fatalf("decision = %s, want RETRY", decision.Action)
	}
}
