package application

import "errors"

// 运行时许可诊断哨兵：错误文本内嵌 LICENSE_RUNTIME_* 稳定诊断码，
// 供 RuntimeLicenseApprovalErrorCode 映射为对外诊断；哨兵本身不得携带
// 底层命令、凭据或主机路径，防止经审批边界外泄宿主信息。
var (
	// ErrRuntimeLicenseDockerUnavailable 表示运行时镜像校验服务不可用，
	// 属环境故障而非审批结论，可重试但不得降级为跳过校验。
	ErrRuntimeLicenseDockerUnavailable = errors.New("LICENSE_RUNTIME_DOCKER_UNAVAILABLE: runtime image verification service unavailable")
	// ErrRuntimeLicenseStartupFailed 表示已批准组件启动校验失败：
	// 镜像与审批一致但运行行为不符，必须阻断并要求人工介入。
	ErrRuntimeLicenseStartupFailed = errors.New("LICENSE_RUNTIME_STARTUP_FAILED: approved runtime components failed startup verification")
	// ErrRuntimeLicenseApprovalUnavailable 表示找不到已评审的发行审批，
	// 通常是审批尚未完成或目录版本漂移。
	ErrRuntimeLicenseApprovalUnavailable = errors.New("LICENSE_RUNTIME_APPROVAL_UNAVAILABLE: reviewed runtime release approval unavailable")
	// ErrRuntimeLicenseApprovalMismatch 表示运行时发行与已评审审批不一致，
	// 属于完整性破坏信号，不允许仅凭重启恢复。
	ErrRuntimeLicenseApprovalMismatch = errors.New("LICENSE_RUNTIME_APPROVAL_MISMATCH: runtime release does not match reviewed approval")
)

// RuntimeLicenseApprovalFailure 把任意错误归约为已知诊断哨兵后原样返回：
// 未命中白名单的一律折叠为 ErrRuntimeLicenseApprovalUnavailable。
// 该函数是审批边界：底层命令、凭据、主机路径绝不可随错误穿越到调用方。
func RuntimeLicenseApprovalFailure(err error) error {
	for _, known := range []error{ErrRuntimeLicenseDockerUnavailable, ErrRuntimeLicenseApprovalMismatch, ErrRuntimeLicenseApprovalUnavailable, ErrRuntimeLicenseStartupFailed, ErrRuntimeLicenseNotInitialized} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrRuntimeLicenseApprovalUnavailable
}

// RuntimeLicenseApprovalErrorCode 把许可诊断哨兵映射为对外稳定诊断码
// （LICENSE_RUNTIME_* 等）；未识别错误返回空串，由调用方回退到通用文案，
// 避免把内部错误细节直接暴露给前端。
func RuntimeLicenseApprovalErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrRuntimeLicenseStartupFailed):
		return "LICENSE_RUNTIME_STARTUP_FAILED"
	case errors.Is(err, ErrRuntimeLicenseDockerUnavailable):
		return "LICENSE_RUNTIME_DOCKER_UNAVAILABLE"
	case errors.Is(err, ErrRuntimeLicenseApprovalMismatch):
		return "LICENSE_RUNTIME_APPROVAL_MISMATCH"
	case errors.Is(err, ErrRuntimeLicenseApprovalUnavailable):
		return "LICENSE_RUNTIME_APPROVAL_UNAVAILABLE"
	// 商业运行实例尚未初始化：已批准发行拿不到组件凭据时的根因。必须映射到
	// 用户可读诊断，而不是落到与授权无关的通用“部署 Agent 执行失败”。
	case errors.Is(err, ErrRuntimeLicenseNotInitialized):
		return "LICENSE_NOT_INITIALIZED"
	default:
		return ""
	}
}
