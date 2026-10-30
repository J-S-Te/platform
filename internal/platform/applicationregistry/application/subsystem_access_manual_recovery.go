package application

import (
	"context"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

type SubsystemAccessManualRecoveryInput struct {
	TenantID        string
	OperationID     string
	OperatorID      string
	ExpectedVersion uint64
	Generation      uint64
	EvidenceDigest  string
	Reason          string
}

type SubsystemAccessManualRecoveryPreview struct {
	OperationID      string                          `json:"operation_id"`
	OperationVersion uint64                          `json:"operation_version"`
	ErrorCode        string                          `json:"error_code"`
	ErrorMessage     string                          `json:"error_message"`
	Evidence         SubsystemAccessRecoveryEvidence `json:"evidence"`
}

type SubsystemAccessManualRecoveryPort interface {
	PreviewSubsystemAccessRecovery(context.Context, SubsystemAccessOperation) (SubsystemAccessRecoveryEvidence, error)
	AbandonSubsystemAccess(context.Context, SubsystemAccessOperation, SubsystemAccessManualRecoveryInput) error
}

// SetManualRecovery 接入有权限和代次保护的人工恢复基础设施。
// 参数：port为Agent事实核验及平台事务端口，空值保持能力不可用。
// 返回值：无，不改变既有普通恢复行为。
func (service *SubsystemAccessService) SetManualRecovery(port SubsystemAccessManualRecoveryPort) {
	service.manualRecovery = port
}

// validRecoveryDigest 严格限定人工核对事实的规范SHA256摘要。
// 参数：value为预检返回并随提交带回的摘要。
// 返回值：仅64位小写十六进制返回true。
func validRecoveryDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// manualRecoveryOperation 只允许同租户、无活动租约的旧未知派发进入人工核对。
// 参数：ctx为请求上下文，tenantID及operationID限定可信租户任务。
// 返回值：返回原任务；不符合狭窄状态或能力未接入时返回错误。
func (service *SubsystemAccessService) manualRecoveryOperation(ctx context.Context,
	tenantID, operationID string) (SubsystemAccessOperation, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(operationID) == "" {
		return SubsystemAccessOperation{}, ErrSubsystemAccessInvalidRequest
	}
	operation, err := service.repository.GetOperation(ctx, strings.TrimSpace(tenantID), strings.TrimSpace(operationID))
	if err != nil {
		return operation, err
	}
	if operation.Status != SubsystemAccessStatusWaiting || operation.Stage != SubsystemAccessStageDeploy ||
		operation.ErrorCode != "DEPLOYMENT_EFFECT_UNKNOWN" || operation.LeaseOwner != "" {
		return operation, ErrSubsystemAccessOperationConflict
	}
	if service.manualRecovery == nil {
		return operation, ErrSubsystemAccessStageNotWired
	}
	return operation, nil
}

// PreviewManualRecovery 返回真实运行事实及当前任务版本，不终止或重新部署。
// 参数：ctx为请求上下文，tenantID及operationID为已授权租户内的原任务。
// 返回值：可审查的原代次事实；跨目标、状态漂移或不可观察时拒绝。
func (service *SubsystemAccessService) PreviewManualRecovery(ctx context.Context,
	tenantID, operationID string) (SubsystemAccessManualRecoveryPreview, error) {
	var result SubsystemAccessManualRecoveryPreview
	operation, err := service.manualRecoveryOperation(ctx, tenantID, operationID)
	if err != nil {
		return result, err
	}
	evidence, err := service.manualRecovery.PreviewSubsystemAccessRecovery(ctx, operation)
	if err != nil {
		return result, err
	}
	receipt := evidence.Receipt
	if receipt.TenantID != operation.TenantID || receipt.OperationID != operation.ID ||
		receipt.ApplicationCode != operation.ApplicationCode || receipt.Environment != operation.EnvironmentCode ||
		receipt.Generation == 0 || receipt.ManifestChecksum == "" || !validRecoveryDigest(evidence.EvidenceDigest) {
		return result, ErrSubsystemAccessOperationConflict
	}
	return SubsystemAccessManualRecoveryPreview{OperationID: operation.ID, OperationVersion: operation.Version,
		ErrorCode: operation.ErrorCode, ErrorMessage: operation.ErrorMessage, Evidence: evidence}, nil
}

// AbandonManualRecovery 在显式核对后经Agent栅栏与平台CAS终止原未知代次。
// 参数：input含可信租户/操作者、原任务版本/代次、预检摘要及人工原因。
// 返回值：失败收口后的真实视图；任何冲突均不授权自动重放。
func (service *SubsystemAccessService) AbandonManualRecovery(ctx context.Context,
	input SubsystemAccessManualRecoveryInput) (SubsystemAccessOperationView, error) {
	input.OperatorID, input.Reason = strings.TrimSpace(input.OperatorID), strings.TrimSpace(input.Reason)
	if input.OperatorID == "" || len(input.OperatorID) > 64 || input.ExpectedVersion == 0 || input.Generation == 0 ||
		!validRecoveryDigest(input.EvidenceDigest) || !utf8.ValidString(input.Reason) ||
		utf8.RuneCountInString(input.Reason) < 10 || utf8.RuneCountInString(input.Reason) > 500 {
		return SubsystemAccessOperationView{}, ErrSubsystemAccessInvalidRequest
	}
	operation, err := service.manualRecoveryOperation(ctx, input.TenantID, input.OperationID)
	if err != nil {
		return SubsystemAccessOperationView{}, err
	}
	if operation.Version != input.ExpectedVersion {
		return SubsystemAccessOperationView{}, ErrSubsystemAccessOperationConflict
	}
	if err = service.manualRecovery.AbandonSubsystemAccess(ctx, operation, input); err != nil {
		return SubsystemAccessOperationView{}, err
	}
	return service.Describe(ctx, operation.TenantID, operation.ID)
}
