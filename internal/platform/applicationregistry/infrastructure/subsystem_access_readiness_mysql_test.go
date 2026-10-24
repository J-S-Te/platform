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

type accessApprovalSourceStub struct {
	components []application.RuntimeLicenseApproval
	err        error
}

func (stub accessApprovalSourceStub) ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]application.RuntimeLicenseApproval, error) {
	return stub.components, stub.err
}

type accessImageReaderStub struct {
	ready    bool
	version  string
	verified bool
}

func (stub accessImageReaderStub) ReadSubsystemAccessImageReadiness(context.Context, string, string, string) (bool, string, bool, error) {
	return stub.ready, stub.version, stub.verified, nil
}

type accessObservationReaderStub struct {
	pending bool
}

func (stub accessObservationReaderStub) ReadSubsystemAccessAuthObservation(context.Context, string, string, string) (bool, string, error) {
	return stub.pending, "observation-1", nil
}

// 真实数据库验证就绪事实：许可证产物 + 该环境部署行（时钟未阻断）才算就绪；未接线的镜像/观察期如实标记未验证。
func TestSubsystemAccessReadinessAdapterOnMySQL(t *testing.T) {
	dsn := os.Getenv("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("PLATFORM_SUBSYSTEM_ACCESS_TEST_DSN is not configured; no database connection attempted")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open disposable MySQL schema: %v", err)
	}
	for _, statement := range []string{
		"DROP TABLE IF EXISTS license_artifact",
		"DROP TABLE IF EXISTS license_deployment",
		`CREATE TABLE license_artifact (digest VARCHAR(64) NOT NULL PRIMARY KEY, raw_jws MEDIUMTEXT NOT NULL, version BIGINT UNSIGNED NOT NULL, created_at DATETIME(3) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE license_deployment (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, instance_id VARCHAR(128) NOT NULL, customer_id VARCHAR(128) NOT NULL, environment VARCHAR(128) NOT NULL, revision BIGINT UNSIGNED NOT NULL DEFAULT 0, current_digest VARCHAR(64) NOT NULL DEFAULT '', pending_digest VARCHAR(64) NOT NULL DEFAULT '', highest_version BIGINT UNSIGNED NOT NULL DEFAULT 0, highest_observed_at BIGINT NOT NULL DEFAULT 0, clock_blocked BOOLEAN NOT NULL DEFAULT FALSE, created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL, UNIQUE KEY uq_license_instance (instance_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("prepare disposable schema: %v", err)
		}
	}
	now := time.Now().UTC()

	// A) 无产物、无部署：许可证不就绪；未接线的镜像/观察期/漂移/历史演示标记为未验证。
	adapter, err := NewSubsystemAccessReadinessAdapter(db, accessApprovalSourceStub{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	readiness, err := adapter.ReadSubsystemAccessReadiness(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if readiness.LicenseReady || readiness.ApprovalReady {
		t.Fatalf("readiness = %+v, want neither license nor approval ready", readiness)
	}
	unverified := map[string]bool{}
	for _, code := range readiness.Unverified {
		unverified[code] = true
	}
	for _, expected := range []string{"images", "auth_observation", "configuration_drift", "legacy_demo"} {
		if !unverified[expected] {
			t.Fatalf("unverified missing %s: %v", expected, readiness.Unverified)
		}
	}

	// B) 产物 + 该环境部署（未阻断）→ 就绪；批准来源非空 → 批准就绪；版本随事实变化。
	if err := db.Exec("INSERT INTO license_artifact (digest, raw_jws, version, created_at) VALUES (?,?,?,?)",
		"digest-a", "jws", 3, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO license_deployment (id, instance_id, customer_id, environment, revision, current_digest, highest_version, highest_observed_at, clock_blocked, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		1, "instance-1", "customer-1", "dev", 5, "digest-a", 3, now.Unix(), false, now, now).Error; err != nil {
		t.Fatal(err)
	}
	approvals := accessApprovalSourceStub{components: []application.RuntimeLicenseApproval{
		{ServiceID: "settlement-api", CoverageDigest: "coverage-1", ImageDigest: "sha256:aaa"}}}
	withSources, err := NewSubsystemAccessReadinessAdapter(db, approvals, accessImageReaderStub{ready: true, version: "images-1", verified: true}, accessObservationReaderStub{})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := withSources.ReadSubsystemAccessReadiness(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !ready.LicenseReady || !ready.ApprovalReady || !ready.ImagesReady || len(ready.Unverified) != 2 {
		t.Fatalf("readiness = %+v (unverified=%v)", ready, ready.Unverified)
	}
	if ready.Version == readiness.Version {
		t.Fatal("readiness version must change when facts change")
	}

	// C) 时钟阻断 → 该环境不算就绪。
	if err := db.Exec("UPDATE license_deployment SET clock_blocked = TRUE WHERE environment = ?", "dev").Error; err != nil {
		t.Fatal(err)
	}
	blocked, err := withSources.ReadSubsystemAccessReadiness(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if blocked.LicenseReady {
		t.Fatalf("clock-blocked deployment must not count as ready: %+v", blocked)
	}

	// D) 观察期未结束 → AuthObservationPending（等待，不得绕过）。
	pending, err := NewSubsystemAccessReadinessAdapter(db, approvals, accessImageReaderStub{ready: true, version: "images-1", verified: true}, accessObservationReaderStub{pending: true})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := pending.ReadSubsystemAccessReadiness(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !observation.AuthObservationPending {
		t.Fatalf("observation = %+v, want pending", observation)
	}
}
