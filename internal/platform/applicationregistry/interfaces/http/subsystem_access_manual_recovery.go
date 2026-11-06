package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

type subsystemAccessRecoveryService interface {
	PreviewManualRecovery(context.Context, string, string) (application.SubsystemAccessManualRecoveryPreview, error)
	AbandonManualRecovery(context.Context, application.SubsystemAccessManualRecoveryInput) (application.SubsystemAccessOperationView, error)
}

// manualRecoveryAccess 在读取事实前要求既有完整更新权限，不信任页面权限或身份。
// 参数：writer用于结构化拒绝，request承载受信认证主体。
// 返回值：用例端口、主体与是否允许；失败已写响应。
func (handler *SubsystemAccessHandler) manualRecoveryAccess(writer stdhttp.ResponseWriter,
	request *stdhttp.Request) (subsystemAccessRecoveryService, authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return nil, principal, false
	}
	if !principalHasSubsystemAccessPermissions(principal,
		application.SubsystemAccessRequiredPermissions(application.SubsystemAccessActionUpdate)) {
		httpresponse.WriteError(writer, request, stdhttp.StatusForbidden, httperror.Forbidden)
		return nil, principal, false
	}
	service, ok := handler.service.(subsystemAccessRecoveryService)
	if !ok {
		httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.DependencyUnavailable)
		return nil, principal, false
	}
	return service, principal, true
}

// PreviewManualRecovery 返回已授权原任务的真实核对信息，不重新派发部署。
// 参数：writer承载响应，request提供可信租户及URL中的原任务ID。
// 返回值：无，预检失败保持原未知状态并返回结构化错误。
func (handler *SubsystemAccessHandler) PreviewManualRecovery(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	service, principal, ok := handler.manualRecoveryAccess(writer, request)
	if !ok {
		return
	}
	value, err := service.PreviewManualRecovery(request.Context(), principal.Tenant.ID, request.PathValue("operation_id"))
	if err != nil {
		handler.writeManualRecoveryError(writer, request, err)
		return
	}
	// 旧回执使用Go默认字段名且参与持久文件哈希，不能为前端改动其序列化。
	// 此处显式映射公共DTO，保持旧代次回执路径和恢复绑定不变。
	message := "人工恢复现场核验完成，尚未终止原代次"
	if value.Mode == application.SubsystemAccessRecoveryModeSuperseded {
		// 审核基准已变更：只确认否定证明，不提供（也不允许）运行事实核对。
		message = "原代次的审核基准已被当前发布取代；仅确认既无成功回执也无终止结果，尚未终止原代次"
	}
	// 取代模式没有运行事实：显式下发空数组，避免前端把 null 当成"字段缺失"。
	files := value.Evidence.RuntimeFiles
	if files == nil {
		files = []application.SubsystemAccessRecoveryFile{}
	}
	containers := value.Evidence.Containers
	if containers == nil {
		containers = []application.SubsystemAccessRecoveryContainer{}
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, message, map[string]any{
		"operation_id": value.OperationID, "operation_version": value.OperationVersion,
		"application_code": value.Evidence.Receipt.ApplicationCode, "environment": value.Evidence.Receipt.Environment,
		"generation": value.Evidence.Receipt.Generation, "manifest_checksum": value.Evidence.Receipt.ManifestChecksum,
		"evidence_digest": value.Evidence.EvidenceDigest, "runtime_files": files,
		"containers": containers, "already_fenced": value.Evidence.AlreadyFenced,
		"error_code": value.ErrorCode, "error_message": value.ErrorMessage,
		"mode": value.Mode, "receipt_manifest_checksum": value.ReceiptManifestChecksum,
		"current_profile_checksum": value.CurrentProfileChecksum,
	})
}

type manualRecoveryRequest struct {
	ExpectedVersion uint64 `json:"expected_version"`
	Generation      uint64 `json:"generation"`
	EvidenceDigest  string `json:"evidence_digest"`
	// Mode 为空时按 fenced 处理，保持既有请求体兼容。
	Mode                    string `json:"mode"`
	ReceiptManifestChecksum string `json:"receipt_manifest_checksum"`
	CurrentProfileChecksum  string `json:"current_profile_checksum"`
	Reason                  string `json:"reason"`
}

// decodeManualRecovery 严格限制终止请求的字段、大小及尾随JSON。
// 参数：writer用于拒绝响应，request为原始请求，target接收白名单字段。
// 返回值：成功为true，错误时不调用恢复用例。
func decodeManualRecovery(writer stdhttp.ResponseWriter, request *stdhttp.Request, target *manualRecoveryRequest) bool {
	decoder := json.NewDecoder(stdhttp.MaxBytesReader(writer, request.Body, maxSubsystemAccessRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
		return false
	}
	return true
}

// AbandonManualRecovery 以可信主体提交人工决定，成功也只表示原代次失败收口。
// 参数：writer承载响应，request含版本、代次、预检摘要与人工原因。
// 返回值：无，不接受客户端提供操作者、租户或成功状态。
func (handler *SubsystemAccessHandler) AbandonManualRecovery(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	service, principal, ok := handler.manualRecoveryAccess(writer, request)
	if !ok {
		return
	}
	var body manualRecoveryRequest
	if !decodeManualRecovery(writer, request, &body) {
		return
	}
	value, err := service.AbandonManualRecovery(request.Context(), application.SubsystemAccessManualRecoveryInput{
		TenantID: principal.Tenant.ID, OperatorID: principal.User.ID, OperationID: request.PathValue("operation_id"),
		ExpectedVersion: body.ExpectedVersion, Generation: body.Generation, EvidenceDigest: body.EvidenceDigest,
		Mode: body.Mode, ReceiptManifestChecksum: body.ReceiptManifestChecksum,
		CurrentProfileChecksum: body.CurrentProfileChecksum, Reason: body.Reason})
	if err != nil {
		handler.writeManualRecoveryError(writer, request, err)
		return
	}
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "人工决定已保存，原代次已失败收口；尚未重试部署", subsystemAccessViewResponse(value))
}

// writeManualRecoveryError 保持冲突、未接线与依赖失败的可理解语义。
// 参数：writer和request提供响应上下文，err为用例错误。
// 返回值：无，底层故障记录追踪但不输出运行配置。
func (handler *SubsystemAccessHandler) writeManualRecoveryError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error) {
	if errors.Is(err, application.ErrSubsystemAccessStageNotWired) || errors.Is(err, application.ErrSubsystemProvisioningUnavailable) {
		handler.logger.Warn("subsystem manual recovery unavailable", "error", err, "path", request.URL.Path)
		httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable,
			httperror.New("SUBSYSTEM_ACCESS_RECOVERY_UNAVAILABLE", "当前无法安全核对或终止原代次", map[string]any{
				"next_action": "核对部署Agent版本、签名和运行状态后，重新查询原操作；不要重复部署"}))
		return
	}
	if errors.Is(err, application.ErrValidation) {
		err = application.ErrSubsystemAccessInvalidRequest
	}
	handler.writeError(writer, request, err)
}
