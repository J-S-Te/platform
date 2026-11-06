package application

import (
	"context"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// 人工恢复的两种模式：
//   - fenced：原回执清单仍是当前审核清单（清单未漂移），Agent 可全面核对运行事实后写入终止栅栏；
//   - superseded：原回执清单已被新的发布取代，Agent 只能只读地证明"既无成功回执也无终止结果"，
//     平台据此失败收口。两种模式都不声明部署成功，也都不自动回滚。
const (
	// SubsystemAccessRecoveryModeFenced 表示原代次未被取代的原有终止路径。
	SubsystemAccessRecoveryModeFenced = "fenced"
	// SubsystemAccessRecoveryModeSuperseded 表示原代次已被新审核清单取代的收口路径。
	SubsystemAccessRecoveryModeSuperseded = "superseded"
)

type SubsystemAccessManualRecoveryInput struct {
	TenantID        string
	OperationID     string
	OperatorID      string
	ExpectedVersion uint64
	Generation      uint64
	EvidenceDigest  string
	// Mode 为空时按 fenced 处理，保持既有客户端与旧请求体兼容。
	Mode string
	// ReceiptManifestChecksum / CurrentProfileChecksum 只在 superseded 模式必需：
	// 操作者必须携带预检返回的原代次清单摘要与当前清单摘要，平台据此证明原代次确实
	// 已被取代，并拒绝清单再次漂移后的盲目收口。
	ReceiptManifestChecksum string
	CurrentProfileChecksum  string
	Reason                  string
}

type SubsystemAccessManualRecoveryPreview struct {
	OperationID      string                          `json:"operation_id"`
	OperationVersion uint64                          `json:"operation_version"`
	ErrorCode        string                          `json:"error_code"`
	ErrorMessage     string                          `json:"error_message"`
	Mode             string                          `json:"mode"`
	Evidence         SubsystemAccessRecoveryEvidence `json:"evidence"`
	// ReceiptManifestChecksum 是原代次派发时的审核清单摘要；CurrentProfileChecksum 是
	// Agent 当前清单摘要。两者不同即"审核基准已变更"，只能走 superseded 收口。
	ReceiptManifestChecksum string `json:"receipt_manifest_checksum"`
	CurrentProfileChecksum  string `json:"current_profile_checksum"`
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

// ValidSubsystemAccessChecksum 判断清单摘要是否是可回传比对的受控格式。
// 参数：value为预检返回或操作者携带的清单摘要。
// 返回值：非空、无内嵌空白且不超过既有列宽时返回true；不猜测具体摘要算法前缀。
func ValidSubsystemAccessChecksum(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, " \t\r\n")
}

// subsystemAccessRecoveryMode 归一化人工恢复模式；未知取值不得被当成默认值。
// 参数：value为请求体中的模式（空表示沿用既有 fenced 语义）。
// 返回值：合法模式或 ErrSubsystemAccessInvalidRequest。
func subsystemAccessRecoveryMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", SubsystemAccessRecoveryModeFenced:
		return SubsystemAccessRecoveryModeFenced, nil
	case SubsystemAccessRecoveryModeSuperseded:
		return SubsystemAccessRecoveryModeSuperseded, nil
	default:
		return "", ErrSubsystemAccessInvalidRequest
	}
}

