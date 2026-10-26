package infrastructure

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"testing"
)

func TestSubsystemAccessConfigurationDetectsUpdates(t *testing.T) {
	first := []application.RuntimeLicenseApproval{{ServiceID: "api", ImageDigest: "sha256:old"}}
	second := []application.RuntimeLicenseApproval{{ServiceID: "api", ImageDigest: "sha256:new"}}
	baseline := accessConfigurationFingerprint("manifest-1", first)
	for _, tt := range []struct {
		name, manifest, applied, baseline string
		images                            []application.RuntimeLicenseApproval
		drift                             bool
	}{
		{"unchanged", "manifest-1", "manifest-1", baseline, first, false},
		{"new image", "manifest-1", "manifest-1", baseline, second, true},
		{"new manifest", "manifest-2", "manifest-1", baseline, first, true},
		{"unknown baseline", "manifest-1", "manifest-1", "", first, true},
		{"not applied", "manifest-1", "", baseline, first, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			drift, summary := accessConfigurationDrift(accessConfigurationFingerprint(tt.manifest, tt.images), tt.baseline, tt.manifest, tt.applied)
			if drift != tt.drift || (drift && len(summary) == 0) {
				t.Fatalf("drift=%v summary=%v", drift, summary)
			}
		})
	}
	if got := accessCandidateFromEvidence(accessCandidatePrefix + baseline + ";provision:manifest-1"); got != baseline {
		t.Fatal(got)
	}
	if accessCandidateFromEvidence("provision:manifest-1") != "" || accessCandidateFromEvidence(accessCandidatePrefix+"invalid") != "" {
		t.Fatal("legacy or malformed evidence accepted")
	}
}
