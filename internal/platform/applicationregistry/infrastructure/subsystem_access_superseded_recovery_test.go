package infrastructure

import (
	"errors"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const (
	supersededReceiptManifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	supersededCurrentManifest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// supersededRecoveryFixture 构造逐条核验所需的最小合法事实。
// 参数：无，固定租户、目标与第五代派发。
// 返回值：原任务、原代次、部署状态、Agent 否定证明与操作者输入。
func supersededRecoveryFixture() (application.SubsystemAccessOperation,
	subsystemAccessDeploymentAttempt, subsystemDeploymentStateModel,
	application.SubsystemAccessRecoveryEvidence, application.SubsystemAccessManualRecoveryInput) {
	operation := application.SubsystemAccessOperation{ID: "01SAOP000000000000000000051", TenantID: "tenant-1",
		ApplicationID: "app-1", EnvironmentID: "env-1", ApplicationCode: "contract_management",
		EnvironmentCode: "prod", Action: application.SubsystemAccessActionUpdate,
		Stage: application.SubsystemAccessStageDeploy, Status: application.SubsystemAccessStatusWaiting,
		ErrorCode: application.AccessDeploymentEffectUnknown, Version: 7}
	attempt := subsystemAccessDeploymentAttempt{TenantID: operation.TenantID, OperationID: operation.ID,
		ApplicationID: operation.ApplicationID, EnvironmentID: operation.EnvironmentID,
		Generation: 5, Phase: "DISPATCHED", ManifestChecksum: supersededReceiptManifest,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	state := subsystemDeploymentStateModel{TenantID: operation.TenantID, ApplicationID: operation.ApplicationID,
		EnvironmentID: operation.EnvironmentID, ApplicationCode: operation.ApplicationCode,
		Environment: operation.EnvironmentCode, Status: string(application.SubsystemDeploymentStatusUpdating),
		Generation: 5, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	proof := application.SubsystemAccessRecoveryEvidence{Receipt: accessAttemptReceipt(operation, attempt),
		CurrentProfileChecksum: supersededCurrentManifest}
	input := application.SubsystemAccessManualRecoveryInput{TenantID: operation.TenantID, OperationID: operation.ID,
		OperatorID: "operator-1", ExpectedVersion: operation.Version, Generation: 5,
		Mode:                    application.SubsystemAccessRecoveryModeSuperseded,
		ReceiptManifestChecksum: supersededReceiptManifest, CurrentProfileChecksum: supersededCurrentManifest,
		Reason: "已核对原代次无成功回执且无终止结果，确认由新发布取代"}
	return operation, attempt, state, proof, input
}

// TestValidateSupersededRecoveryEnforcesEveryPrecondition 逐条验证取代收口的十个前提。
// 参数：t为本地内存事实的测试上下文，不访问数据库或外部系统。
// 返回值：无，任一前提被破坏都必须失败关闭。
func TestValidateSupersededRecoveryEnforcesEveryPrecondition(t *testing.T) {
	cases := map[string]func(*application.SubsystemAccessOperation, *subsystemAccessDeploymentAttempt,
		*subsystemDeploymentStateModel, *application.SubsystemAccessRecoveryEvidence,
		*application.SubsystemAccessManualRecoveryInput){
		"1_status_not_waiting": func(op *application.SubsystemAccessOperation, _ *subsystemAccessDeploymentAttempt,
			_ *subsystemDeploymentStateModel, _ *application.SubsystemAccessRecoveryEvidence,
			_ *application.SubsystemAccessManualRecoveryInput) {
			op.Status = application.SubsystemAccessStatusRunning
		},
		"1_status_failed": func(op *application.SubsystemAccessOperation, _ *subsystemAccessDeploymentAttempt,
			_ *subsystemDeploymentStateModel, _ *application.SubsystemAccessRecoveryEvidence,
			_ *application.SubsystemAccessManualRecoveryInput) {
			op.Status = application.SubsystemAccessStatusFailed
		},
		"2_stage_not_deploy": func(op *application.SubsystemAccessOperation, _ *subsystemAccessDeploymentAttempt,
			_ *subsystemDeploymentStateModel, _ *application.SubsystemAccessRecoveryEvidence,
			_ *application.SubsystemAccessManualRecoveryInput) {
			op.Stage = application.SubsystemAccessStageReadiness
		},
		"3_error_code_changed": func(op *application.SubsystemAccessOperation, _ *subsystemAccessDeploymentAttempt,
			_ *subsystemDeploymentStateModel, _ *application.SubsystemAccessRecoveryEvidence,
			_ *application.SubsystemAccessManualRecoveryInput) {
			op.ErrorCode = "DEPLOYMENT_FAILED"
		},
		"4_active_lease": func(op *application.SubsystemAccessOperation, _ *subsystemAccessDeploymentAttempt,
			_ *subsystemDeploymentStateModel, _ *application.SubsystemAccessRecoveryEvidence,
			_ *application.SubsystemAccessManualRecoveryInput) {
			op.LeaseOwner = "worker-1"
		},
		"5_attempt_not_dispatched": func(_ *application.SubsystemAccessOperation,
			attempt *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			attempt.Phase = "PREPARED"
		},
		"6_attempt_target_drift": func(op *application.SubsystemAccessOperation,
			attempt *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			attempt.EnvironmentID = "env-2"
			_ = op
		},
		"6_state_target_drift": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, state *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			state.ApplicationID = "app-2"
		},
		"7_generation_drift": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, state *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			state.Generation = 6
		},
		"8_state_ready": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, state *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			state.Status = string(application.SubsystemDeploymentStatusReady)
		},
		"8_state_failed": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, state *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			state.Status = string(application.SubsystemDeploymentStatusFailed)
		},
		"9_checksum_equal": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			proof *application.SubsystemAccessRecoveryEvidence, input *application.SubsystemAccessManualRecoveryInput) {
			proof.CurrentProfileChecksum = supersededReceiptManifest
			input.CurrentProfileChecksum = supersededReceiptManifest
		},
		"9_operator_current_stale": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, input *application.SubsystemAccessManualRecoveryInput) {
			input.CurrentProfileChecksum = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
		},
		"9_operator_receipt_stale": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, input *application.SubsystemAccessManualRecoveryInput) {
			input.ReceiptManifestChecksum = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
		},
		"9_generation_mismatch": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			_ *application.SubsystemAccessRecoveryEvidence, input *application.SubsystemAccessManualRecoveryInput) {
			input.Generation = 4
		},
		"10_success_receipt": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			proof *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			proof.SuccessReceipt = true
		},
		"10_terminal_outcome": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			proof *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			proof.TerminalOutcome = true
		},
		"10_proof_target_drift": func(_ *application.SubsystemAccessOperation,
			_ *subsystemAccessDeploymentAttempt, _ *subsystemDeploymentStateModel,
			proof *application.SubsystemAccessRecoveryEvidence, _ *application.SubsystemAccessManualRecoveryInput) {
			proof.Receipt.OperationID = "01SAOP000000000000000000052"
		},
	}
	operation, attempt, state, proof, input := supersededRecoveryFixture()
	if err := validateSupersededRecovery(operation, attempt, state, proof, input); err != nil {
		t.Fatalf("baseline facts must satisfy every precondition: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			operation, attempt, state, proof, input := supersededRecoveryFixture()
			mutate(&operation, &attempt, &state, &proof, &input)
			if err := validateSupersededRecovery(operation, attempt, state, proof, input); !errors.Is(err, application.ErrSubsystemAccessOperationConflict) {
				t.Fatalf("precondition %s was not enforced: %v", name, err)
			}
		})
	}
}

// TestValidateSupersededRecoveryAcceptsProvisioningState 验证首次部署（PROVISIONING）同样适用。
// 参数：t为本地内存事实的测试上下文。
// 返回值：无，PROVISIONING 与 UPDATING 都是"仍在进行中"的合法代次状态。
func TestValidateSupersededRecoveryAcceptsProvisioningState(t *testing.T) {
	operation, attempt, state, proof, input := supersededRecoveryFixture()
	state.Status = string(application.SubsystemDeploymentStatusProvisioning)
	if err := validateSupersededRecovery(operation, attempt, state, proof, input); err != nil {
		t.Fatalf("provisioning generation was rejected: %v", err)
	}
}
