package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/platform/license/coordination"
)

type httpRuntimeApprovalFailure struct{ err error }

func (s httpRuntimeApprovalFailure) ApprovedRuntimeLicenseComponents(context.Context, string, string) ([]application.RuntimeLicenseApproval, error) {
	return nil, s.err
}
func (s httpRuntimeApprovalFailure) Register(context.Context, coordination.ServiceSpec) error {
	panic("approval failure must not register components")
}

func TestRuntimeApprovalDiagnosticsPersistAndReturnSafeHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		err            error
		code, guidance string
	}{
		{application.ErrRuntimeLicenseDockerUnavailable, "LICENSE_RUNTIME_DOCKER_UNAVAILABLE", "docker-socket-proxy"},
		{application.ErrRuntimeLicenseApprovalUnavailable, "LICENSE_RUNTIME_APPROVAL_UNAVAILABLE", "可读取"},
		{application.ErrRuntimeLicenseApprovalMismatch, "LICENSE_RUNTIME_APPROVAL_MISMATCH", "源码"},
		{errors.New("unknown failure credential /private/approval.json"), "LICENSE_RUNTIME_APPROVAL_UNAVAILABLE", "可读取"},
		{application.ErrRuntimeLicenseStartupFailed, "LICENSE_RUNTIME_STARTUP_FAILED", "Worker"},
	} {
		t.Run(tc.code+"/"+tc.err.Error(), func(t *testing.T) {
			store := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{ApplicationCode: "customer_and_opportunity", Environment: "dev", Generation: 1}}
			provisioner := &recordingHTTPSubsystemProvisioner{}
			h := asyncDeploymentTestHandler(t, store, provisioner)
			job := subsystemDeploymentJob{TenantID: "tenant", ApplicationID: "app", EnvironmentID: "env", ApplicationCode: "customer_and_opportunity", Environment: "dev", OperatorUserID: "operator", Generation: 1, Operation: "RETRY"}
			if tc.err == application.ErrRuntimeLicenseStartupFailed {
				// 启动在 Prepare 完成后的部署边界处失败。
				provisioner.updateErr = fmt.Errorf("credential /private/startup.log: %w", tc.err)
			} else {
				source := httpRuntimeApprovalFailure{fmt.Errorf("credential /private/approval.json: %w", tc.err)}
				service, err := application.NewRuntimeLicenseEnrollmentService(source, &serviceCredentialManagerStub{}, source)
				if err != nil {
					t.Fatal(err)
				}
				h.ConfigureRuntimeLicenseEnrollment(service)
				h.ConfigureRuntimeLicenseSettings(func(context.Context) (application.RuntimeLicenseSettings, error) {
					return application.RuntimeLicenseSettings{}, nil
				})
			}
			if err := h.runSubsystemDeploymentCompletion(context.Background(), job); err == nil {
				t.Fatal("failure accepted")
			}
			if len(store.transitions) != 1 {
				t.Fatalf("transitions = %v", store.transitions)
			}
			last := store.transitions[0]
			if last.errorCode != tc.code || last.status != application.SubsystemDeploymentStatusFailed {
				t.Fatalf("diagnostic lost: %+v", last)
			}
			store.state.Status, store.state.LastErrorCode, store.state.LastError = last.status, last.errorCode, last.errorMessage
			w := httptest.NewRecorder()
			r := lifecycletestRequest(t, "/api/v1/subsystem-status?application_code=customer_and_opportunity&environment=dev", "")
			h.GetSubsystemStatus(w, r)
			if w.Code != stdhttp.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			var response struct {
				Data subsystemDeploymentStateResponse `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Data.LastErrorCode != tc.code || response.Data.LastError == "" || !strings.Contains(response.Data.NextAction, tc.guidance) {
				t.Fatalf("status diagnosis missing: %s", w.Body.String())
			}
			for _, forbidden := range []string{"credential", "/private/", "docker --", "client_secret"} {
				if strings.Contains(w.Body.String(), forbidden) {
					t.Fatalf("leaked %s", forbidden)
				}
			}
			if tc.err != application.ErrRuntimeLicenseStartupFailed && (provisioner.input.ApplicationCode != "" || provisioner.provisionCalls != 0) {
				t.Fatal("approval failure reached deployment")
			}
		})
	}
}
