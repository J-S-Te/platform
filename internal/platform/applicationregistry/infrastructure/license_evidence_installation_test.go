package infrastructure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallationApprovalRequiresExplicitBoundaryAndProfileExclusions(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "license-evidence.json")
	p := productionSubsystemProfile{}
	p.Manifest.Compose.DependencyServices = []string{"mysql"}
	a := licenseEvidenceApproval{Version: 1, Project: "production", ExcludedServices: []string{"mysql"}}
	write := func() {
		b, _ := json.Marshal(a)
		if os.WriteFile(path, b, 0600) != nil {
			t.Fatal("write")
		}
	}
	write()
	if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("application inventory accepted as installation")
	}
	a.InstallationBoundary = true
	write()
	if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err != nil {
		t.Fatal(err)
	}
	a.ExcludedServices = []string{"arbitrary-api"}
	write()
	if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("unknown unit exclusion accepted")
	}
	a.ExcludedServices = []string{"platform-api", "platform-worker", "frontend", "file-gateway", "keycloak"}
	write()
	if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err != nil {
		t.Fatalf("reviewed core boundary rejected: %v", err)
	}
	// 依赖声明不得隐藏业务 worker，即使其名称
	// 也出现在平台核心组件列表中。
	p.Manifest.Compose.RuntimeServices = []string{"platform-worker"}
	if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("runtime worker hidden by infrastructure exclusion")
	}
	p.Manifest.Compose.RuntimeServices = nil
	p.Manifest.Compose.ConditionalRuntimeServices = map[string]string{"platform-worker": "enabled"}
	if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("conditional runtime hidden by infrastructure exclusion")
	}
}

func TestReviewedCoreLicenseBoundaryExistsInControlledProductionCompose(t *testing.T) {
	b, err := os.ReadFile("../../../../deploy/production/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	for name := range reviewedPlatformLicenseEvidenceInfrastructure() {
		if !strings.Contains(string(b), "\n  "+name+":\n") {
			t.Fatalf("core boundary %s missing from controlled compose", name)
		}
	}
	for _, name := range []string{"contract-api", "project-sla-notifier", "settlement-worker", "data-analysis-metabase", "customer-presale-worker", "portal-api"} {
		if reviewedPlatformLicenseEvidenceInfrastructure()[name] {
			t.Fatalf("business %s excluded", name)
		}
	}
}

func TestReviewedInitializationAndMonitoringExclusionsRemainExplicitAndNonbusiness(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "license-evidence.json")
	for _, name := range []string{"platform-key-init", "subsystem-provisioner-socket-init", "prometheus", "keycloak-backup-metrics", "arbitrary-init", "bootstrap", "data-analysis-metabase", "prometheus-worker", "custom-monitoring"} {
		t.Run(name, func(t *testing.T) {
			a := licenseEvidenceApproval{Version: 1, Project: "production", InstallationBoundary: true, ExcludedServices: []string{name}}
			b, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			approved := name == "platform-key-init" || name == "subsystem-provisioner-socket-init" || name == "prometheus" || name == "keycloak-backup-metrics"
			_, installationErr := loadInstallationLicenseEvidenceApproval(path, "production", nil)
			_, migrationErr := loadMigrationInstallationEvidenceApproval(path, "production", nil)
			if (installationErr == nil) != approved || (migrationErr == nil) != approved {
				t.Fatalf("unexpected exclusion approval: installation=%v migration=%v", installationErr, migrationErr)
			}
			if !approved {
				return
			}
			for _, conditional := range []bool{false, true} {
				p := productionSubsystemProfile{}
				if conditional {
					p.Manifest.Compose.ConditionalRuntimeServices = map[string]string{name: "enabled"}
				} else {
					p.Manifest.Compose.RuntimeServices = []string{name}
				}
				if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
					t.Fatal("business membership hidden by init exclusion")
				}
				if _, err := loadMigrationInstallationEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
					t.Fatal("migration business membership hidden by infrastructure exclusion")
				}
			}
		})
	}
}
