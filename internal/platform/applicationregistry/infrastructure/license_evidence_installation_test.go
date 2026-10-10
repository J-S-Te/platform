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
	// A dependency declaration cannot hide a business worker, even when its name
	// also appears in the platform core list.
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

func TestReviewedInitializationExclusionsRemainExplicitAndNonbusiness(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "license-evidence.json")
	for _, name := range []string{"platform-key-init", "subsystem-provisioner-socket-init", "arbitrary-init", "bootstrap", "data-analysis-metabase"} {
		t.Run(name, func(t *testing.T) {
			a := licenseEvidenceApproval{Version: 1, Project: "production", InstallationBoundary: true, ExcludedServices: []string{name}}
			b, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = loadInstallationLicenseEvidenceApproval(path, "production", nil)
			approved := name == "platform-key-init" || name == "subsystem-provisioner-socket-init"
			if (err == nil) != approved {
				t.Fatalf("unexpected exclusion approval: %v", err)
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
			}
		})
	}
}
