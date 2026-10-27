package infrastructure

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gorm.io/gorm"
	"strings"
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

func TestBuiltinAccessSnapshotDoesNotRequireLegacyApproval(t *testing.T) {
	images := []application.SubsystemImageReadiness{{ServiceID: "contract-api", ImageDigest: "example/contract@sha256:" + strings.Repeat("a", 64), Present: true}, {ServiceID: "contract-worker", ImageDigest: "example/contract@sha256:" + strings.Repeat("a", 64), Present: true}}
	tracker := &SubsystemAccessConfigurationTracker{database: &gorm.DB{}, catalog: accessCatalogWithTarget(), approvals: accessApprovalSourceStub{err: errors.New("legacy source must not be read")}}
	tracker.SetBuiltinRuntimeSource(BuiltinRuntimeComponentCatalog{}, imageReaderStub{images: images, reported: true})
	op := onboardingOperation()
	op.ApplicationCode = "contract_management"
	// 目录目标使用已有 fixture 的应用编码。
	tracker.catalog = accessCatalogStubForBuiltin{}
	first, err := tracker.Snapshot(context.Background(), op)
	if err != nil || first == "" {
		t.Fatalf("builtin snapshot: %s %v", first, err)
	}
	images[0].ImageDigest = "example/contract@sha256:" + strings.Repeat("b", 64)
	changed, err := tracker.Snapshot(context.Background(), op)
	if err != nil || changed == first {
		t.Fatalf("image change not detected: %v", err)
	}
	for _, source := range []imageReaderStub{{reported: false}, {reported: true}, {reported: true, images: []application.SubsystemImageReadiness{{ServiceID: "contract-api", Present: false}}}} {
		tracker.SetBuiltinRuntimeSource(BuiltinRuntimeComponentCatalog{}, source)
		if _, err := tracker.Snapshot(context.Background(), op); err == nil {
			t.Fatal("unverified/empty/missing images accepted")
		}
	}
}

type accessCatalogStubForBuiltin struct{}

func (accessCatalogStubForBuiltin) AvailableCapabilities(context.Context) (application.SubsystemProvisioningCapabilities, error) {
	return application.SubsystemProvisioningCapabilities{Enabled: true, Targets: []application.SubsystemProvisioningTarget{{ApplicationCode: "contract_management", Environment: "dev", ManifestChecksum: "manifest-aaa"}}}, nil
}
