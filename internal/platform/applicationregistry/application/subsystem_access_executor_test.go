package application

import (
	"context"
	"errors"
	"testing"
)

type onboardingPortStub struct {
	preflight SubsystemAccessStageOutcome
	directory SubsystemAccessStageOutcome
	err       error
	calls     []string
}

func (stub *onboardingPortStub) PreflightSubsystemAccess(context.Context, SubsystemAccessOperation) (SubsystemAccessStageOutcome, error) {
	stub.calls = append(stub.calls, "preflight")
	return stub.preflight, stub.err
}

func (stub *onboardingPortStub) RegisterSubsystemAccessDirectory(context.Context, SubsystemAccessOperation) (SubsystemAccessStageOutcome, error) {
	stub.calls = append(stub.calls, "directory")
	return stub.directory, stub.err
}

type deploymentPortStub struct {
	provision SubsystemAccessStageOutcome
	update    SubsystemAccessStageOutcome
	ready     SubsystemAccessStageOutcome
	authDeleg bool
	calls     []string
}

func (stub *deploymentPortStub) ProvisionSubsystemAccess(_ context.Context, _ SubsystemAccessOperation, _ *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error) {
	stub.calls = append(stub.calls, "provision")
	return stub.provision, nil
}

func (stub *deploymentPortStub) UpdateSubsystemAccess(_ context.Context, _ SubsystemAccessOperation, _ *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error) {
	stub.calls = append(stub.calls, "update")
	return stub.update, nil
}

func (stub *deploymentPortStub) SubsystemAccessDeploymentReady(context.Context, SubsystemAccessOperation) (SubsystemAccessStageOutcome, error) {
	stub.calls = append(stub.calls, "ready")
	return stub.ready, nil
}

func (stub *deploymentPortStub) AuthHandledByProvisioning() bool { return stub.authDeleg }

func executorFor(t *testing.T, onboarding *onboardingPortStub, deployment *deploymentPortStub) *SubsystemAccessStageExecutorAdapter {
	t.Helper()
	executor, err := NewSubsystemAccessStageExecutor(onboarding, deployment)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

// 阶段到端口的路由：CHECK→预检、DIRECTORY→登记、AUTH→（由部署承载）、DEPLOY→按动作选择、READINESS→就绪核对、RESULT→证据引用。
func TestSubsystemAccessExecutorRoutesStages(t *testing.T) {
	onboarding := &onboardingPortStub{
		preflight: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "preflight:ok"},
		directory: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "directory:created"},
	}
	deployment := &deploymentPortStub{authDeleg: true,
		provision: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "provision:images"},
		update:    SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "update:images"},
		ready:     SubsystemAccessStageOutcome{Status: SubsystemAccessStatusRunning, EvidenceRef: "ready:pending"}}
	executor := executorFor(t, onboarding, deployment)
	operation := SubsystemAccessOperation{ID: "01SAOP000000000000000000001", TenantID: "tenant-1",
		Action: SubsystemAccessActionOnboard, ResultRef: ""}

	check, err := executor.ExecuteSubsystemAccessStage(context.Background(), operation, SubsystemAccessStageCheck, nil)
	if err != nil || check.EvidenceRef != "preflight:ok" {
		t.Fatalf("check = %+v err=%v", check, err)
	}
	directory, err := executor.ExecuteSubsystemAccessStage(context.Background(), operation, SubsystemAccessStageDirectory, nil)
	if err != nil || directory.EvidenceRef != "directory:created" {
		t.Fatalf("directory = %+v err=%v", directory, err)
	}
	auth, err := executor.ExecuteSubsystemAccessStage(context.Background(), operation, SubsystemAccessStageAuth, nil)
	if err != nil || auth.Status != SubsystemAccessStatusSucceeded || auth.EvidenceRef != "delegated:provisioning" {
		t.Fatalf("auth = %+v err=%v", auth, err)
	}
	deploy, err := executor.ExecuteSubsystemAccessStage(context.Background(), operation, SubsystemAccessStageDeploy, nil)
	if err != nil || deploy.EvidenceRef != "provision:images" {
		t.Fatalf("deploy = %+v err=%v", deploy, err)
	}
	if len(deployment.calls) != 1 || deployment.calls[0] != "provision" {
		t.Fatalf("deployment calls = %v", deployment.calls)
	}
	// 就绪核对返回"仍在进行中"：Worker 会保留租约并在下一轮再核对（不重复部署）。
	readiness, err := executor.ExecuteSubsystemAccessStage(context.Background(), operation, SubsystemAccessStageReadiness, nil)
	if err != nil || readiness.Status != SubsystemAccessStatusRunning {
		t.Fatalf("readiness = %+v err=%v", readiness, err)
	}
	result, err := executor.ExecuteSubsystemAccessStage(context.Background(), operation, SubsystemAccessStageResult, nil)
	if err != nil || result.Status != SubsystemAccessStatusSucceeded || result.EvidenceRef != "operation:01SAOP000000000000000000001/result" {
		t.Fatalf("result = %+v err=%v", result, err)
	}
}