// reviewManualRecoveryEvidence 核对预检事实与已授权原任务绑定，并判定恢复模式。
// 参数：operation为同租户原任务，evidence为Agent返回的原代次事实。
// 返回值：fenced 或 superseded；跨目标、成功/终止事实或运行事实与模式不符时失败关闭。
func reviewManualRecoveryEvidence(operation SubsystemAccessOperation,
	evidence SubsystemAccessRecoveryEvidence) (string, error) {
	receipt := evidence.Receipt
	if receipt.TenantID != operation.TenantID || receipt.OperationID != operation.ID ||
		receipt.ApplicationCode != operation.ApplicationCode || receipt.Environment != operation.EnvironmentCode ||
		receipt.Generation == 0 || !ValidSubsystemAccessChecksum(receipt.ManifestChecksum) {
		return "", ErrSubsystemAccessOperationConflict
	}
	// 真实成功或终止事实不能被人工终止覆盖：交给正常恢复路径读取，不在此处收口。
	if evidence.SuccessReceipt || evidence.TerminalOutcome {
		return "", ErrSubsystemAccessOperationConflict
	}
	current := strings.TrimSpace(evidence.CurrentProfileChecksum)
	if !ValidSubsystemAccessChecksum(current) {
		return "", ErrSubsystemAccessOperationConflict
	}
	if current == receipt.ManifestChecksum {
		// 清单未漂移：沿用原有 fenced 路径，必须同时给出运行事实与摘要。
		if !validRecoveryDigest(evidence.EvidenceDigest) || len(evidence.RuntimeFiles) == 0 || len(evidence.Containers) == 0 {
			return "", ErrSubsystemAccessOperationConflict
		}
		return SubsystemAccessRecoveryModeFenced, nil
	}
	// 审核基准已变更：只接受否定证明，不允许把当前运行事实冒充成原代次的核对事实。
	if len(evidence.RuntimeFiles) != 0 || len(evidence.Containers) != 0 || evidence.EvidenceDigest != "" {
		return "", ErrSubsystemAccessOperationConflict
	}
	return SubsystemAccessRecoveryModeSuperseded, nil
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
		operation.ErrorCode != AccessDeploymentEffectUnknown || operation.LeaseOwner != "" {
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
	mode, err := reviewManualRecoveryEvidence(operation, evidence)
	if err != nil {
		return result, err
	}
	return SubsystemAccessManualRecoveryPreview{OperationID: operation.ID, OperationVersion: operation.Version,
		ErrorCode: operation.ErrorCode, ErrorMessage: operation.ErrorMessage, Mode: mode, Evidence: evidence,
		ReceiptManifestChecksum: evidence.Receipt.ManifestChecksum,
		CurrentProfileChecksum:  evidence.CurrentProfileChecksum}, nil
}

// AbandonManualRecovery 在显式核对后经Agent栅栏与平台CAS终止原未知代次。
// 参数：input含可信租户/操作者、原任务版本/代次、恢复模式、预检摘要及人工原因。
// 返回值：失败收口后的真实视图；任何冲突均不授权自动重放。
func (service *SubsystemAccessService) AbandonManualRecovery(ctx context.Context,
	input SubsystemAccessManualRecoveryInput) (SubsystemAccessOperationView, error) {
	input.OperatorID, input.Reason = strings.TrimSpace(input.OperatorID), strings.TrimSpace(input.Reason)
	mode, err := subsystemAccessRecoveryMode(input.Mode)
	if err != nil {
		return SubsystemAccessOperationView{}, err
	}
	input.Mode, input.ReceiptManifestChecksum, input.CurrentProfileChecksum =
		mode, strings.TrimSpace(input.ReceiptManifestChecksum), strings.TrimSpace(input.CurrentProfileChecksum)
	if input.OperatorID == "" || len(input.OperatorID) > 64 || input.ExpectedVersion == 0 || input.Generation == 0 ||
		!utf8.ValidString(input.Reason) || utf8.RuneCountInString(input.Reason) < 10 ||
		utf8.RuneCountInString(input.Reason) > 500 {
		return SubsystemAccessOperationView{}, ErrSubsystemAccessInvalidRequest
	}
	if mode == SubsystemAccessRecoveryModeSuperseded {
		// 取代路径不接受运行事实摘要，只接受两个清单摘要的显式比对。
		if !ValidSubsystemAccessChecksum(input.ReceiptManifestChecksum) ||
			!ValidSubsystemAccessChecksum(input.CurrentProfileChecksum) ||
			input.ReceiptManifestChecksum == input.CurrentProfileChecksum {
			return SubsystemAccessOperationView{}, ErrSubsystemAccessOperationConflict
		}
	} else if !validRecoveryDigest(input.EvidenceDigest) {
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
