package infrastructure

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type rejectionRecoveryState struct {
	recoveryReceiptState
	rejected bool
}

// RejectAccessDeployment 为测试记录同代次确定拒绝的正式失败收口。
// 参数：operation为当前任务，value为Agent原始持久结果。
// 返回值：测试中无额外仓储错误。
func (s *rejectionRecoveryState) RejectAccessDeployment(_ context.Context,
	_ application.SubsystemAccessOperation, _ application.SubsystemAccessDeploymentOutcome) error {
	s.rejected = true
	s.phase = "FAILED"
	return nil
}

type rejectionRecoveryAgent struct {
	recoveryReceiptAgent
	outcomeErr error
}

// ReadAccessDeploymentOutcome 读取测试Agent实际持久的拒绝文件。
// 参数：receipt为原任务代次绑定。
// 返回值：返回真实文件结果及错误，不用固定成功值。
func (a *rejectionRecoveryAgent) ReadAccessDeploymentOutcome(_ context.Context,
	receipt application.SubsystemAccessDeploymentReceipt) (application.SubsystemAccessDeploymentOutcome, bool, error) {
	if a.outcomeErr != nil {
		return application.SubsystemAccessDeploymentOutcome{}, false, a.outcomeErr
	}
	return readAccessDeploymentOutcome(a.root, receipt)
}

// TestAccessRecoveryOfDurableCredentialRejection 验证真实拒绝不被永久提升为未知。
// 参数：t为隔离结果目录及部署适配器的测试上下文。
// 返回值：无，拒绝只能收口失败且不能重放部署。
func TestAccessRecoveryOfDurableCredentialRejection(t *testing.T) {
	op := onboardingOperation()
	receipt := application.SubsystemAccessDeploymentReceipt{TenantID: op.TenantID,
		OperationID: op.ID, ApplicationCode: op.ApplicationCode, Environment: op.EnvironmentCode,
		Generation: 1, ManifestChecksum: "manifest-1"}
	root := t.TempDir()
	value := application.SubsystemAccessDeploymentOutcome{Receipt: receipt,
		State: application.AccessDeploymentRejected, Code: application.AccessCredentialRetryRequired,
		RecordedAt: time.Now().UTC()}
	if err := writeAccessDeploymentOutcome(root, value); err != nil {
		t.Fatal(err)
	}
	state := &rejectionRecoveryState{recoveryReceiptState: recoveryReceiptState{
		accessLifecycleStub: accessLifecycleStub{phase: "DISPATCHED"}, receipt: receipt}}
	agent := &rejectionRecoveryAgent{recoveryReceiptAgent: recoveryReceiptAgent{root: root}}
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, agent,
		accessCatalogWithTarget(), relatedClientsStub{}, state, issuerStub{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), op, nil)
	if err != nil || outcome.Status != application.SubsystemAccessStatusFailed || !state.rejected {
		t.Fatalf("known rejection stayed unknown: %+v rejected=%v err=%v", outcome, state.rejected, err)
	}
	if state.completed || agent.provisionCalls != 0 || outcome.ErrorCode != application.AccessCredentialRetryRequired {
		t.Fatalf("rejection falsely completed or replayed: %+v", outcome)
	}
}

// TestAccessRecoveryWithUnavailableOutcome 保留旧Agent成功证明并保护真正未知结果。
// 参数：t为真实临时回执文件及显式旧端错误替身的测试上下文。
// 返回值：无，有可信旧回执可恢复；无证明始终WAITING且不得重放。
func TestAccessRecoveryWithUnavailableOutcome(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "legacy_success"}[success], func(t *testing.T) {
			op := onboardingOperation()
			receipt := application.SubsystemAccessDeploymentReceipt{TenantID: op.TenantID, OperationID: op.ID,
				ApplicationCode: op.ApplicationCode, Environment: op.EnvironmentCode, Generation: 1, ManifestChecksum: "manifest-1"}
			root := t.TempDir()
			if success {
				if err := writeAccessDeploymentReceipt(root, application.SubsystemProvisioningInput{
					TenantID: receipt.TenantID, ApplicationCode: receipt.ApplicationCode, Environment: receipt.Environment,
					ManifestChecksum: receipt.ManifestChecksum, AccessReceipt: &receipt}); err != nil {
					t.Fatal(err)
				}
			}
			state := &rejectionRecoveryState{recoveryReceiptState: recoveryReceiptState{accessLifecycleStub: accessLifecycleStub{phase: "DISPATCHED"}, receipt: receipt}}
			agent := &rejectionRecoveryAgent{recoveryReceiptAgent: recoveryReceiptAgent{root: root}, outcomeErr: errors.New("old agent does not implement outcome action")}
			adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, agent, accessCatalogWithTarget(), relatedClientsStub{}, state, issuerStub{}, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), op, nil)
			expected := application.SubsystemAccessStatusWaiting
			if success {
				expected = application.SubsystemAccessStatusSucceeded
			}
			if err != nil || outcome.Status != expected || agent.provisionCalls != 0 || state.rejected {
				t.Fatalf("legacy outcome recovery: %+v %v", outcome, err)
			}
		})
	}
}
