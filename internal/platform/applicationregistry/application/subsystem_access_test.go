package application

import (
	"errors"
	"testing"
)

// 方案 §2.1 判定表：动作由服务端依据事实判定，用户不自行选择。
func TestDecideSubsystemAccessActionFollowsServerFacts(t *testing.T) {
	cases := []struct {
		name        string
		facts       SubsystemAccessFacts
		wantAction  SubsystemAccessAction
		wantDisplay bool
	}{
		{"范围外", SubsystemAccessFacts{InScope: false, Registered: true}, SubsystemAccessActionNone, true},
		{"未登记", SubsystemAccessFacts{InScope: true}, SubsystemAccessActionOnboard, false},
		{"已登记无受控部署", SubsystemAccessFacts{InScope: true, Registered: true}, SubsystemAccessActionAdopt, false},
		{"历史演示环境", SubsystemAccessFacts{InScope: true, Registered: true, LegacyDemo: true}, SubsystemAccessActionAdopt, false},
		{"上次失败", SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, LastOperationFailed: true}, SubsystemAccessActionRetry, false},
		{"配置漂移", SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, ConfigurationDrift: true}, SubsystemAccessActionUpdate, false},
		{"配置一致性未核验不能视为无需操作", SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true, Unverified: []string{"configuration_drift"}}, SubsystemAccessActionUpdate, false},
		{"已一致", SubsystemAccessFacts{InScope: true, Registered: true, ControlledDeployment: true}, SubsystemAccessActionNone, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision := DecideSubsystemAccessAction(testCase.facts)
			if decision.Action != testCase.wantAction {
				t.Fatalf("action = %s, want %s", decision.Action, testCase.wantAction)
			}
			if decision.DisplayOnly != testCase.wantDisplay {
				t.Fatalf("displayOnly = %v, want %v", decision.DisplayOnly, testCase.wantDisplay)
			}
			if decision.Reason == "" {
				t.Fatal("decision must carry a user-facing reason")
			}
		})
	}
}

// 失败优先于漂移：恢复失败阶段优先于发起更新。
func TestDecideSubsystemAccessActionPrefersRetryOverUpdate(t *testing.T) {
	decision := DecideSubsystemAccessAction(SubsystemAccessFacts{InScope: true, Registered: true,
		ControlledDeployment: true, LastOperationFailed: true, ConfigurationDrift: true})
	if decision.Action != SubsystemAccessActionRetry {
		t.Fatalf("action = %s, want RETRY", decision.Action)
	}
}

// 阶段顺序固定：CHECK→DIRECTORY→AUTH→DEPLOY→READINESS→RESULT，RESULT 之后无下一阶段。
func TestSubsystemAccessStageSequenceIsFixed(t *testing.T) {
	sequence := SubsystemAccessStageSequence()
	want := []SubsystemAccessStage{SubsystemAccessStageCheck, SubsystemAccessStageDirectory, SubsystemAccessStageAuth,
		SubsystemAccessStageDeploy, SubsystemAccessStageReadiness, SubsystemAccessStageResult}
	if len(sequence) != len(want) {
		t.Fatalf("sequence length = %d, want %d", len(sequence), len(want))
	}
	for index, stage := range want {
		if sequence[index] != stage {
			t.Fatalf("stage[%d] = %s, want %s", index, sequence[index], stage)
		}
		next, ok := NextSubsystemAccessStage(stage)
		if index == len(want)-1 {
			if ok {
				t.Fatalf("RESULT must not have a next stage, got %s", next)
			}
			continue
		}
		if !ok || next != want[index+1] {
			t.Fatalf("next(%s) = %s (%v), want %s", stage, next, ok, want[index+1])
		}
	}
	// 返回副本：调用方修改不得影响内部顺序。
	sequence[0] = "TAMPERED"
	if SubsystemAccessStageSequence()[0] != SubsystemAccessStageCheck {
		t.Fatal("stage sequence must be returned as a copy")
	}
	if ValidSubsystemAccessStage("DEPLOY") == false || ValidSubsystemAccessStage("deploy") || ValidSubsystemAccessStage("UNKNOWN") {
		t.Fatal("stage validation must accept only the fixed upper-case set")
	}
}

// 槽位语义：进行中的状态占用唯一槽位；结束或失败后释放。
func TestSubsystemAccessStatusSlotSemantics(t *testing.T) {
	for status, want := range map[SubsystemAccessStatus]bool{
		SubsystemAccessStatusPending: true, SubsystemAccessStatusRunning: true, SubsystemAccessStatusWaiting: true,
		SubsystemAccessStatusFailed: false, SubsystemAccessStatusSucceeded: false,
	} {
		if status.HoldsActiveSlot() != want {
			t.Fatalf("%s.HoldsActiveSlot() = %v, want %v", status, status.HoldsActiveSlot(), want)
		}
	}
	if !SubsystemAccessStatusSucceeded.Terminal() || !SubsystemAccessStatusFailed.Terminal() {
		t.Fatal("SUCCEEDED and FAILED must be terminal")
	}
	if SubsystemAccessStatusWaiting.Terminal() {
		t.Fatal("WAITING must not be terminal: the operator resumes it")
	}
}

// 槽位键必须包含租户，避免跨租户互相阻塞；缺字段必须报错而不是生成半截键。
func TestActiveSlotKeyRequiresTarget(t *testing.T) {
	key, err := ActiveSlotKey("tenant-1", "settlement", "prod")
	if err != nil || key != "tenant-1:settlement:prod" {
		t.Fatalf("key = %q err = %v", key, err)
	}
	if _, err := ActiveSlotKey("tenant-1", "settlement", " "); !errors.Is(err, ErrSubsystemAccessInvalidRequest) {
		t.Fatalf("missing environment must be rejected, got %v", err)
	}
}

// 预检三态：镜像状态"未上报"不能被当成"缺少镜像"——那会让镜像已齐全的目标
// 一直提示去准备镜像，而用户按提示操作也无济于事。
func TestSubsystemAccessMissingDistinguishesUnverifiedImages(t *testing.T) {
	unverified := describeSubsystemAccessMissing(SubsystemAccessFacts{InScope: true, Registered: true, Unverified: []string{"images"}})
	found := false
	for _, item := range unverified {
		if item.Code == "IMAGES_UNVERIFIED" {
			found = true
		}
		if item.Code == "IMAGES_MISSING" {
			t.Fatal("未验证状态不得报告为缺少镜像")
		}
	}
	if !found {
		t.Fatalf("缺少 IMAGES_UNVERIFIED：%#v", unverified)
	}

	missing := describeSubsystemAccessMissing(SubsystemAccessFacts{InScope: true, Registered: true})
	for _, item := range missing {
		if item.Code == "IMAGES_MISSING" {
			return
		}
	}
	t.Fatalf("确认缺失时必须报告 IMAGES_MISSING：%#v", missing)
}
