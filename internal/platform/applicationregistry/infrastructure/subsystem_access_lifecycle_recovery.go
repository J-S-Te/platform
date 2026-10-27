package infrastructure

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// accessDeploymentRecovery 用途：在准备凭据前判断是否应恢复收口或等待核对。
// 参数：ctx 为上下文，operation 为任务。
// 返回值：返回恢复结果及是否应停止派发；无生命周期端口沿用旧测试适配器行为。
func (adapter *SubsystemAccessDeploymentAdapter) accessDeploymentRecovery(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemAccessStageOutcome, bool, error) {
	lifecycle, ok := adapter.state.(SubsystemAccessDeploymentLifecycle)
	if !ok {
		return application.SubsystemAccessStageOutcome{}, false, nil
	}
	phase, err := lifecycle.PrepareAccessDeployment(ctx, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, true, err
	}
	if phase == "SUCCEEDED" || phase == "DISPATCHED" {
		receiptReader, ok := adapter.state.(interface {
			ReadAccessDeploymentReceipt(context.Context, application.SubsystemAccessOperation) (application.SubsystemAccessDeploymentReceipt, string, error)
		})
		if !ok {
			if phase == "SUCCEEDED" {
				return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded}, true, nil
			}
			return unknownAccessDeployment(), true, nil
		}
		receipt, candidate, err := receiptReader.ReadAccessDeploymentReceipt(ctx, operation)
		if err != nil {
			return application.SubsystemAccessStageOutcome{}, true, err
		}
		if phase == "DISPATCHED" {
			agent, ok := adapter.provisioner.(accessDeploymentReceiptReader)
			if !ok {
				return unknownAccessDeployment(), true, nil
			}
			confirmed, err := agent.ReadAccessDeploymentCompletion(ctx, receipt)
			if err != nil {
				return application.SubsystemAccessStageOutcome{}, true, err
			}
			if !confirmed {
				return unknownAccessDeployment(), true, nil
			}
			if err := adapter.confirmRecoveredAccessLicense(ctx, operation); err != nil {
				return application.SubsystemAccessStageOutcome{}, true, err
			}
			if err := lifecycle.CompleteAccessDeployment(ctx, operation, receipt.ManifestChecksum); err != nil {
				return application.SubsystemAccessStageOutcome{}, true, err
			}
		}
		evidence := "deployment:durable-completion"
		if candidate != "" {
			evidence = accessCandidatePrefix + candidate + ";" + evidence
		}
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded, EvidenceRef: evidence}, true, nil
	}
	return application.SubsystemAccessStageOutcome{}, false, nil
}

// unknownAccessDeployment 用途：表达尚无可信完成回执的派发结果。
// 参数：无。
// 返回值：返回等待人工核对的阶段结果，不授权重放。
func unknownAccessDeployment() application.SubsystemAccessStageOutcome {
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusWaiting, ErrorCode: "DEPLOYMENT_EFFECT_UNKNOWN", Message: "部署已派发但未取得持久完成回执；需核对实际运行结果后恢复，不能直接重复部署"}
}

// finishAccessFailure 用途：将确定失败同步收口到同一代次。
// 参数：ctx 为上下文，operation 为任务，outcome/err 为待返回阶段结果。
// 返回值：无；收口错误写回 err 防止掩盖失败。
func (adapter *SubsystemAccessDeploymentAdapter) finishAccessFailure(ctx context.Context, operation application.SubsystemAccessOperation, outcome *application.SubsystemAccessStageOutcome, resultErr *error) {
	if *resultErr != nil || outcome.Status != application.SubsystemAccessStatusFailed {
		return
	}
	if store, ok := adapter.state.(interface {
		FailAccessDeployment(context.Context, application.SubsystemAccessOperation, string, string) error
	}); ok {
		*resultErr = store.FailAccessDeployment(ctx, operation, outcome.ErrorCode, outcome.Message)
	}
}

// confirmRecoveredAccessLicense 用途：恢复时保留本地自然确认门禁。
// 参数：ctx 为上下文，operation 为任务。
// 返回值：缺期望身份或消费者尚未确认时返回错误。
func (adapter *SubsystemAccessDeploymentAdapter) confirmRecoveredAccessLicense(ctx context.Context, operation application.SubsystemAccessOperation) error {
	if !adapter.runtimeLocal || adapter.runtimeLicenses == nil {
		return nil
	}
	reader, ok := adapter.state.(interface {
		ReadAccessRuntimeExpectations(context.Context, application.SubsystemAccessOperation) ([]application.RuntimeLicenseCredential, error)
	})
	if !ok {
		return application.ErrValidation
	}
	credentials, err := reader.ReadAccessRuntimeExpectations(ctx, operation)
	if err != nil {
		return err
	}
	if len(credentials) == 0 {
		return nil
	}
	return adapter.runtimeLicenses.WaitForRuntimeConfirmation(ctx, operation.ApplicationCode, operation.EnvironmentCode, credentials)
}
