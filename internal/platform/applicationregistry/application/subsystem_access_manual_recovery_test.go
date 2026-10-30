package application

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type manualRecoveryPortStub struct {
	repo  *accessRepositoryStub
	calls int
}

// PreviewSubsystemAccessRecovery 返回已限定原代次的非秘密事实替身。
// 参数：operation为应用服务已解析的同租户任务。
// 返回值：本测试使用第五代运行事实，不执行真实Agent操作。
func (p *manualRecoveryPortStub) PreviewSubsystemAccessRecovery(_ context.Context,
	operation SubsystemAccessOperation) (SubsystemAccessRecoveryEvidence, error) {
	p.calls++
	return SubsystemAccessRecoveryEvidence{Receipt: SubsystemAccessDeploymentReceipt{
		TenantID: operation.TenantID, OperationID: operation.ID,
		ApplicationCode: operation.ApplicationCode, Environment: operation.EnvironmentCode,
		Generation: 5, ManifestChecksum: "manifest-1"}, EvidenceDigest: strings.Repeat("a", 64)}, nil
}

// AbandonSubsystemAccess 为应用服务测试模拟已验证后的事务失败收口。
// 参数：operation为原任务，input为已校验的人工决定。
// 返回值：无，真实代次及事务保护由独立MySQL测试覆盖。
func (p *manualRecoveryPortStub) AbandonSubsystemAccess(_ context.Context,
	operation SubsystemAccessOperation, _ SubsystemAccessManualRecoveryInput) error {
	p.calls++
	operation.Status, operation.ErrorCode = SubsystemAccessStatusFailed, AccessOperatorAbandoned
	p.repo.operations[operation.ID] = operation
	return nil
}

// TestManualRecoveryApplicationChecksBeforeAgent 验证过期版本、跨租户及活动任务不会触达Agent。
// 参数：t为应用服务及无外部副作用的仓储替身上下文。
// 返回值：无，未核对的人工请求必须失败关闭。
func TestManualRecoveryApplicationChecksBeforeAgent(t *testing.T) {
	op := SubsystemAccessOperation{ID: "operation-1", TenantID: "tenant-1",
		ApplicationCode: "contract_management", EnvironmentCode: "prod", Version: 9,
		Action: SubsystemAccessActionUpdate, Stage: SubsystemAccessStageDeploy,
		Status: SubsystemAccessStatusWaiting, ErrorCode: "DEPLOYMENT_EFFECT_UNKNOWN"}
	repo := &accessRepositoryStub{operations: map[string]SubsystemAccessOperation{op.ID: op}}
	service := accessServiceFor(t, &accessFactsStub{}, repo)
	port := &manualRecoveryPortStub{repo: repo}
	service.SetManualRecovery(port)
	input := SubsystemAccessManualRecoveryInput{TenantID: op.TenantID, OperationID: op.ID,
		OperatorID: "operator-1", ExpectedVersion: 8, Generation: 5,
		EvidenceDigest: strings.Repeat("a", 64), Reason: "verified original failure and current runtime facts"}
	if _, err := service.AbandonManualRecovery(context.Background(), input); !errors.Is(err, ErrSubsystemAccessOperationConflict) || port.calls != 0 {
		t.Fatal("stale operator decision reached agent", err)
	}
	input.ExpectedVersion, input.TenantID = 9, "other-tenant"
	if _, err := service.AbandonManualRecovery(context.Background(), input); !errors.Is(err, ErrSubsystemAccessOperationNotFound) || port.calls != 0 {
		t.Fatal("cross-tenant decision reached agent", err)
	}
	input.TenantID = op.TenantID
	active := op
	active.Status = SubsystemAccessStatusRunning
	repo.operations[op.ID] = active
	if _, err := service.AbandonManualRecovery(context.Background(), input); !errors.Is(err, ErrSubsystemAccessOperationConflict) || port.calls != 0 {
		t.Fatal("active operation reached agent", err)
	}
	repo.operations[op.ID] = op
	view, err := service.AbandonManualRecovery(context.Background(), input)
	if err != nil || view.Operation.Status != SubsystemAccessStatusFailed || port.calls != 1 {
		t.Fatalf("reviewed decision: %+v %v", view, err)
	}
}
