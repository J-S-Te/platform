package infrastructure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

func TestConditionalRuntimeApprovalsUseSetUnion(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := productionSubsystemProfile{}
	p.Manifest.Application.Code = "customer_portal"
	p.Manifest.Application.Environment = "prod"
	p.Manifest.Compose.RuntimeServices = []string{"portal-api", "portal-report-worker", "portal-invite-compensation-worker"}
	p.Manifest.Compose.ConditionalRuntimeServices = map[string]string{"portal-invite-compensation-worker": "runtime/customer.env"}
	sum := "sha256:" + strings.Repeat("a", 64)
	release := runtimeLicenseRelease{Version: 1, Application: "customer_portal", Environment: "prod"}
	evidence := licenseEvidenceApproval{Version: 1, Project: "test", InstallationBoundary: true}
	for _, service := range p.Manifest.Compose.RuntimeServices {
		release.Components = append(release.Components, application.RuntimeLicenseApproval{ServiceID: service, Protocol: 1, CoverageDigest: sum, ImageDigest: sum})
		evidence.Services = append(evidence.Services, licenseevidence.Service{Application: "customer_portal", Environment: "prod", Name: service, ImageDigest: sum, Version: "v1", Protocol: "1", Replicas: 1})
	}
	write := func(name string, v any) string {
		path := filepath.Join(dir, name)
		raw, err := json.Marshal(v)
		if err != nil || os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("fixture write")
		}
		return path
	}
	if items, err := loadRuntimeLicenseRelease(write("runtime.json", release), p); err != nil || len(items) != 3 {
		t.Fatal("conditional counted twice", err)
	}
	if config, err := loadLicenseEvidenceApproval(write("evidence.json", evidence), "test", p); err != nil || len(config.Services) != 3 {
		t.Fatal("conditional counted twice", err)
	}
	for _, missing := range p.Manifest.Compose.RuntimeServices {
		t.Run("missing-"+missing, func(t *testing.T) {
			partialRelease, partialEvidence := release, evidence
			partialRelease.Components, partialEvidence.Services = nil, nil
			for index, item := range release.Components {
				if item.ServiceID != missing {
					partialRelease.Components = append(partialRelease.Components, item)
					partialEvidence.Services = append(partialEvidence.Services, evidence.Services[index])
				}
			}
			// 完整声明必须始终包含条件消费者；
			// 实际证据只允许省略指定的条件执行单元。
			if _, err := loadRuntimeLicenseRelease(write("runtime.json", partialRelease), p); err == nil {
				t.Fatal("incomplete runtime release accepted")
			}
			path := write("evidence.json", partialEvidence)
			config, appErr := loadLicenseEvidenceApproval(path, "test", p)
			installation, installationErr := loadInstallationLicenseEvidenceApproval(path, "test", []productionSubsystemProfile{p})
			_, conditional := p.Manifest.Compose.ConditionalRuntimeServices[missing]
			if (appErr == nil) != conditional || (installationErr == nil) != conditional {
				t.Fatalf("conditional/required execution boundary changed: app=%v installation=%v", appErr, installationErr)
			}
			if conditional && (len(config.Services) != 2 || len(installation.Services) != 2) {
				t.Fatal("absent optional execution was fabricated in actual evidence")
			}
		})
	}
	for _, invalid := range []string{"duplicate", "unknown"} {
		t.Run(invalid, func(t *testing.T) {
			badRelease, badEvidence := release, evidence
			extra, extraService := release.Components[0], evidence.Services[0]
			if invalid == "unknown" {
				extra.ServiceID, extraService.Name = "portal-unreviewed-worker", "portal-unreviewed-worker"
			}
			badRelease.Components = append(append([]application.RuntimeLicenseApproval(nil), release.Components...), extra)
			badEvidence.Services = append(append([]licenseevidence.Service(nil), evidence.Services...), extraService)
			if _, err := loadRuntimeLicenseRelease(write("runtime.json", badRelease), p); err == nil {
				t.Fatal("invalid declaration accepted")
			}
			path := write("evidence.json", badEvidence)
			if _, err := loadLicenseEvidenceApproval(path, "test", p); err == nil {
				t.Fatal("invalid actual evidence accepted")
			}
			if _, err := loadInstallationLicenseEvidenceApproval(path, "test", []productionSubsystemProfile{p}); err == nil {
				t.Fatal("invalid installation evidence accepted")
			}
		})
	}
}
