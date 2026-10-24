package application

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
	licenseruntime "github.com/J-S-Te/license-core/runtime"
)

func confirmationFixture() (coordination.Status, map[string]RuntimeLicenseCredential) {
	now := time.Now().UTC()
	c := RuntimeLicenseCredential{Approval: RuntimeLicenseApproval{ServiceID: "presale-worker", Protocol: 1, CoverageDigest: "sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64)}, ClientID: "runtime-client"}
	status := coordination.Status{Application: coordination.Application{Application: "customer_and_opportunity", Revision: 4, State: licenseruntime.Pending}, SnapshotCurrent: true, Members: []coordination.Member{{ServiceID: c.Approval.ServiceID, Application: "customer_and_opportunity", Environment: "dev", OAuthClientID: c.ClientID, CoverageDigest: c.Approval.CoverageDigest, ImageDigest: c.Approval.ImageDigest, ReadyAt: &now, IssuedRevision: 4, AckRevision: 4, AckAt: &now}}}
	return status, map[string]RuntimeLicenseCredential{c.Approval.ServiceID: c}
}
func TestRuntimeConfirmationRequiresCompleteNaturalCurrentAcknowledgements(t *testing.T) {
	for _, mode := range []string{"pending-valid", "expired-current-valid", "missing-ack", "old-ack", "stale-ready", "future-ack", "other-app", "other-env", "other-client", "other-image", "other-coverage", "duplicate", "retired", "snapshot-stale", "empty"} {
		t.Run(mode, func(t *testing.T) {
			s, c := confirmationFixture()
			now := time.Now().UTC()
			switch mode {
			case "expired-current-valid":
				s.State = licenseruntime.Enforced
			case "missing-ack":
				s.Members[0].AckAt = nil
			case "old-ack":
				s.Members[0].AckRevision = 3
			case "stale-ready":
				old := now.Add(-2 * time.Minute)
				s.Members[0].ReadyAt = &old
			case "future-ack":
				future := now.Add(time.Minute)
				s.Members[0].AckAt = &future
			case "other-app":
				s.Members[0].Application = "contract_management"
			case "other-env":
				s.Members[0].Environment = "prod"
			case "other-client":
				s.Members[0].OAuthClientID = "foreign"
			case "other-image":
				s.Members[0].ImageDigest = "sha256:" + strings.Repeat("f", 64)
			case "other-coverage":
				s.Members[0].CoverageDigest = "sha256:" + strings.Repeat("f", 64)
			case "duplicate":
				s.Members = append(s.Members, s.Members[0])
			case "retired":
				s.Members[0].RetiredAt = &now
			case "snapshot-stale":
				s.SnapshotCurrent = false
			case "empty":
				s.Members = nil
			}
			got := runtimeConfirmationCurrent(s, "customer_and_opportunity", "dev", c, now)
			want := mode == "pending-valid" || mode == "expired-current-valid"
			if got != want {
				t.Fatalf("confirmation=%v want=%v", got, want)
			}
		})
	}
}
func TestWaitRuntimeConfirmationReadsWithoutChangingEnforcement(t *testing.T) {
	status, expected := confirmationFixture()
	registrar := &runtimeLifecycleRegistrarStub{status: status}
	s := &RuntimeLicenseEnrollmentService{registrar: registrar}
	list := []RuntimeLicenseCredential{}
	for _, c := range expected {
		list = append(list, c)
	}
	if err := s.WaitForRuntimeConfirmation(context.Background(), "customer_and_opportunity", "dev", list); err != nil {
		t.Fatal(err)
	}
	if len(registrar.requests) != 0 || len(registrar.specs) != 0 || registrar.status.State != licenseruntime.Pending {
		t.Fatal("readiness changed enforcement or membership")
	}
	registrar.status.Members[0].AckRevision = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.WaitForRuntimeConfirmation(ctx, "customer_and_opportunity", "dev", list); err == nil {
		t.Fatal("missing confirmation accepted")
	}
}
