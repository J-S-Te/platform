package http

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	authctx "github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

func asyncDeploymentTestHandler(t *testing.T, stateStore *recordingSubsystemDeploymentStateStore, provisioner *recordingHTTPSubsystemProvisioner) *SubsystemOnboardingHandler {
	t.Helper()
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	handler.serviceCredentials = &serviceCredentialManagerStub{}
	return handler
}

func asyncDeploymentRequest(t *testing.T, body string) *stdhttp.Request {
	t.Helper()
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-update", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	return request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
}

// 并发部署必须以稳定错误码拒绝，而不是让两个编排互相覆盖状态机。
func TestUpdateSubsystemRejectsConcurrentInFlightDeployment(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{
		ApplicationID: "app-1", EnvironmentID: "env-1", Status: application.SubsystemDeploymentStatusUpdating, StartedAt: &now,
	}}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler := asyncDeploymentTestHandler(t, stateStore, provisioner)
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, asyncDeploymentRequest(t, `{"application_code":"contract_management","environment":"prod"}`))

	if response.Code != stdhttp.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "SUBSYSTEM_DEPLOYMENT_IN_PROGRESS") {
		t.Fatalf("body missing stable in-progress code: %s", response.Body.String())
	}
	if provisioner.input.ApplicationCode != "" {
		t.Fatalf("provisioner must not run for a rejected concurrent update: %#v", provisioner.input)
	}
	if len(stateStore.transitions) != 0 {
		t.Fatalf("rejected update must not touch the state machine: %#v", stateStore.transitions)
	}
}

// 过期的在途状态由更新入口的既有恢复语义收口为失败后放行，不再永久冻结。
func TestUpdateSubsystemProceedsAfterStaleInFlightRecovery(t *testing.T) {
	t.Parallel()
	stale := time.Now().UTC().Add(-25 * time.Minute)
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{
		ApplicationID: "app-1", EnvironmentID: "env-1", Status: application.SubsystemDeploymentStatusUpdating, StartedAt: &stale,
	}}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler := asyncDeploymentTestHandler(t, stateStore, provisioner)
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, asyncDeploymentRequest(t, `{"application_code":"contract_management","environment":"prod"}`))
	handler.waitForDeploymentJobs()

	if response.Code != stdhttp.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	if len(stateStore.transitions) < 3 {
		t.Fatalf("transitions = %#v, want recovered failure + UPDATING + terminal", stateStore.transitions)
	}
	if stateStore.transitions[0].status != application.SubsystemDeploymentStatusFailed ||
		stateStore.transitions[0].errorCode != "DEPLOYMENT_INTERRUPTED" {
		t.Fatalf("stale recovery transition = %#v", stateStore.transitions[0])
	}
}

// 后台编排窗口耗尽必须把状态收口为 PROVISION_FAILED，绝不留在 UPDATING 冻结状态机。
func TestUpdateSubsystemBackgroundTimeoutClosesStateAsFailed(t *testing.T) {
	t.Parallel()
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{ApplicationID: "app-1", EnvironmentID: "env-1"}}
	provisioner := &recordingHTTPSubsystemProvisioner{update: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	handler := asyncDeploymentTestHandler(t, stateStore, provisioner)
	handler.deploymentTimeout = 80 * time.Millisecond
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, asyncDeploymentRequest(t, `{"application_code":"contract_management","environment":"prod"}`))

	if response.Code != stdhttp.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", response.Code, response.Body.String())
	}
	handler.waitForDeploymentJobs()

	if len(stateStore.transitions) != 2 {
		t.Fatalf("transitions = %#v, want [UPDATING, PROVISION_FAILED]", stateStore.transitions)
	}
	closed := stateStore.transitions[1]
	if closed.status != application.SubsystemDeploymentStatusFailed || closed.errorCode != "DEPLOYMENT_AGENT_FAILED" {
		t.Fatalf("timeout closure = %#v, want PROVISION_FAILED/DEPLOYMENT_AGENT_FAILED", closed)
	}
}

// 清单无变化且环境已就绪时，纯更新重放直接确认现状（跳过构建），不触碰状态机。
func TestUpdateSubsystemSkipsUnchangedReadyDeployment(t *testing.T) {
	t.Parallel()
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{
		ApplicationID: "app-1", EnvironmentID: "env-1",
		Status:                  application.SubsystemDeploymentStatusReady,
		DesiredManifestChecksum: "sha256:unchanged",
		AppliedManifestChecksum: "sha256:unchanged",
	}}
	provisioner := &recordingHTTPSubsystemProvisioner{capabilities: application.SubsystemProvisioningCapabilities{
		Targets: []application.SubsystemProvisioningTarget{{
			ApplicationCode: "contract_management", Environment: "prod", ManifestChecksum: "sha256:unchanged",
		}},
	}}
	handler := asyncDeploymentTestHandler(t, stateStore, provisioner)
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, asyncDeploymentRequest(t, `{"application_code":"contract_management","environment":"prod"}`))

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":"READY"`) {
		t.Fatalf("body missing unchanged contract: %s", response.Body.String())
	}
	if provisioner.input.ApplicationCode != "" {
		t.Fatalf("unchanged update must not rebuild: %#v", provisioner.input)
	}
	if len(stateStore.transitions) != 0 {
		t.Fatalf("unchanged update must not touch the state machine: %#v", stateStore.transitions)
	}
}
