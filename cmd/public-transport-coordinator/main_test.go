package main

import (
	"strings"
	"testing"
)

// abort 是 fail 的显式确认版本：它放弃当前切换，并让协调器恢复降级期被截断的会话有效期。
// 因此必须强制 --yes，且阶段固定为 OPERATOR_ABORT（不允许改写成其它阶段）。
func TestParseAbortRequestRequiresExplicitConfirmation(t *testing.T) {
	if _, err := parseAbortRequest(nil); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("abort without --yes error = %v, want a --yes confirmation error", err)
	}
	if _, err := parseAbortRequest([]string{"--transition-id", "01TRANSITION000000000000001"}); err == nil {
		t.Fatal("abort without --yes must be rejected even with a transition id")
	}
	request, err := parseAbortRequest([]string{"--yes", "--transition-id", "01TRANSITION000000000000001"})
	if err != nil {
		t.Fatalf("abort --yes: %v", err)
	}
	if request.Stage != operatorAbortStage || request.TransitionID != "01TRANSITION000000000000001" || request.Message != defaultAbortMessage {
		t.Fatalf("abort request = %#v", request)
	}
	// 阶段不允许被覆盖：abort 的语义就是"运维放弃"，不能被伪装成别的失败阶段。
	if _, err := parseAbortRequest([]string{"--yes", "--stage", "KEYCLOAK_DUAL_CALLBACK"}); err == nil {
		t.Fatal("abort must reject --stage")
	}
	// 位置参数不允许：避免把误输入当成确认。
	if _, err := parseAbortRequest([]string{"--yes", "extra"}); err == nil {
		t.Fatal("abort must reject positional arguments")
	}
}

// fail 保持既有兼容性：不需要 --yes，默认阶段仍是 OPERATOR_ABORT，--stage 可覆盖。
func TestParseFailRequestKeepsExistingSemantics(t *testing.T) {
	request, err := parseFailRequest(nil)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if request.Stage != operatorAbortStage || request.TransitionID != "" || request.Message != defaultAbortMessage {
		t.Fatalf("default fail request = %#v", request)
	}
	request, err = parseFailRequest([]string{"--transition-id", "01TRANSITION000000000000001", "--stage", "KEYCLOAK_DUAL_CALLBACK", "--message", "dual callbacks rejected"})
	if err != nil {
		t.Fatalf("fail with stage: %v", err)
	}
	if request.Stage != "KEYCLOAK_DUAL_CALLBACK" || request.TransitionID != "01TRANSITION000000000000001" || request.Message != "dual callbacks rejected" {
		t.Fatalf("fail request = %#v", request)
	}
	// fail 不接受 --yes：未知参数必须报错而不是被静默忽略。
	if _, err := parseFailRequest([]string{"--yes"}); err == nil {
		t.Fatal("fail must reject --yes")
	}
	// 位置参数同样拒绝：命令行拼错时不能悄悄放弃一次切换。
	if _, err := parseFailRequest([]string{"extra"}); err == nil {
		t.Fatal("fail must reject positional arguments")
	}
}
