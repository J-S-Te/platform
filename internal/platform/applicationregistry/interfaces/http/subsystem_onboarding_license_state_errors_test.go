package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
)

// updateSubsystemFailureTransition 触发一次失败的后台更新，并返回落库的失败转移。
// 参数：t 为测试上下文，cause 为部署 Agent 返回的下层原因。
// 返回值：状态机记录的失败转移。
func updateSubsystemFailureTransition(t *testing.T, cause error) recordedDeploymentTransition {
	t.Helper()
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{
		ApplicationID: "app-1", EnvironmentID: "env-1", InitialAdminUserID: "01K10B00000000000000000001",
	}}
	provisioner := &recordingHTTPSubsystemProvisioner{updateErr: cause}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	// project_management 会在启动容器前要求授权目录发布凭据；用一个不做网络访问的
	// 凭据管理器满足该前置条件，使失败发生在部署 Agent（本测试的观测点）。
	handler.serviceCredentials = &serviceCredentialManagerStub{}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-update",
		bytes.NewBufferString(`{"application_code":"project_management","environment":"dev"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)
	handler.waitForDeploymentJobs()

	if response.Code != stdhttp.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	for index := len(stateStore.transitions) - 1; index >= 0; index-- {
		if stateStore.transitions[index].status == application.SubsystemDeploymentStatusFailed {
			return stateStore.transitions[index]
		}
	}
	t.Fatalf("no failed transition recorded: %#v", stateStore.transitions)
	return recordedDeploymentTransition{}
}

// TestSubsystemProvisioningDiagnosesLicenseRuntimeState 覆盖运维可见的错误映射：
// 许可运行态未下发必须映射成专用错误码与可执行指引，而不是“部署 Agent 执行失败”。
func TestSubsystemProvisioningDiagnosesLicenseRuntimeState(t *testing.T) {
	t.Parallel()
	guard := fmt.Errorf("%w for project_management", application.ErrRuntimeLicenseStateUnavailable)
	if code := subsystemProvisioningErrorCode(guard); code != "LICENSE_RUNTIME_STATE_UNAVAILABLE" {
		t.Fatalf("error code = %q, want LICENSE_RUNTIME_STATE_UNAVAILABLE", code)
	}
	nextAction := subsystemProvisioningNextAction(guard)
	for _, want := range []string{"商业许可运行态未下发", "license-state 卷", "不要重复创建应用环境"} {
		if !strings.Contains(nextAction, want) {
			t.Fatalf("next action = %q, want substring %q", nextAction, want)
		}
	}
	if strings.Contains(nextAction, "子系统构建或启动失败") {
		t.Fatalf("license runtime state gap fell back to the generic next action: %q", nextAction)
	}

	// 底层 Compose/容器原因：真实原因是许可配置失败时，必须先命中该分支。
	composeFailure := fmt.Errorf("%w: rebuild subsystem containers: exit status 1: compose output: "+
		"project-api | commercial license configuration failed", application.ErrSubsystemProvisioningUnavailable)
	if code := subsystemProvisioningErrorCode(composeFailure); code != "LICENSE_RUNTIME_CONFIGURATION_FAILED" {
		t.Fatalf("error code = %q, want LICENSE_RUNTIME_CONFIGURATION_FAILED", code)
	}
	composeAction := subsystemProvisioningNextAction(composeFailure)
	for _, want := range []string{"商业许可配置失败", "license-state 卷挂载"} {
		if !strings.Contains(composeAction, want) {
			t.Fatalf("next action = %q, want substring %q", composeAction, want)
		}
	}
	if strings.Contains(composeAction, "子系统构建或启动失败") {
		t.Fatalf("license configuration failure fell back to the generic next action: %q", composeAction)
	}
}

// TestSubsystemDeploymentPersistsLicenseRuntimeStateFailure 覆盖状态表：失败原因必须写进
// subsystem_deployment_state（last_error_code/last_error_message），而不是通用文案。
func TestSubsystemDeploymentPersistsLicenseRuntimeStateFailure(t *testing.T) {
	t.Parallel()
	guard := fmt.Errorf("%w for project_management", application.ErrRuntimeLicenseStateUnavailable)
	failed := updateSubsystemFailureTransition(t, guard)
	if failed.errorCode != "LICENSE_RUNTIME_STATE_UNAVAILABLE" {
		t.Fatalf("error code = %q, want LICENSE_RUNTIME_STATE_UNAVAILABLE", failed.errorCode)
	}
	for _, want := range []string{"商业许可运行态未下发", "license-state 卷"} {
		if !strings.Contains(failed.errorMessage, want) {
			t.Fatalf("error message = %q, want substring %q", failed.errorMessage, want)
		}
	}
	if strings.Contains(failed.errorMessage, "部署 Agent 执行失败") {
		t.Fatalf("persisted message = %q, want the actionable cause", failed.errorMessage)
	}
}

// TestSubsystemDeploymentPersistsComposeCauseAsSingleLineJSON 覆盖最影响运维的一条：
// 容器/Compose 下层原因必须进入状态表，且截断、脱敏、单行，不破坏 varchar(1000) 与 JSON。
func TestSubsystemDeploymentPersistsComposeCauseAsSingleLineJSON(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("%w: rebuild subsystem containers: exit status 1: compose output: "+
		"project-api | commercial license configuration failed\nproject-api restarted every minute\n%s",
		application.ErrSubsystemProvisioningUnavailable, strings.Repeat("许可运行态", 400))
	failed := updateSubsystemFailureTransition(t, cause)
	if failed.errorCode != "LICENSE_RUNTIME_CONFIGURATION_FAILED" {
		t.Fatalf("error code = %q, want LICENSE_RUNTIME_CONFIGURATION_FAILED", failed.errorCode)
	}
	for _, want := range []string{"rebuild subsystem containers", "commercial license configuration failed"} {
		if !strings.Contains(failed.errorMessage, want) {
			t.Fatalf("error message = %q, want substring %q", failed.errorMessage, want)
		}
	}
	if strings.ContainsAny(failed.errorMessage, "\r\n\t") {
		t.Fatalf("error message contains line breaks: %q", failed.errorMessage)
	}
	if utf8.RuneCountInString(failed.errorMessage) > 800+len("...(truncated)") {
		t.Fatalf("error message exceeds the column budget: %d runes", utf8.RuneCountInString(failed.errorMessage))
	}
	if !utf8.ValidString(failed.errorMessage) {
		t.Fatal("error message is not valid UTF-8")
	}
	if _, err := json.Marshal(map[string]string{"last_error_message": failed.errorMessage}); err != nil {
		t.Fatalf("persisted message is not JSON safe: %v", err)
	}
}

// TestSubsystemDeploymentKeepsGenericMessageForContextCancellation 覆盖既有语义：
// 上下文取消与裸哨兵不携带新信息时，失败文案保持“部署 Agent 执行失败”。
func TestSubsystemDeploymentKeepsGenericMessageForContextCancellation(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, application.ErrSubsystemProvisioningUnavailable} {
		cause := cause
		t.Run(cause.Error(), func(t *testing.T) {
			t.Parallel()
			failed := updateSubsystemFailureTransition(t, cause)
			if failed.errorCode != "DEPLOYMENT_AGENT_FAILED" || failed.errorMessage != "部署 Agent 执行失败" {
				t.Fatalf("failure summary = %#v, want the legacy generic summary", failed)
			}
		})
	}
}
