package infrastructure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	licenseevidence "github.com/J-S-Te/Basic-Platform/internal/platform/license/evidence"
)

func conditionalEvidenceFixture(t *testing.T) (productionSubsystemProfile, string, licenseEvidenceApproval) {
	t.Helper()
	_, _, profiles := offlineInteropProfiles(t)
	var profile productionSubsystemProfile
	for _, item := range profiles {
		if item.Manifest.Application.Code == "customer_portal" {
			profile = item
		}
	}
	if len(profile.Manifest.Compose.RuntimeServices) != 3 || len(profile.Manifest.Compose.ConditionalRuntimeServices) != 1 {
		t.Fatal("actual Portal required/conditional boundary changed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	approval := licenseEvidenceApproval{Version: 1, Project: "conditional-interop", InstallationBoundary: true, ExcludedServices: []string{"platform-api"}, Services: []licenseevidence.Service{}}
	for _, name := range profile.Manifest.Compose.RuntimeServices {
		if _, conditional := profile.Manifest.Compose.ConditionalRuntimeServices[name]; !conditional {
			approval.Services = append(approval.Services, licenseevidence.Service{Application: "customer_portal", Environment: "prod", Name: name, ImageDigest: "sha256:" + strings.Repeat("a", 64), Version: "synthetic-test-v1", Protocol: "1", Replicas: 1})
		}
	}
	return profile, filepath.Join(dir, "license-evidence.json"), approval
}

func writeConditionalEvidence(t *testing.T, path string, approval licenseEvidenceApproval) {
	t.Helper()
	raw, err := json.Marshal(approval)
	if err != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("write isolated conditional evidence", err)
	}
}

func TestInstallationEvidenceAllowsOnlyFixedConditionalAbsence(t *testing.T) {
	for _, mode := range []string{"absent-conditional", "present-conditional", "missing-api", "missing-report-worker", "unknown-worker", "duplicate", "invalid-metadata", "wrong-environment"} {
		t.Run(mode, func(t *testing.T) {
			profile, path, approval := conditionalEvidenceFixture(t)
			switch mode {
			case "present-conditional":
				optional := approval.Services[0]
				optional.Name = "portal-invite-compensation-worker"
				approval.Services = append(approval.Services, optional)
			case "missing-api", "missing-report-worker":
				name := "portal-api"
				if mode == "missing-report-worker" {
					name = "portal-report-worker"
				}
				approval.Services = slices.DeleteFunc(approval.Services, func(item licenseevidence.Service) bool { return item.Name == name })
			case "unknown-worker":
				extra := approval.Services[0]
				extra.Name = "portal-unreviewed-worker"
				approval.Services = append(approval.Services, extra)
			case "duplicate":
				approval.Services = append(approval.Services, approval.Services[0])
			case "invalid-metadata":
				approval.Services[0].Protocol = ""
			case "wrong-environment":
				approval.Services[0].Environment = "dev"
			}
			writeConditionalEvidence(t, path, approval)
			_, appErr := loadLicenseEvidenceApproval(path, approval.Project, profile)
			_, installErr := loadInstallationLicenseEvidenceApproval(path, approval.Project, []productionSubsystemProfile{profile})
			valid := mode == "absent-conditional" || mode == "present-conditional"
			if (appErr == nil) != valid || (installErr == nil) != valid {
				t.Fatalf("conditional/required boundary: application=%v installation=%v", appErr, installErr)
			}
		})
	}
}

type conditionalInstallationRunner struct {
	services []licenseevidence.Service
	project  string
	extra    bool
}

func (r conditionalInstallationRunner) RunOutput(_ context.Context, _ int, command string, args ...string) ([]byte, error) {
	if command != "docker" || len(args) < 2 {
		return nil, fmt.Errorf("unexpected synthetic read-only command")
	}
	names := []string{}
	for _, service := range r.services {
		names = append(names, service.Name)
	}
	if r.extra {
		names = append(names, "portal-invite-compensation-worker")
	}
	if args[0] == "ps" {
		ids := []string{}
		for i := range names {
			ids = append(ids, fmt.Sprintf("%064x", i+1))
		}
		return []byte(strings.Join(ids, "\n")), nil
	}
	if args[0] == "container" && args[1] == "inspect" {
		id := args[len(args)-1]
		for i, name := range names {
			if id == fmt.Sprintf("%064x", i+1) {
				return json.Marshal(map[string]any{"id": id, "image": "sha256:" + strings.Repeat("a", 64), "project": r.project, "service": name, "oneoff": "False", "running": true, "started_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)})
			}
		}
	}
	if args[0] == "image" && args[1] == "inspect" {
		return json.Marshal(map[string]string{"id": "sha256:" + strings.Repeat("a", 64), "version": "synthetic-test-v1", "protocol": "1"})
	}
	return nil, fmt.Errorf("unexpected synthetic Docker inspection")
}

func TestOmittedConditionalEvidenceCannotHideAnActualProjectContainer(t *testing.T) {
	profile, path, approval := conditionalEvidenceFixture(t)
	writeConditionalEvidence(t, path, approval)
	config, err := loadInstallationLicenseEvidenceApproval(path, approval.Project, []productionSubsystemProfile{profile})
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []bool{false, true} {
		collector, err := licenseevidence.NewInstallationCollector(conditionalInstallationRunner{services: config.Services, project: config.Project, extra: extra}, config)
		if err != nil {
			t.Fatal(err)
		}
		report := collector.Collect(context.Background())
		if report.Complete == extra {
			t.Fatal("actual extra conditional container escaped complete project collection", report)
		}
		if extra && !slices.Contains(report.Problems, "unknown_installation_execution_unit") {
			t.Fatal("actual unapproved runtime was not classified as an error", report)
		}
	}
}

func TestConditionalEvidenceDoesNotChooseRuntimeStartup(t *testing.T) {
	profile, path, approval := conditionalEvidenceFixture(t)
	writeConditionalEvidence(t, path, approval)
	target := &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: filepath.Dir(path), Profile: profile}}
	target.enabledServices = map[string]bool{"portal-api": true, "portal-report-worker": true, "portal-invite-compensation-worker": true, "customer-api": true}
	if slices.Contains(target.selectedRuntimeLicenseConsumers(), "portal-invite-compensation-worker") {
		t.Fatal("a complete declaration inferred a configured optional runtime")
	}
	if _, err := loadLicenseEvidenceApproval(path, approval.Project, profile); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(target.selectedRuntimeLicenseConsumers(), "portal-invite-compensation-worker") {
		t.Fatal("evidence parsing changed actual runtime selection")
	}
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	dependency := filepath.Join(filepath.Dir(path), "runtime/customer.env")
	if err := os.WriteFile(dependency, []byte("OIDC_CLIENT_ID=synthetic-client\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(target.selectedRuntimeLicenseConsumers(), "portal-invite-compensation-worker") {
		t.Fatal("an incomplete dependency file selected the conditional consumer")
	}
	keys := []string{}
	for _, file := range profile.Manifest.Runtime.Files {
		if file.Path == "runtime/customer.env" {
			keys = append(keys, file.RequiredExistingKeys...)
		}
	}
	if len(keys) == 0 {
		t.Fatal("actual conditional dependency must have reviewed required keys")
	}
	var complete strings.Builder
	for _, key := range keys {
		complete.WriteString(key + "=synthetic-test-value\n")
	}
	if err := os.WriteFile(dependency, []byte(complete.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(target.selectedRuntimeLicenseConsumers(), "portal-invite-compensation-worker") {
		t.Fatal("enabled dependency with complete required keys did not select the consumer")
	}
	target.enabledServices["customer-api"] = false
	if slices.Contains(target.selectedRuntimeLicenseConsumers(), "portal-invite-compensation-worker") {
		t.Fatal("file presence selected a conditional consumer whose dependency is disabled")
	}
}

func TestConditionalAbsenceDoesNotRelaxCompleteRuntimeReleaseDeclaration(t *testing.T) {
	profile, evidencePath, evidence := conditionalEvidenceFixture(t)
	path := filepath.Join(filepath.Dir(evidencePath), "runtime-license-customer_portal-prod.json")
	release := runtimeLicenseRelease{Version: 1, Application: "customer_portal", Environment: "prod"}
	for _, service := range evidence.Services {
		release.Components = append(release.Components, application.RuntimeLicenseApproval{ServiceID: service.Name, Protocol: 1, ImageDigest: service.ImageDigest, CoverageDigest: "sha256:" + strings.Repeat("b", 64)})
	}
	for _, complete := range []bool{false, true} {
		if complete {
			optional := release.Components[0]
			optional.ServiceID = "portal-invite-compensation-worker"
			release.Components = append(release.Components, optional)
		}
		raw, err := json.Marshal(release)
		if err != nil || os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("write isolated release declaration", err)
		}
		_, err = loadRuntimeLicenseReleaseDocument(path, profile)
		if (err == nil) != complete {
			t.Fatal("only actual evidence may omit a fixed conditional runtime", err)
		}
	}
}