// 动作路由：接管/更新/恢复走受控更新；未知动作必须报错（不猜测）。
func TestSubsystemAccessExecutorRoutesActionToUpdate(t *testing.T) {
	deployment := &deploymentPortStub{authDeleg: true, update: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "update:ok"}}
	executor := executorFor(t, &onboardingPortStub{}, deployment)
	for _, action := range []SubsystemAccessAction{SubsystemAccessActionAdopt, SubsystemAccessActionUpdate, SubsystemAccessActionRetry} {
		outcome, err := executor.ExecuteSubsystemAccessStage(context.Background(),
			SubsystemAccessOperation{ID: "op", Action: action}, SubsystemAccessStageDeploy, nil)
		if err != nil || outcome.EvidenceRef != "update:ok" {
			t.Fatalf("action %s → %+v err=%v", action, outcome, err)
		}
	}
	_, err := executor.ExecuteSubsystemAccessStage(context.Background(),
		SubsystemAccessOperation{ID: "op", Action: SubsystemAccessActionNone}, SubsystemAccessStageDeploy, nil)
	if err == nil {
		t.Fatal("NONE action must not be deployable")
	}
}

// 认证未接线且部署不承载认证时，必须报"未知/可重试"，不得静默跳过。
func TestSubsystemAccessExecutorAuthRequiresWiring(t *testing.T) {
	executor := executorFor(t, &onboardingPortStub{}, &deploymentPortStub{authDeleg: false})
	if _, err := executor.ExecuteSubsystemAccessStage(context.Background(),
		SubsystemAccessOperation{ID: "op"}, SubsystemAccessStageAuth, nil); err == nil {
		t.Fatal("auth stage without wiring must report an error instead of silently succeeding")
	}
}

// 预检缺项：outcome 原样返回（WAITING + 原因码），让 Worker 进入等待而不是失败。
func TestSubsystemAccessExecutorPropagatesWaitingOutcome(t *testing.T) {
	onboarding := &onboardingPortStub{preflight: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusWaiting,
		ErrorCode: "RUNTIME_APPROVAL_MISSING", Message: "缺少运行批准"}}
	executor := executorFor(t, onboarding, &deploymentPortStub{authDeleg: true})
	outcome, err := executor.ExecuteSubsystemAccessStage(context.Background(),
		SubsystemAccessOperation{ID: "op"}, SubsystemAccessStageCheck, nil)
	if err != nil || outcome.Status != SubsystemAccessStatusWaiting || outcome.ErrorCode != "RUNTIME_APPROVAL_MISSING" {
		t.Fatalf("outcome = %+v err=%v", outcome, err)
	}
}

// 未知阶段：报错而不是猜一个语义。
func TestSubsystemAccessExecutorRejectsUnknownStage(t *testing.T) {
	executor := executorFor(t, &onboardingPortStub{}, &deploymentPortStub{authDeleg: true})
	if _, err := executor.ExecuteSubsystemAccessStage(context.Background(),
		SubsystemAccessOperation{ID: "op"}, SubsystemAccessStage("UNKNOWN"), nil); !errors.Is(err, ErrSubsystemAccessInvalidRequest) {
		t.Fatalf("err = %v, want invalid request", err)
	}
}

// 四类动作都必须能从统一入口跑完整阶段流（不仅是直接测某个函数）：
// 未登记首次接入 / 已登记未部署接管 / 失败重试 / 已接入更新。
func TestSubsystemAccessExecutorRunsFullChainForEveryAction(t *testing.T) {
	cases := []struct {
		name     string
		action   SubsystemAccessAction
		wantCall string
	}{
		{"未登记首次接入", SubsystemAccessActionOnboard, "provision"},
		{"已登记未部署接管", SubsystemAccessActionAdopt, "update"},
		{"失败重试", SubsystemAccessActionRetry, "update"},
		{"已接入更新", SubsystemAccessActionUpdate, "update"},
	}
	for _, item := range cases {
		onboarding := &onboardingPortStub{
			preflight: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "preflight:ok"},
			directory: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "directory:ok"},
		}
		deployment := &deploymentPortStub{
			// 认证由部署承载时 AUTH 只声明"随部署生效"；真实适配器另外实现了认证端口。
			authDeleg: true,
			provision: SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "provision:ok"},
			update:    SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "update:ok"},
			ready:     SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "ready:ok"},
		}
		executor := executorFor(t, onboarding, deployment)
		operation := SubsystemAccessOperation{ID: "op-" + string(item.action), TenantID: "tenant-1", Action: item.action}
		run := &SubsystemAccessRunState{}

		for _, stage := range []SubsystemAccessStage{
			SubsystemAccessStageCheck, SubsystemAccessStageDirectory, SubsystemAccessStageAuth,
			SubsystemAccessStageDeploy, SubsystemAccessStageReadiness, SubsystemAccessStageResult,
		} {
			outcome, err := executor.ExecuteSubsystemAccessStage(context.Background(), operation, stage, run)
			if err != nil {
				t.Fatalf("%s: stage %s err=%v", item.name, stage, err)
			}
			if outcome.Status == SubsystemAccessStatusFailed {
				t.Fatalf("%s: stage %s failed: %+v", item.name, stage, outcome)
			}
		}
		if len(deployment.calls) == 0 || deployment.calls[0] != item.wantCall {
			t.Fatalf("%s: deployment calls=%v，want 首调用 %s", item.name, deployment.calls, item.wantCall)
		}
		if len(onboarding.calls) < 2 || onboarding.calls[0] != "preflight" || onboarding.calls[1] != "directory" {
			t.Fatalf("%s: onboarding calls=%v，必须先预检再登记", item.name, onboarding.calls)
		}
	}
}
