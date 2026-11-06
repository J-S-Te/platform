package application

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	supersededTestReceiptChecksum = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	supersededTestCurrentChecksum = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	supersededTestReason          = "已核对原代次无成功回执且无终止结果，确认由新发布取代"
)

// supersededRecoveryPortStub 模拟 Agent 的预检事实与平台收口端口。
type supersededRecoveryPortStub struct {
	evidence SubsystemAccessRecoveryEvidence
	repo     *accessRepositoryStub
	err      error
	calls    int
	seen     SubsystemAccessManualRecoveryInput
}

// PreviewSubsystemAccessRecovery 返回预置的 Agent 事实。
// 参数：其余参数为已授权原任务，本替身不访问外部系统。
// 返回值：预置事实或错误，用于区分 fenced 与 superseded 模式判定。
func (p *supersededRecoveryPortStub) PreviewSubsystemAccessRecovery(_ context.Context,
	_ SubsystemAccessOperation) (SubsystemAccessRecoveryEvidence, error) {
	return p.evidence, p.err
}

// AbandonSubsystemAccess 记录应用层下发的收口输入。
// 参数：operation为原任务，input为已校验的人工决定。
// 返回值：预置错误；真实事务与代次保护由 MySQL 集成测试覆盖。
func (p *supersededRecoveryPortStub) AbandonSubsystemAccess(_ context.Context,
	operation SubsystemAccessOperation, input SubsystemAccessManualRecoveryInput) error {
	p.calls++
	p.seen = input
	if p.err != nil {
		return p.err
	}
	operation.Status, operation.ErrorCode = SubsystemAccessStatusFailed, AccessDeploymentSupersededUnknown
	if p.repo != nil {
		p.repo.operations[operation.ID] = operation
	}
	return nil
}

// supersededRecoveryOperation 构造等待收口的原未知派发任务。
// 参数：无。
// 返回值：WAITING/DEPLOY/DEPLOYMENT_EFFECT_UNKNOWN 且无活动租约的操作。
func supersededRecoveryOperation() SubsystemAccessOperation {
	return SubsystemAccessOperation{ID: "01SAOP000000000000000000041", TenantID: "tenant-1",
		ApplicationID: "app-1", EnvironmentID: "env-1", ApplicationCode: "contract_management",
		EnvironmentCode: "prod", Action: SubsystemAccessActionUpdate,
		Stage: SubsystemAccessStageDeploy, Status: SubsystemAccessStatusWaiting,
		ErrorCode: AccessDeploymentEffectUnknown, Version: 9}
}

// supersededRecoveryEvidence 构造"审核基准已变更"的 Agent 否定证明。
// 参数：无。
// 返回值：回执清单与当前清单不同、且没有任何运行事实的预检结果。
func supersededRecoveryEvidence() SubsystemAccessRecoveryEvidence {
	return SubsystemAccessRecoveryEvidence{
		Receipt: SubsystemAccessDeploymentReceipt{TenantID: "tenant-1", OperationID: "01SAOP000000000000000000041",
			ApplicationCode: "contract_management", Environment: "prod", Generation: 5,
			ManifestChecksum: supersededTestReceiptChecksum},
		CurrentProfileChecksum: supersededTestCurrentChecksum,
	}
}

// supersededRecoveryService 装配带人工恢复端口的应用服务与仓储替身。
// 参数：t为测试上下文，operation为预置原任务。
// 返回值：服务、仓储、端口。
func supersededRecoveryService(t *testing.T, operation SubsystemAccessOperation,
	port *supersededRecoveryPortStub) (*SubsystemAccessService, *accessRepositoryStub) {
	t.Helper()
	repository := newAccessRepositoryStub()
	repository.operations[operation.ID] = operation
	service := accessServiceFor(t, &accessFactsStub{}, repository)
	port.repo = repository
	service.SetManualRecovery(port)
	return service, repository
}

