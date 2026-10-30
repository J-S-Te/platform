package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type projectReadinessRunner struct {
	*projectBackgroundFixtureRunner
	activate, ensured, resumed   bool
	routingReads, executionReads int
	neverReady, wrongTenant      bool
	verifyExit, pausedLicense    bool
	ensureExit                   bool
	inspectionFault              string
	cancelInspection             context.CancelFunc
}

// RunOutput 在真实生产控制调用边界模拟先滞后、后可观测的后台状态。
// 参数：ctx及命令参数由正式激活流程提供；不启动容器或外部连接。
// 返回值：身份绑定的真实协议形状；verifyExit模拟CLI非零及结构化拒绝。
func (r *projectReadinessRunner) RunOutput(ctx context.Context, dir string, env []string,
	command string, args ...string) ([]byte, error) {
	output, err := r.projectBackgroundFixtureRunner.RunOutput(ctx, dir, env, command, args...)
	action := projectReadinessAction(args)
	if err != nil || !r.activate || action == "" {
		return output, err
	}
	var state map[string]any
	if err := json.Unmarshal(output, &state); err != nil {
		return nil, err
	}
	switch action {
	case "ensure":
		r.ensured = true
	case "resume":
		r.resumed = true
	case "inspect":
		if r.ensured && !r.resumed {
			r.routingReads++
		}
		if r.resumed {
			r.executionReads++
		}
	}
	state["routing_ready"] = r.ensured && r.routingReads > 1 && !r.neverReady
	state["execution_ready"] = r.resumed && r.executionReads > 1
	state["paused_reason"] = "PAUSED_OPERATION"
	if r.pausedLicense {
		state["paused_reason"] = "PAUSED_LICENSE"
	}
	if r.wrongTenant && r.ensured {
		state["tenant_id"] = "other-tenant"
	}
	if action == "inspect" && r.ensured {
		if r.cancelInspection != nil {
			r.cancelInspection()
			r.cancelInspection = nil
		}
		switch r.inspectionFault {
		case "build drift":
			state["build_id"] = "other-build"
		case "environment drift":
			state["environment"] = "other-environment"
		case "engine drift":
			state["engine"] = "legacy"
		case "protocol drift":
			state["protocol"] = 2
		case "malformed":
			return []byte("invalid JSON"), nil
		}
	}
	if action == "verify" && r.verifyExit {
		state["error_code"] = "CONTROL_BLOCKED"
		state["execution_ready"] = false
		output, _ = json.Marshal(state)
		return output, errors.New("synthetic CLI exit 1")
	}
	if action == "ensure" && r.ensureExit {
		state["error_code"] = "CONTROL_BLOCKED"
		state["paused_reason"] = "POLLING_UNPROMOTED"
		operation := ""
		for i, arg := range args {
			if arg == "--operation-id" && i+1 < len(args) {
				operation = args[i+1]
			}
		}
		state["schedules"] = []any{
			map[string]any{"operation_id": operation},
			map[string]any{"operation_id": operation},
			map[string]any{"operation_id": operation},
		}
		output, _ = json.Marshal(state)
		return append(output, []byte("\nbackground control blocked\n")...), errors.New("synthetic CLI exit 1")
	}
	return json.Marshal(state)
}

