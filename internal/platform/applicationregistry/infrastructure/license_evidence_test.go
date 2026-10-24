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

func TestLicenseEvidenceApprovalRequiresFullApprovedRelease(t *testing.T) {
	profile := productionSubsystemProfile{}
	profile.Manifest.Application.Code = "project_management"
	profile.Manifest.Application.Environment = "prod"
	profile.Manifest.Compose.RuntimeServices = []string{"project-api", "project-worker"}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "license-evidence.json")
	if _, err := loadLicenseEvidenceApproval(path, "production", profile); err == nil {
		t.Fatal("missing approval accepted")
	}
	s := licenseevidence.Service{Application: "project_management", Environment: "prod", Name: "project-api", ImageDigest: "sha256:" + strings.Repeat("a", 64), Version: "v1", Protocol: "1", Replicas: 1}
	approval := licenseEvidenceApproval{Version: 1, Project: "production", Services: []licenseevidence.Service{s}}
	write := func() {
		b, _ := json.Marshal(approval)
		if os.WriteFile(path, b, 0600) != nil {
			t.Fatal("write fixture")
		}
	}
	write()
	if _, err := loadLicenseEvidenceApproval(path, "production", profile); err == nil {
		t.Fatal("worker omission accepted")
	}
	s.Name = "project-worker"
	approval.Services = append(approval.Services, s)
	write()
	if _, err := loadLicenseEvidenceApproval(path, "production", profile); err != nil {
		t.Fatal(err)
	}
	approval.Services[0].Protocol = ""
	write()
	if _, err := loadLicenseEvidenceApproval(path, "production", profile); err == nil {
		t.Fatal("missing protocol accepted")
	}
	approval.Services[0].Protocol = "1"
	write()
	if os.Chmod(path, 0666) != nil {
		t.Fatal("chmod")
	}
	if _, err := loadLicenseEvidenceApproval(path, "production", profile); err == nil {
		t.Fatal("writable approval accepted")
	}
}

func TestLicenseEvidenceDefaultExecutorsRejectWithoutApproval(t *testing.T) {
	if _, err := (&LocalDockerSubsystemProvisioner{}).CollectLicenseEvidence(context.Background(), "project_management", "prod"); err == nil || !strings.Contains(err.Error(), "LICENSE_EVIDENCE_UNSUPPORTED") {
		t.Fatal(err)
	}
	if _, err := (&ProductionComposeSubsystemProvisioner{enabled: true}).CollectLicenseEvidence(context.Background(), "project_management", "prod"); err == nil {
		t.Fatal("unapproved target accepted")
	}
}

type evidenceSocketExecutor struct {
	recordingSubsystemProvisioner
	called bool
}

func (e *evidenceSocketExecutor) CollectLicenseEvidence(ctx context.Context, app, env string) (licenseevidence.Report, error) {
	e.called = true
	return licenseevidence.Report{Project: "production", Complete: false, Problems: []string{"project-api:collection_failed"}}, nil
}

func TestLicenseEvidenceFixedSocketAction(t *testing.T) {
	for _, payload := range []bool{false, true} {
		left, right := net.Pipe()
		executor := &evidenceSocketExecutor{}
		done := make(chan struct{})
		go func() { handleSubsystemProvisioningConnection(context.Background(), left, executor); close(done) }()
		req := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_EVIDENCE", Code: "project_management", Environment: "prod"}
		if payload {
			req.TenantID = "override"
		}
		if err := json.NewEncoder(right).Encode(req); err != nil {
			t.Fatal(err)
		}
		var reply subsystemProvisioningReply
		if err := json.NewDecoder(right).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		right.Close()
		<-done
		if payload {
			if reply.Success || executor.called {
				t.Fatal("unsupported payload executed")
			}
		} else if !reply.Success || !executor.called || reply.LicenseEvidence == nil || reply.LicenseEvidence.Complete {
			t.Fatal("incomplete facts lost", reply)
		}
	}
}