// TestManualRecoveryPreviewDetectsSupersededBaseline 验证清单漂移时预检给出取代模式与双摘要。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，superseded 模式允许运行事实为空。
func TestManualRecoveryPreviewDetectsSupersededBaseline(t *testing.T) {
	operation := supersededRecoveryOperation()
	port := &supersededRecoveryPortStub{evidence: supersededRecoveryEvidence()}
	service, _ := supersededRecoveryService(t, operation, port)

	preview, err := service.PreviewManualRecovery(context.Background(), operation.TenantID, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Mode != SubsystemAccessRecoveryModeSuperseded ||
		preview.ReceiptManifestChecksum != supersededTestReceiptChecksum ||
		preview.CurrentProfileChecksum != supersededTestCurrentChecksum ||
		len(preview.Evidence.RuntimeFiles) != 0 || len(preview.Evidence.Containers) != 0 {
		t.Fatalf("superseded preview = %+v", preview)
	}
	if preview.OperationVersion != operation.Version || preview.ErrorCode != AccessDeploymentEffectUnknown {
		t.Fatalf("preview lost operation facts: %+v", preview)
	}
}

// TestManualRecoveryPreviewKeepsFencedMode 验证清单未漂移时仍走原有 fenced 模式。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，fenced 模式仍要求运行事实与证据摘要。
func TestManualRecoveryPreviewKeepsFencedMode(t *testing.T) {
	operation := supersededRecoveryOperation()
	evidence := supersededRecoveryEvidence()
	evidence.CurrentProfileChecksum = supersededTestReceiptChecksum
	evidence.AlreadyFenced = true
	evidence.EvidenceDigest = strings.Repeat("a", 64)
	evidence.RuntimeFiles = []SubsystemAccessRecoveryFile{{Path: "runtime/contract.env", Present: true}}
	evidence.Containers = []SubsystemAccessRecoveryContainer{{ServiceID: "contract-api"}}
	port := &supersededRecoveryPortStub{evidence: evidence}
	service, _ := supersededRecoveryService(t, operation, port)

	preview, err := service.PreviewManualRecovery(context.Background(), operation.TenantID, operation.ID)
	if err != nil || preview.Mode != SubsystemAccessRecoveryModeFenced {
		t.Fatalf("fenced preview = %+v %v", preview, err)
	}
}

// TestManualRecoveryPreviewRejectsPositiveDeploymentEvidence 验证成功回执或终止结果不能被取代收口。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，两种肯定事实都必须返回冲突且不进入收口。
func TestManualRecoveryPreviewRejectsPositiveDeploymentEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*SubsystemAccessRecoveryEvidence){
		"success_receipt":  func(evidence *SubsystemAccessRecoveryEvidence) { evidence.SuccessReceipt = true },
		"terminal_outcome": func(evidence *SubsystemAccessRecoveryEvidence) { evidence.TerminalOutcome = true },
	} {
		t.Run(name, func(t *testing.T) {
			operation := supersededRecoveryOperation()
			evidence := supersededRecoveryEvidence()
			mutate(&evidence)
			port := &supersededRecoveryPortStub{evidence: evidence}
			service, _ := supersededRecoveryService(t, operation, port)
			if _, err := service.PreviewManualRecovery(context.Background(), operation.TenantID, operation.ID); !errors.Is(err, ErrSubsystemAccessOperationConflict) {
				t.Fatalf("positive deployment evidence accepted: %v", err)
			}
		})
	}
}

// TestManualRecoveryPreviewRejectsRuntimeFactsWhenSuperseded 验证取代模式不接受当前运行事实冒充核对。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，审核基准已变更时运行事实必须为空。
func TestManualRecoveryPreviewRejectsRuntimeFactsWhenSuperseded(t *testing.T) {
	operation := supersededRecoveryOperation()
	evidence := supersededRecoveryEvidence()
	evidence.RuntimeFiles = []SubsystemAccessRecoveryFile{{Path: "runtime/contract.env", Present: true}}
	port := &supersededRecoveryPortStub{evidence: evidence}
	service, _ := supersededRecoveryService(t, operation, port)
	if _, err := service.PreviewManualRecovery(context.Background(), operation.TenantID, operation.ID); !errors.Is(err, ErrSubsystemAccessOperationConflict) {
		t.Fatalf("superseded mode accepted current runtime facts: %v", err)
	}
}

// TestManualRecoveryRejectsActiveLeaseBeforeAgent 验证有活动租约时不会触达 Agent。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，租约存在说明结果仍可能被写入，必须拒绝人工收口。
func TestManualRecoveryRejectsActiveLeaseBeforeAgent(t *testing.T) {
	operation := supersededRecoveryOperation()
	operation.LeaseOwner = "worker-1"
	port := &supersededRecoveryPortStub{evidence: supersededRecoveryEvidence()}
	service, _ := supersededRecoveryService(t, operation, port)
	if _, err := service.PreviewManualRecovery(context.Background(), operation.TenantID, operation.ID); !errors.Is(err, ErrSubsystemAccessOperationConflict) {
		t.Fatalf("active lease reached agent: %v", err)
	}
	input := supersededRecoveryInput(operation)
	input.Mode, input.ReceiptManifestChecksum, input.CurrentProfileChecksum =
		SubsystemAccessRecoveryModeSuperseded, supersededTestReceiptChecksum, supersededTestCurrentChecksum
	if _, err := service.AbandonManualRecovery(context.Background(), input); !errors.Is(err, ErrSubsystemAccessOperationConflict) || port.calls != 0 {
		t.Fatalf("active lease reached agent: calls=%d err=%v", port.calls, err)
	}
}

// supersededRecoveryInput 构造 superseded 模式的人工收口输入。
// 参数：operation为原任务。
// 返回值：版本/代次与原因齐备，模式仍待显式设置。
func supersededRecoveryInput(operation SubsystemAccessOperation) SubsystemAccessManualRecoveryInput {
	return SubsystemAccessManualRecoveryInput{TenantID: operation.TenantID, OperationID: operation.ID,
		OperatorID: "operator-1", ExpectedVersion: operation.Version, Generation: 5, Reason: supersededTestReason}
}

