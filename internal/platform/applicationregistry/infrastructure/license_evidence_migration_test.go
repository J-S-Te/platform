package infrastructure

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

type migrationEvidenceSocketExecutor struct {
	recordingSubsystemProvisioner
	called bool
}

func (e *migrationEvidenceSocketExecutor) CollectMigrationInstallationEvidence(context.Context) (licenseevidence.Report, error) {
	e.called = true
	return licenseevidence.Report{Scope: "MIGRATION_INSTALLATION", BoundarySupported: true, Complete: true}, nil
}

func TestMigrationEvidenceSocketRejectsCallerSelectors(t *testing.T) {
	for _, inject := range []string{"", "code", "environment", "checksum", "tenant"} {
		t.Run(inject, func(t *testing.T) {
			left, right := net.Pipe()
			executor := &migrationEvidenceSocketExecutor{}
			done := make(chan struct{})
			go func() { handleSubsystemProvisioningConnection(context.Background(), left, executor); close(done) }()
			request := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_MIGRATION_INSTALLATION_EVIDENCE"}
			switch inject {
			case "code":
				request.Code = "project_management"
			case "environment":
				request.Environment = "prod"
			case "checksum":
				request.ManifestChecksum = "override"
			case "tenant":
				request.TenantID = "override"
			}
			if err := json.NewEncoder(right).Encode(request); err != nil {
				t.Fatal(err)
			}
			var reply subsystemProvisioningReply
			if err := json.NewDecoder(right).Decode(&reply); err != nil {
				t.Fatal(err)
			}
			right.Close()
			<-done
			if inject != "" {
				if reply.Success || executor.called {
					t.Fatal("caller overrode fixed migration boundary")
				}
			} else if !reply.Success || !executor.called || reply.LicenseEvidence == nil || reply.LicenseEvidence.Scope != "MIGRATION_INSTALLATION" {
				t.Fatal(reply)
			}
		})
	}
}

func TestMigrationApprovalLegacySubsetAndExclusionBoundary(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, migrationLicenseEvidenceApprovalFile)
	p := productionSubsystemProfile{}
	p.Manifest.Application.Code = "project_management"
	p.Manifest.Application.Environment = "prod"
	p.Manifest.Compose.RuntimeServices = []string{"project-api", "project-sla-notifier"}
	a := licenseEvidenceApproval{Version: 1, Project: "production", InstallationBoundary: true, Services: []licenseevidence.Service{{Application: "project_management", Environment: "prod", Name: "project-api", ImageDigest: "sha256:" + strings.Repeat("a", 64), Replicas: 1}}}
	write := func() {
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := loadMigrationInstallationEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstallationLicenseEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("strict installation accepted legacy subset")
	}
	a.Services[0].Name = "unknown-api"
	write()
	if _, err := loadMigrationInstallationEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("unknown old service accepted")
	}
	a.Services[0].Name = "project-api"
	a.ExcludedServices = []string{"project-sla-notifier"}
	write()
	if _, err := loadMigrationInstallationEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("new runtime hidden by exclusion")
	}
	a.ExcludedServices = nil
	a.Services[0].Environment = "other"
	write()
	if _, err := loadMigrationInstallationEvidenceApproval(path, "production", []productionSubsystemProfile{p}); err == nil {
		t.Fatal("unapproved environment accepted")
	}
}
