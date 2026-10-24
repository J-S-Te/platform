package http

func runtimeLicenseApprovalDiagnostic(code string) (message, nextAction string) {
	switch code {
	case "LICENSE_RUNTIME_STARTUP_FAILED":
		return "已批准运行组件未通过启动稳定性校验", "请检查目标 API 和 Worker 的运行状态、重启及健康检查结果；修复后在当前环境点击“重试”，不要重复新增接入"
	case "LICENSE_RUNTIME_DOCKER_UNAVAILABLE":
		return "Docker 镜像校验服务不可用，暂时无法核验运行批准", "请检查 Docker 服务、docker-socket-proxy 和部署 Agent 的连接状态；恢复后在当前环境点击“重试”，不要重复新增接入"
	case "LICENSE_RUNTIME_APPROVAL_UNAVAILABLE":
		return "当前环境的已审核运行批准缺失或不可读取", "请确认部署 Agent 可用，且当前应用与环境的已审核运行批准已提供并可读取；修复后在当前环境点击“重试”"
	case "LICENSE_RUNTIME_APPROVAL_MISMATCH":
		return "当前发布与已审核运行批准不匹配，已停止组件登记", "请核对当前源码、编排配置、镜像及组件清单与已审核运行批准是否一致；完成既有审核流程后在当前环境点击“重试”"
	case "LICENSE_RUNTIME_NOT_READY":
		return "执行组件尚未完成登记、覆盖核验或当前修订确认", "请确认已导入有效许可证，并等待组件完成登记与当前修订确认；确认完成后在当前环境点击“重试”，不要重复新增接入"
	case "LICENSE_RUNTIME_ENROLLMENT_FAILED":
		return "运行组件登记失败，当前实例未完成商业授权登记", "请确认许可证已激活且实例标识一致，随后在当前环境点击“重试”；仍失败时导出部署状态交由授权管理员核对"
	case "LICENSE_RUNTIME_CONFIRMATION_FAILED":
		return "运行组件当前修订确认失败", "请核对当前部署修订与许可证覆盖范围是否一致，完成确认流程后在当前环境点击“重试”"
	case "LICENSE_RUNTIME_BINDING_FAILED":
		return "许可证绑定校验失败，运行组件无法确认授权归属", "请确认当前应用与环境已绑定到有效许可证，绑定修复后点击“重试”，不要重复新增接入"
	case "LICENSE_NOT_INITIALIZED":
		return "商业运行实例尚未初始化（缺少有效许可证）", "请先由授权管理员导入厂商签发的许可证并完成初始化，再回到当前环境重试接入"
	case "LICENSE_CONFIRMATION_REQUIRED":
		return "许可证变更需要显式确认", "请在许可证页面核对变更内容并完成显式确认后重试；未确认前不会生效"
	case "LICENSE_BINDING_MISMATCH":
		return "许可证绑定信息与当前运行实例不匹配", "请核对许可证的客户、实例与环境信息是否与本机一致，由授权管理员更正后重试"
	case "LICENSE_EVIDENCE_UNAVAILABLE":
		return "商业授权运行证据不可读取", "请确认部署 Agent 可用且运行证据文件完整可读，恢复后点击“重试”"
	case "LICENSE_CLOCK_RECOVERY_REQUIRED":
		return "检测到系统时钟回拨，许可证有效期无法核验", "请先校正服务器时间并完成时钟恢复流程，再重试接入与授权核验"
	default:
		return "", ""
	}
}
