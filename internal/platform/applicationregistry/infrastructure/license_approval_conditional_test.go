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
	p.Manifest.Compose.RuntimeServices = []string{"portal-api", "portal-worker"}
	p.Manifest.Compose.ConditionalRuntimeServices = map[string]string{"portal-worker": "runtime/customer.env"}
	sum := "sha256:" + strings.Repeat("a", 64)
	release := runtimeLicenseRelease{Version: 1, Application: "customer_portal", Environment: "prod", Components: []application.RuntimeLicenseApproval{{ServiceID: "portal-api", Protocol: 1, CoverageDigest: sum, ImageDigest: sum}, {ServiceID: "portal-worker", Protocol: 1, CoverageDigest: sum, ImageDigest: sum}}}
	evidence := licenseEvidenceApproval{Version: 1, Project: "test", Services: []licenseevidence.Service{{Application: "customer_portal", Environment: "prod", Name: "portal-api", ImageDigest: sum, Version: "v1", Protocol: "1", Replicas: 1}, {Application: "customer_portal", Environment: "prod", Name: "portal-worker", ImageDigest: sum, Version: "v1", Protocol: "1", Replicas: 1}}}
	write := func(name string, v any) string {
		path := filepath.Join(dir, name)
		raw, err := json.Marshal(v)
		if err != nil || os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("fixture write")
		}
		return path
	}
	if _, err := loadRuntimeLicenseRelease(write("runtime.json", release), p); err != nil {
		t.Fatal("conditional counted twice", err)
	}
	if _, err := loadLicenseEvidenceApproval(write("evidence.json", evidence), "test", p); err != nil {
		t.Fatal("conditional counted twice", err)
	}
	release.Components = release.Components[:1]
	evidence.Services = evidence.Services[:1]
	if _, err := loadRuntimeLicenseRelease(write("runtime.json", release), p); err == nil {
		t.Fatal("missing conditional accepted")
	}
	if _, err := loadLicenseEvidenceApproval(write("evidence.json", evidence), "test", p); err == nil {
		t.Fatal("missing conditional accepted")
	}
	release.Components = append(release.Components, release.Components[0])
	evidence.Services = append(evidence.Services, evidence.Services[0])
	if _, err := loadRuntimeLicenseRelease(write("runtime.json", release), p); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := loadLicenseEvidenceApproval(write("evidence.json", evidence), "test", p); err == nil {
		t.Fatal("duplicate accepted")
	}
}
