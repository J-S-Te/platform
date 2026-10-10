package infrastructure

import (
	"context"
	"encoding/json"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"net"
	"strings"
	"testing"
)

type runtimeApprovalSocketExecutor struct {
	recordingSubsystemProvisioner
	called bool
}

func (e *runtimeApprovalSocketExecutor) ApprovedRuntimeLicenseComponents(_ context.Context, app, env string) ([]application.RuntimeLicenseApproval, error) {
	e.called = true
	if app != "contract_management" || env != "prod" {
		return nil, application.ErrValidation
	}
	return []application.RuntimeLicenseApproval{{ServiceID: "contract-api", Protocol: 1, CoverageDigest: "sha256:" + strings.Repeat("a", 64), ImageDigest: "sha256:" + strings.Repeat("b", 64)}}, nil
}
func (e *runtimeApprovalSocketExecutor) ApprovedRuntimeLicenseLifecycle(ctx context.Context, app, env string) (application.RuntimeLicenseReleaseApproval, error) {
	items, err := e.ApprovedRuntimeLicenseComponents(ctx, app, env)
	return application.RuntimeLicenseReleaseApproval{Components: items, RequiredServiceIDs: []string{"contract-api"}, RetireServiceIDs: []string{"contract-old-worker"}}, err
}
func TestRuntimeLicenseLifecycleSocketRejectsPayloadInjection(t *testing.T) {
	for _, inject := range []bool{false, true} {
		left, right := net.Pipe()
		executor := &runtimeApprovalSocketExecutor{}
		done := make(chan struct{})
		go func() { handleSubsystemProvisioningConnection(context.Background(), left, executor); close(done) }()
		request := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_LIFECYCLE_APPROVAL", Code: "contract_management", Environment: "prod"}
		if inject {
			request.Input = &application.SubsystemProvisioningInput{ApplicationCode: "override"}
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
		if inject {
			if reply.Success || executor.called {
				t.Fatal("accepted caller lifecycle payload")
			}
		} else if !reply.Success || reply.RuntimeLicenseLifecycle == nil || len(reply.RuntimeLicenseLifecycle.RetireServiceIDs) != 1 {
			t.Fatal("reviewed retirement lost")
		}
	}
}
func TestRuntimeLicenseApprovalSocketRejectsPayloadInjection(t *testing.T) {
	for _, inject := range []bool{false, true} {
		left, right := net.Pipe()
		executor := &runtimeApprovalSocketExecutor{}
		done := make(chan struct{})
		go func() { handleSubsystemProvisioningConnection(context.Background(), left, executor); close(done) }()
		request := subsystemProvisioningRequest{Version: subsystemProvisioningProtocolVersion, Action: "LICENSE_RUNTIME_APPROVAL", Code: "contract_management", Environment: "prod"}
		if inject {
			request.Input = &application.SubsystemProvisioningInput{ApplicationCode: "override"}
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
		if inject {
			if reply.Success || executor.called {
				t.Fatal("executed browser supplied envelope")
			}
		} else if !reply.Success || !executor.called || len(reply.RuntimeLicenseApproval) != 1 {
			t.Fatal("approved inventory missing")
		}
	}
}