// projectReadinessAction 读取正式控制命令的固定动作参数。
// 参数：args为生产调用传入的参数数组。
// 返回值：控制动作；普通容器检查返回空。
func projectReadinessAction(args []string) string {
	for i, arg := range args {
		if arg == "--action" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// projectReadinessFixture 在独立目录中准备同镜像候选及可观测控制边界。
// 参数：t为测试上下文；复用既有正式目标夹具，不接触运行环境。
// 返回值：正式目标及状态可控的命令适配器。
func projectReadinessFixture(t *testing.T) (*productionComposeTarget, *projectReadinessRunner) {
	t.Helper()
	target, base := projectBackgroundTargetFixture(t)
	if err := target.prepareProjectBackground(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner := &projectReadinessRunner{projectBackgroundFixtureRunner: base, activate: true}
	target.runner = runner
	return target, runner
}

// TestProjectBackgroundWaitsForObservedRoutingAndExecution 重现控制完成后状态可见性滞后。
// 参数：t为测试上下文；期望来自独立的异步就绪契约。
// 返回值：无；过早失败、重复变更动作或提前标记complete时失败。
func TestProjectBackgroundWaitsForObservedRoutingAndExecution(t *testing.T) {
	t.Run("control exits zero", func(t *testing.T) { assertProjectBackgroundDelayedReadiness(t, false) })
	t.Run("ensure exits one with structured pending", func(t *testing.T) {
		assertProjectBackgroundDelayedReadiness(t, true)
	})
}

// assertProjectBackgroundDelayedReadiness 检查真实CLI成功与结构化未就绪两种形态。
// 参数：t为测试上下文，ensureExit表示提升后CLI尚未观察到路由而非零退出。
// 返回值：无；控制动作重发或未等待真实观测时失败。
func assertProjectBackgroundDelayedReadiness(t *testing.T, ensureExit bool) {
	t.Helper()
	target, runner := projectReadinessFixture(t)
	runner.ensureExit = ensureExit
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := target.activateProjectBackground(ctx); err != nil {
		t.Fatalf("candidate became ready within budget: %v", err)
	}
	if runner.routingReads < 2 || runner.executionReads < 2 {
		t.Fatal("readiness was inferred from control exit instead of observed")
	}
	for _, action := range []string{"ensure", "resume", "verify"} {
		count := 0
		for _, call := range runner.calls {
			if strings.Contains(call, "--action "+action+" ") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("mutating/final action %s called %d times, want once", action, count)
		}
	}
	operation, err := target.loadProjectBackgroundOperation()
	if err != nil || operation.Stage != "complete" {
		t.Fatal("observed ready candidate was not completed")
	}
}

// TestProjectBackgroundReadinessFailsClosed 覆盖持续等待、取消、身份漂移和非零CLI。
// 参数：t为测试上下文；全部边界使用独立的短期限或固定错误。
// 返回值：无；未通过门禁却写complete时失败。
func TestProjectBackgroundReadinessFailsClosed(t *testing.T) {
	for _, name := range []string{"never ready", "cancelled", "cancel during inspection",
		"wrong tenant", "build drift", "environment drift", "engine drift",
		"protocol drift", "malformed", "verify exit", "license"} {
		t.Run(name, func(t *testing.T) {
			target, runner := projectReadinessFixture(t)
			runner.neverReady = name == "never ready"
			runner.wrongTenant = name == "wrong tenant"
			runner.verifyExit = name == "verify exit"
			runner.pausedLicense = name == "license"
			runner.inspectionFault = name
			budget := 5 * time.Second
			if name == "never ready" {
				budget = 30 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			if name == "cancelled" {
				cancel()
			}
			if name == "cancel during inspection" {
				runner.cancelInspection = cancel
			}
			if err := target.activateProjectBackground(ctx); err == nil {
				t.Fatal("blocked candidate reported successful activation")
			}
			operation, err := target.loadProjectBackgroundOperation()
			if err != nil || operation.Stage == "complete" {
				t.Fatal("blocked candidate lost recoverable pending state")
			}
		})
	}
}

// TestProjectBackgroundPendingEnsureRequiresExactCLIProof 拒绝把任意CLI失败当作可等待状态。
// 参数：t为测试上下文；状态和固定stderr均为独立协议样例。
// 返回值：无；错身份、错动作、跨操作、坏协议或额外输出被接纳时失败。
func TestProjectBackgroundPendingEnsureRequiresExactCLIProof(t *testing.T) {
	build := "project-background-sha256-" + strings.Repeat("a", 64)
	for _, name := range []string{"valid", "tenant", "build", "environment", "engine", "protocol",
		"bootstrap", "license", "owner", "schedules", "missing trailer", "extra output", "verify", "resume"} {
		t.Run(name, func(t *testing.T) {
			state := map[string]any{
				"protocol": 1, "engine": "temporal", "tenant_id": "qa-tenant", "environment": "qa",
				"build_id": build, "bootstrap_ready": true, "routing_ready": false, "execution_ready": false,
				"paused_reason": "POLLING_UNPROMOTED", "error_code": "CONTROL_BLOCKED",
				"schedules": []any{map[string]any{"operation_id": "own"},
					map[string]any{"operation_id": "own"}, map[string]any{"operation_id": "own"}},
			}
			action := "ensure"
			switch name {
			case "tenant":
				state["tenant_id"] = "other"
			case "build":
				state["build_id"] = "other"
			case "environment":
				state["environment"] = "other"
			case "engine":
				state["engine"] = "legacy"
			case "protocol":
				state["protocol"] = 2
			case "bootstrap":
				state["bootstrap_ready"] = false
			case "license":
				state["paused_reason"] = "PAUSED_LICENSE"
			case "owner":
				state["schedules"].([]any)[0] = map[string]any{"operation_id": "other"}
			case "schedules":
				state["schedules"] = []any{}
			case "verify", "resume":
				action = name
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if name != "missing trailer" {
				raw = append(raw, []byte("\nbackground control blocked\n")...)
			}
			if name == "extra output" {
				raw = append(raw, []byte("unexpected diagnostic")...)
			}
			_, err = projectBackgroundControlResult(raw, errors.New("synthetic exit 1"), action,
				"own", build, "qa-tenant", "qa", false)
			if errors.Is(err, errProjectBackgroundEnsurePending) != (name == "valid") {
				t.Fatalf("CLI failure classification mismatch for %s", name)
			}
		})
	}
}