// TestManualRecoverySupersededInputRequiresBaselineChecksums 验证取代模式必须携带预检双摘要。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，缺失或相等的摘要一律冲突且不触达端口。
func TestManualRecoverySupersededInputRequiresBaselineChecksums(t *testing.T) {
	operation := supersededRecoveryOperation()
	cases := map[string]func(*SubsystemAccessManualRecoveryInput){
		"missing_receipt": func(input *SubsystemAccessManualRecoveryInput) { input.ReceiptManifestChecksum = "" },
		"missing_current": func(input *SubsystemAccessManualRecoveryInput) { input.CurrentProfileChecksum = "" },
		"blank_current":   func(input *SubsystemAccessManualRecoveryInput) { input.CurrentProfileChecksum = "   " },
		"equal_checksums": func(input *SubsystemAccessManualRecoveryInput) {
			input.ReceiptManifestChecksum, input.CurrentProfileChecksum = supersededTestCurrentChecksum, supersededTestCurrentChecksum
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			port := &supersededRecoveryPortStub{evidence: supersededRecoveryEvidence()}
			service, _ := supersededRecoveryService(t, operation, port)
			input := supersededRecoveryInput(operation)
			input.Mode, input.ReceiptManifestChecksum, input.CurrentProfileChecksum =
				SubsystemAccessRecoveryModeSuperseded, supersededTestReceiptChecksum, supersededTestCurrentChecksum
			mutate(&input)
			if _, err := service.AbandonManualRecovery(context.Background(), input); !errors.Is(err, ErrSubsystemAccessOperationConflict) {
				t.Fatalf("incomplete superseded input accepted: %v", err)
			}
			if port.calls != 0 {
				t.Fatal("incomplete superseded input reached the recovery port")
			}
		})
	}
}

// TestManualRecoverySupersededForwardsModeAndChecksums 验证合法取代请求原样下传且不要求证据摘要。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，模式、双摘要与版本/代次必须保持操作者携带的值。
func TestManualRecoverySupersededForwardsModeAndChecksums(t *testing.T) {
	operation := supersededRecoveryOperation()
	port := &supersededRecoveryPortStub{evidence: supersededRecoveryEvidence()}
	service, _ := supersededRecoveryService(t, operation, port)
	input := supersededRecoveryInput(operation)
	input.Mode, input.ReceiptManifestChecksum, input.CurrentProfileChecksum =
		SubsystemAccessRecoveryModeSuperseded, supersededTestReceiptChecksum, supersededTestCurrentChecksum

	view, err := service.AbandonManualRecovery(context.Background(), input)
	if err != nil || port.calls != 1 {
		t.Fatalf("superseded abandon: calls=%d err=%v", port.calls, err)
	}
	if port.seen.Mode != SubsystemAccessRecoveryModeSuperseded ||
		port.seen.ReceiptManifestChecksum != supersededTestReceiptChecksum ||
		port.seen.CurrentProfileChecksum != supersededTestCurrentChecksum ||
		port.seen.ExpectedVersion != operation.Version || port.seen.Generation != 5 {
		t.Fatalf("superseded input was not forwarded verbatim: %+v", port.seen)
	}
	if view.Operation.Status != SubsystemAccessStatusFailed {
		t.Fatalf("superseded abandon view = %+v", view)
	}
}

// TestManualRecoveryRejectsUnknownMode 验证未知模式不得退化成 fenced。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，未知模式按非法请求拒绝且不触达端口。
func TestManualRecoveryRejectsUnknownMode(t *testing.T) {
	operation := supersededRecoveryOperation()
	port := &supersededRecoveryPortStub{evidence: supersededRecoveryEvidence()}
	service, _ := supersededRecoveryService(t, operation, port)
	input := supersededRecoveryInput(operation)
	input.Mode = "force"
	if _, err := service.AbandonManualRecovery(context.Background(), input); !errors.Is(err, ErrSubsystemAccessInvalidRequest) {
		t.Fatalf("unknown mode accepted: %v", err)
	}
	if port.calls != 0 {
		t.Fatal("unknown mode reached the recovery port")
	}
}

// TestManualRecoveryKeepsFencedRequestCompatible 验证不带模式的既有请求仍按 fenced 下传。
// 参数：t为应用服务及 Agent 替身上下文。
// 返回值：无，旧客户端无需携带新字段即可继续人工核验。
func TestManualRecoveryKeepsFencedRequestCompatible(t *testing.T) {
	operation := supersededRecoveryOperation()
	port := &supersededRecoveryPortStub{evidence: supersededRecoveryEvidence()}
	service, _ := supersededRecoveryService(t, operation, port)
	input := supersededRecoveryInput(operation)
	input.EvidenceDigest = strings.Repeat("b", 64)
	if _, err := service.AbandonManualRecovery(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if port.calls != 1 || port.seen.Mode != SubsystemAccessRecoveryModeFenced {
		t.Fatalf("fenced compatibility lost: calls=%d mode=%q", port.calls, port.seen.Mode)
	}
}
