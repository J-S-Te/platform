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

// Opt-in interop checks use the real CLI artifact and immutable Docker image
// mapping, not a hand-built success fixture. It never starts or enrolls services.
func TestRuntimeApprovalCandidateInteroperability(t *testing.T) {
	dir := os.Getenv("LICENSE_APPROVAL_CANDIDATE_DIR")
	if dir == "" {
		t.Skip("requires genuine runtime-approval candidate directory")
	}
	root, err := filepath.Abs("../../../../deploy/production")
	if err != nil {
		t.Fatal(err)
	}
	profiles, _, err := loadProductionSubsystemProfiles(root, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "license-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document licenseEvidenceApproval
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	config, err := loadInstallationLicenseEvidenceApproval(filepath.Join(dir, "license-evidence.json"), document.Project, profiles)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, profile := range profiles {
		items, err := loadRuntimeLicenseRelease(filepath.Join(dir, "runtime-license-"+profile.Manifest.Application.Code+"-"+profile.Manifest.Application.Environment+".json"), profile)
		if err != nil {
			t.Fatal(err)
		}
		count += len(items)
	}
	if len(profiles) != 6 || count != 19 || len(config.Services) != 19 || len(config.Infrastructure) != 1 || config.Infrastructure[0].Name != "data-analysis-metabase" {
		t.Fatal("real tool/platform boundary mismatch")
	}
}

func TestBIApprovalSeparatesDeploymentFromConsumers(t *testing.T) {
	target, input := runtimeLicenseFixture(t)
	profile := &target.config.Profile
	profile.Manifest.Application.Code = "data_analysis"
	profile.Manifest.Compose.RuntimeServices = []string{"data-analysis-metabase", "data-analysis-api", "data-analysis-aggregation-worker", "data-analysis-alert-worker"}
	consumers, err := runtimeLicenseConsumerServices(*profile)
	if err != nil || len(consumers) != 3 || len(target.selectedRuntimeServices()) != 4 || len(target.selectedRuntimeLicenseConsumers()) != 3 {
		t.Fatal("deployment/consumer boundary lost", err)
	}
	input.ApplicationCode = "data_analysis"
	input.RuntimeLicenseCredentials = nil
	release := runtimeLicenseRelease{Version: 1, Application: "data_analysis", Environment: "prod"}
	for _, name := range consumers {
		release.Components = append(release.Components, application.RuntimeLicenseApproval{ServiceID: name, Protocol: 1, ImageDigest: "sha256:" + strings.Repeat("b", 64), CoverageDigest: "sha256:" + strings.Repeat("a", 64)})
	}
	write := func() string {
		raw, e := json.Marshal(release)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(target.config.DeployRoot, "runtime-license-data_analysis-prod.json")
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	if _, err := loadRuntimeLicenseRelease(write(), *profile); err != nil {
		t.Fatal(err)
	}
	digest, err := target.runtimeLicenseReleaseDigest(release)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range release.Components {
		input.RuntimeLicenseCredentials = append(input.RuntimeLicenseCredentials, application.RuntimeLicenseCredential{Approval: item, ClientID: item.ServiceID, ClientSecret: "isolated-test", ReleaseDigest: digest})
	}
	if err = target.writeRuntimeLicenseCredentials(input); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(target.config.DeployRoot, "runtime", "license-data-analysis-metabase.env")); !os.IsNotExist(err) {
		t.Fatal("third-party consumer credential created")
	}
	release.Components = append(release.Components, application.RuntimeLicenseApproval{ServiceID: "data-analysis-metabase", Protocol: 1, ImageDigest: "sha256:" + strings.Repeat("b", 64), CoverageDigest: "sha256:" + strings.Repeat("a", 64)})
	if _, err = loadRuntimeLicenseRelease(write(), *profile); err == nil {
		t.Fatal("fake Metabase ACK member accepted")
	}
	profile.Manifest.Compose.ConditionalRuntimeServices = map[string]string{"data-analysis-metabase": "runtime/optional.env"}
	if _, err = runtimeLicenseConsumerServices(*profile); err == nil {
		t.Fatal("conditional infrastructure boundary accepted")
	}
}

func TestBIEvidenceStillRequiresMetabaseInventory(t *testing.T) {
	profile := productionSubsystemProfile{}
	profile.Manifest.Application.Code, profile.Manifest.Application.Environment = "data_analysis", "prod"
	profile.Manifest.Compose.RuntimeServices = []string{"data-analysis-metabase", "data-analysis-api"}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "license-evidence.json")
	approval := licenseEvidenceApproval{Version: 1, Project: "isolated", InstallationBoundary: true,
		Services:       []licenseevidence.Service{{Application: "data_analysis", Environment: "prod", Name: "data-analysis-api", ImageDigest: "sha256:" + strings.Repeat("a", 64), Version: "v1", Protocol: "1", Replicas: 1}},
		Infrastructure: []licenseevidence.Infrastructure{{Application: "data_analysis", Environment: "prod", Name: "data-analysis-metabase", ImageDigest: "sha256:" + strings.Repeat("b", 64), Replicas: 1, GatewayService: "data-analysis-api"}},
	}
	write := func() {
		raw, e := json.Marshal(approval)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	if _, err = loadLicenseEvidenceApproval(path, "isolated", profile); err != nil {
		t.Fatal(err)
	}
	if _, err = loadInstallationLicenseEvidenceApproval(path, "isolated", []productionSubsystemProfile{profile}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"missing", "excluded", "business-disguise", "foreign-env", "duplicate", "missing-protocol"} {
		t.Run(bad, func(t *testing.T) {
			saved := approval
			approval.Infrastructure = append([]licenseevidence.Infrastructure(nil), saved.Infrastructure...)
			approval.Services = append([]licenseevidence.Service(nil), saved.Services...)
			switch bad {
			case "missing":
				approval.Infrastructure = nil
			case "excluded":
				approval.ExcludedServices = []string{"data-analysis-metabase"}
			case "business-disguise":
				approval.Infrastructure[0].Name = "data-analysis-api"
			case "foreign-env":
				approval.Infrastructure[0].Environment = "dev"
			case "duplicate":
				approval.Infrastructure = append(approval.Infrastructure, approval.Infrastructure[0])
			case "missing-protocol":
				approval.Services[0].Protocol = ""
			}
			write()
			if _, err := loadInstallationLicenseEvidenceApproval(path, "isolated", []productionSubsystemProfile{profile}); err == nil {
				t.Fatal("untrusted infrastructure boundary accepted")
			}
			approval = saved
		})
	}
}
