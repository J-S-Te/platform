package coordination

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/domain"
	runtime "github.com/J-S-Te/license-core/runtime"
	"testing"
	"time"
)

func TestApprovedServiceValidation(t *testing.T) {
	valid := ServiceSpec{Application: "contract_management", Environment: "production", ServiceID: "contract-api", OAuthClientID: "client-1", CoverageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ImageDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if !validSpec(valid) {
		t.Fatal("valid spec")
	}
	for _, field := range []string{"app", "environment", "service", "client", "coverage", "image"} {
		bad := valid
		switch field {
		case "app":
			bad.Application = "platform"
		case "environment":
			bad.Environment = "../prod"
		case "service":
			bad.ServiceID = ""
		case "client":
			bad.OAuthClientID = " "
		case "coverage":
			bad.CoverageDigest = "bad"
		case "image":
			bad.ImageDigest = "latest"
		}
		if validSpec(bad) {
			t.Fatal(field)
		}
	}
}
func TestFreshnessAndContentDigest(t *testing.T) {
	now := time.Unix(1900000000, 0)
	if !fresh(now.Add(-60*time.Second), now) || fresh(now.Add(time.Second), now) || fresh(now.Add(-61*time.Second), now) {
		t.Fatal("freshness")
	}
	a := Application{State: runtime.Pending, MigrationEligible: true}
	d := &domain.Deployment{CurrentDigest: "a", HighestVersion: 1}
	before := contentDigest(a, d)
	d.Revision++
	if contentDigest(a, d) != before {
		t.Fatal("unrelated global revision churn")
	}
	a.State = runtime.Applying
	a.MigrationEligible = false
	if contentDigest(a, d) == before {
		t.Fatal("lifecycle digest unchanged")
	}
}
