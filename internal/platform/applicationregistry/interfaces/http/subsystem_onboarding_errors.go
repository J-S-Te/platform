package http

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	stdhttp "net/http"
	"strings"
	"unicode/utf8"
)

// writeError 输出接入错误及安全的受控恢复指引。
// 参数：writer和request为HTTP上下文，err为原因，provisioningStages为失败阶段。
// 返回值：无；通过HTTP输出错误。
func (handler *SubsystemOnboardingHandler) writeError(writer stdhttp.ResponseWriter, request *stdhttp.Request, err error, provisioningStages ...string) {
	var onboardingConflict *application.SubsystemOnboardingConflict
	switch {
	case errors.As(err, &onboardingConflict):
		handler.logger.Warn("subsystem onboarding skipped because environment already exists",
			"path", request.URL.Path,
			"application_code", onboardingConflict.ApplicationCode,
			"environment", onboardingConflict.Environment,
			"environment_status", onboardingConflict.Status,
		)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
			"IAM_SUBSYSTEM_ALREADY_ONBOARDED",
			"该应用环境已存在；接入脚本不会覆盖已有登录目标或 OAuth 客户端",
			map[string]string{
				"application_code": onboardingConflict.ApplicationCode,
				"environment":      onboardingConflict.Environment,
				"status":           onboardingConflict.Status,
				"next_action":      "该环境的控制面记录已存在。子系统日常代码、镜像、功能模块和业务迁移发布无需执行接入或撤销脚本；仅基础设施入口参数变更时使用环境、登录目标或 OAuth 客户端的受控更新接口。若此前部署 Agent 失败，请先查询 subsystem-status，再使用 subsystem-retry，不要重复 onboard",
			},
		))
	case errors.Is(err, application.ErrSubsystemOnboardingAlreadyExists):
		// 预检在写控制面之前拦下"应用+环境已存在"时返回的是哨兵值，而不是
		// *SubsystemOnboardingConflict；这里补齐映射，避免落到通用 500。
		handler.logger.Warn("subsystem onboarding skipped because environment already exists",
			"path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
			"IAM_SUBSYSTEM_ALREADY_ONBOARDED",
			"该应用环境已存在；接入脚本不会覆盖已有登录目标或 OAuth 客户端",
			map[string]string{
				"next_action": "该环境的控制面记录已存在。若此前未完成部署，请使用 subsystem.sh update/retry 继续受控部署，不要重复 onboard",
			},
		))
	case errors.Is(err, errRelatedReimbursementCostDependency):
		handler.logger.Warn("subsystem source credential dependency unavailable",
			"path", request.URL.Path, "source_application", "settlement", "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
			"IAM_SUBSYSTEM_REIMBURSEMENT_COST_DEPENDENCY_REQUIRED",
			"报销成本候选入口的结算只读机器身份不可用",
			map[string]string{
				"source_application": "settlement",
				"next_action":        "请核对同租户同环境的结算系统，受控补齐独立费用候选读取身份后重试报销更新",
			}))
	case errors.Is(err, errRelatedSettlementReferenceDependency):
		handler.logger.Warn("subsystem source credential dependency unavailable",
			"path", request.URL.Path, "source_application", "settlement", "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
			"IAM_SUBSYSTEM_SETTLEMENT_REFERENCE_DEPENDENCY_REQUIRED",
			"项目接入需要已登记的结算项目引用只读机器身份",
			map[string]string{
				"source_application": "settlement",
				"next_action":        "请先在同一租户和环境接入或受控更新结算系统，补齐 settlement_project_reference_read 用途凭据，再重试项目接入或更新",
			}))
	case errors.Is(err, errRelatedContractDependency):
		handler.logger.Warn("subsystem source credential dependency unavailable", "path", request.URL.Path, "source_application", "contract_management", "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New("IAM_SUBSYSTEM_CONTRACT_DEPENDENCY_REQUIRED", "结算接入需要已登记的合同事件机器凭据", map[string]string{"source_application": "contract_management", "next_action": "请先在同一租户和环境接入或受控更新合同管理系统，补齐 settlement_contract_ingest 用途凭据，再重试结算接入或更新"}))
	case errors.Is(err, application.ErrValidation), errors.Is(err, application.ErrManagementValidation):
		httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.Validation)
	case errors.Is(err, application.ErrNotFound), errors.Is(err, application.ErrManagementNotFound):
		httpresponse.WriteError(writer, request, stdhttp.StatusNotFound, httperror.NotFound)
	case errors.Is(err, application.ErrConflict), errors.Is(err, application.ErrVersionConflict), errors.Is(err, application.ErrManagementConflict):
		httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.Conflict)
	case errors.Is(err, application.ErrSubsystemProvisioningUnavailable):
		handler.logger.Error("subsystem automatic provisioning failed", "path", request.URL.Path, "error", err)
		message := httperror.DependencyUnavailable.Message
		if strings.Contains(strings.ToLower(err.Error()), "disabled") {
			message = "当前部署未启用受控部署 Agent，无法在平台内完成一键接入"
		}
		// 安全（SEC-B3）：details 只暴露稳定错误码与本地化指引，Agent/DB 错误原文
		// （表名、SQL state、路径）只进上面的结构化日志，不再附在响应里。
		httpresponse.WriteError(writer, request, stdhttp.StatusServiceUnavailable, httperror.New(
			httperror.DependencyUnavailable.Code,
			message,
			map[string]string{
				"next_action": subsystemProvisioningNextAction(err, provisioningStages...),
				"error_code":  subsystemProvisioningErrorCode(err),
			},
		))
	default:
		handler.logger.Error("subsystem onboarding request failed", "path", request.URL.Path, "error", err)
		httpresponse.WriteError(writer, request, stdhttp.StatusInternalServerError, httperror.Internal)
	}
}

// subsystemProvisioningErrorCode 把 Agent/部署错误归类为稳定错误码（SEC-B3）。
// 分类键与 subsystemProvisioningNextAction 的诊断键保持一致；错误原文只在本函数
// 内部用于匹配，绝不离开函数。
func subsystemProvisioningErrorCode(err error) string {
	if err == nil {
		return "PROVISIONING_FAILED"
	}
	message := strings.ToLower(err.Error())
	switch {
	// 许可运行态类失败必须排在通用 "unavailable" 分支之前：这些错误文本同时含
	// unavailable，落入通用分支只会给出与许可无关的“依赖不可用”误导诊断。
	case errors.Is(err, application.ErrRuntimeLicenseStateUnavailable),
		strings.Contains(message, "license_runtime_state_unavailable"),
		strings.Contains(message, "license runtime state is unavailable"):
		return "LICENSE_RUNTIME_STATE_UNAVAILABLE"
	case strings.Contains(message, "commercial license configuration failed"),
		strings.Contains(message, "license configuration failed"):
		return "LICENSE_RUNTIME_CONFIGURATION_FAILED"
	case strings.Contains(message, "disabled"):
		return "PROVISIONING_AGENT_DISABLED"
	case strings.Contains(message, "deployment helper is unavailable"), strings.Contains(message, "read deployment response"), strings.Contains(message, "send deployment request"), strings.Contains(message, "docker service"):
		return "PROVISIONING_AGENT_UNREACHABLE"
	case strings.Contains(message, "target is not allowed"), strings.Contains(message, "tenant is not allowed"):
		return "PROVISIONING_TARGET_NOT_ALLOWED"
	case strings.Contains(message, "requires an approved manifest checksum"):
		return "PROVISIONING_MANIFEST_CHECKSUM_REQUIRED"
	case strings.Contains(message, "preflight values are inconsistent"), strings.Contains(message, "integration values are inconsistent"), strings.Contains(message, "deployment request is invalid"):
		return "PROVISIONING_REQUEST_MISMATCH"
	case strings.Contains(message, "secrets are incomplete"), strings.Contains(message, "credentials are incomplete"), strings.Contains(message, "credential is incomplete"), strings.Contains(message, "generated runtime secret is invalid"), strings.Contains(message, "contract_summary"):
		return "PROVISIONING_CREDENTIALS_INCOMPLETE"
	case strings.Contains(message, "immutable digest"):
		return "PROVISIONING_IMAGE_NOT_RELEASED"
	case strings.Contains(message, "production deployment lock is unavailable"):
		return "PROVISIONING_DEPLOYMENT_LOCK_BUSY"
	case strings.Contains(message, "migrate production subsystem database"):
		return "PROVISIONING_MIGRATION_FAILED"
	case strings.Contains(message, "backup production subsystem database"), strings.Contains(message, "prepare production subsystem backup"):
		return "PROVISIONING_BACKUP_FAILED"
	case strings.Contains(message, "license runtime state is unavailable"):
		return "LICENSE_RUNTIME_STATE_UNAVAILABLE"
	case strings.Contains(message, "commercial license configuration failed"):
		return "LICENSE_RUNTIME_CONFIGURATION_FAILED"
	case strings.Contains(message, "start production subsystem dependencies"), strings.Contains(message, "start production subsystem services"), strings.Contains(message, "start subsystem containers"), strings.Contains(message, "rebuild subsystem containers"), strings.Contains(message, "release environment is unavailable"), strings.Contains(message, "production environment is unavailable"):
		return "PROVISIONING_RUNTIME_UNHEALTHY"
	case strings.Contains(message, "unavailable"):
		return "PROVISIONING_DEPENDENCY_UNAVAILABLE"
	default:
		return "PROVISIONING_FAILED"
	}
}

// subsystemDeploymentFailureDetail 生成写入部署状态表的失败摘要：单行、限长、脱敏。
// 只有携带真实下层原因的错误才返回非空摘要；裸哨兵、上下文取消与纯内部细节（SQL/路径）
// 返回空串，让调用方保留既有“部署 Agent 执行失败”通用文案。
// 参数：err 为部署失败的下层原因。
// 返回值：可持久化的摘要；不携带可公开信息时返回空串。
func subsystemDeploymentFailureDetail(err error) string {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ""
	}
	message := err.Error()
	for _, sentinel := range subsystemFailureSentinels {
		message = strings.ReplaceAll(message, sentinel.Error(), " ")
	}
	return singleLineDiagnosticDetail(message)
}

// subsystemFailureSentinels 是不携带下层信息的哨兵：它们自身文本必须从摘要中剔除，
// 否则“裸哨兵”会被误判为可诊断失败而覆盖既有通用文案。
var subsystemFailureSentinels = []error{application.ErrSubsystemProvisioningUnavailable}

// singleLineDiagnosticDetail 把任意诊断文本压成单行、按 rune 限长并脱敏的摘要。
// last_error_message 是 varchar(1000) 且参与 JSON 编码：换行、超长或半个多字节字符
// 都会破坏状态接口，因此这里按 rune 截断而不是按字节。
// 参数：raw 为原始诊断文本。
// 返回值：单行摘要；命中 SQL/ORM 内部细节时返回空串。
func singleLineDiagnosticDetail(raw string) string {
	if internalErrorDetailPattern.MatchString(raw) {
		return ""
	}
	message := errorPathPattern.ReplaceAllString(raw, "[internal]")
	message = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' {
			return ' '
		}
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return ""
	}
	const limit = 800
	if utf8.RuneCountInString(message) > limit {
		message = string([]rune(message)[:limit]) + "...(truncated)"
	}
	return message
}

// subsystemLifecycleFailureDetail 生成站内通知用的失败摘要：稳定错误码 + 脱敏短摘要。
// 安全（SEC-B9）：含 SQL 关键词的原文整体丢弃，文件路径替换为占位符并截断；
// 完整 cause 只允许出现在结构化日志里。

// subsystemLifecycleFailureDetail 生成站内通知用的失败摘要：稳定错误码 + 脱敏短摘要。
// 安全（SEC-B9）：含 SQL 关键词的原文整体丢弃，文件路径替换为占位符并截断；
// 完整 cause 只允许出现在结构化日志里。
func subsystemLifecycleFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	code := subsystemProvisioningErrorCode(err)
	summary := redactInternalText(err.Error())
	if summary == "" {
		return code
	}
	return code + ": " + summary
}

// persistedDeploymentFailureDetail 生成可以写入 subsystem_deployment_state.last_error_message
// 的真实失败原因：单行、已脱敏、限长。没有它时，状态表与状态接口只剩一句“部署 Agent
// 执行失败”，运维无法区分许可运行态、镜像与数据库问题，只能盲目重试。
// 安全（SEC-B3）：SQL/ORM 关键词命中时整体丢弃；文件路径替换为 [internal]。
func persistedDeploymentFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if message == "" || internalErrorDetailPattern.MatchString(message) {
		return ""
	}
	message = errorPathPattern.ReplaceAllString(message, "[internal]")
	// 换行与制表符必须消失：该文本既要进 varchar(1000)，也要出现在状态接口的 JSON 里。
	message = strings.Join(strings.Fields(message), " ")
	const limit = 800
	if utf8.RuneCountInString(message) > limit {
		message = string([]rune(message)[:limit]) + "...(truncated)"
	}
	return message
}

// redactInternalText 剥离内部细节：SQL 关键词命中时返回空串（不保留任何原文），
// 文件路径替换为 [internal]，压缩空白并限制长度。

// redactInternalText 剥离内部细节：SQL 关键词命中时返回空串（不保留任何原文），
// 文件路径替换为 [internal]，压缩空白并限制长度。
func redactInternalText(raw string) string {
	message := strings.TrimSpace(raw)
	if message == "" {
		return ""
	}
	if internalErrorDetailPattern.MatchString(message) {
		return ""
	}
	message = errorPathPattern.ReplaceAllString(message, "[internal]")
	message = strings.Join(strings.Fields(message), " ")
	const limit = 200
	if len(message) > limit {
		message = message[:limit] + "..."
	}
	return message
}

// sanitizeLifecycleNotificationDetail 是通知入库前的最后一道防线：即使调用方误传
// 错误原文，含 SQL 关键词或文件路径的详情也只保留稳定错误码（或占位符）并限制长度。

// sanitizeLifecycleNotificationDetail 是通知入库前的最后一道防线：即使调用方误传
// 错误原文，含 SQL 关键词或文件路径的详情也只保留稳定错误码（或占位符）并限制长度。
func sanitizeLifecycleNotificationDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	if internalErrorDetailPattern.MatchString(detail) || errorPathPattern.MatchString(detail) {
		if code, _, ok := strings.Cut(detail, ":"); ok && stableDetailCodePattern.MatchString(strings.TrimSpace(code)) {
			return strings.TrimSpace(code)
		}
		return "[内部错误已脱敏]"
	}
	const limit = 300
	if len(detail) > limit {
		return detail[:limit] + "..."
	}
	return detail
}

func subsystemProvisioningNextAction(err error, stages ...string) string {
	message := strings.ToLower(err.Error())
	diagnosis := "平台部署 Agent 或目标 API 暂时不可用"
	switch {
	// 许可运行态判定必须优先于通用的“重建/启动失败”与“不可用”分支：这两类失败的真实
	// 原因就是许可运行态没下发，泛化文案会让运维只能反复重试。
	case strings.Contains(message, "license runtime state is unavailable"):
		diagnosis = "该应用的商业许可运行态未下发（常见原因：许可实例未初始化/未激活，或固定记录缺少 license-state 卷）。请先在商业授权完成导入与激活，再执行受控更新；不要重复创建应用环境"
	case strings.Contains(message, "commercial license configuration failed"):
		diagnosis = "目标 API 因商业许可配置失败而反复重启；请核对 license-state 卷挂载与许可实例是否一致"
	case strings.Contains(message, "disabled"):
		diagnosis = "当前环境未启用受控部署 Agent；请升级并重新发布生产部署资产，确认 platform-api 与 subsystem-provisioner 均健康"
	case strings.Contains(message, "deployment helper is unavailable"), strings.Contains(message, "read deployment response"), strings.Contains(message, "send deployment request"):
		diagnosis = "平台 API 无法连接生产部署 Agent；请检查 subsystem-provisioner 状态和启动日志，并确认 Agent 与 platform-api 使用同一版本"
	case strings.Contains(message, "target is not allowed"):
		diagnosis = "该应用/环境未被当前 Agent 的审核清单允许，或 Agent 仍运行旧版本；请同步最新 subsystems.d 清单并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "requires an approved manifest checksum"):
		diagnosis = "该应用/环境不在服务器 subsystems.d 审核清单中，或 platform-api 与 subsystem-provisioner 的部署模式不一致；请同步清单并确认两个服务都使用 SUBSYSTEM_ONBOARDING_MODE=production 后一同重启"
	case strings.Contains(message, "tenant is not allowed"):
		diagnosis = "当前租户不是该服务器绑定的生产租户；请核对 SUBSYSTEM_PRODUCTION_ALLOWED_TENANT_ID，禁止用其他租户覆盖现有实例"
	case strings.Contains(message, "preflight values are inconsistent"), strings.Contains(message, "integration values are inconsistent"), strings.Contains(message, "deployment request is invalid"):
		diagnosis = "页面提交的应用编码、环境、回调地址或上游地址与服务器审核清单不一致；请刷新应用接入页面并重新选择服务器接入目标"
	case strings.Contains(message, "infrastructure secrets are incomplete"):
		diagnosis = "服务器仍运行会在预检阶段检查全部基础设施密钥的旧版 Agent；请同步最新生产资产并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "subsystem database credentials are incomplete"):
		diagnosis = "当前子系统实际使用的数据库凭据仍为空或占位值；这些凭据可能已绑定持久化数据，Agent 不会自动生成或轮换，请完成一次性数据库初始化"
	case strings.Contains(message, "generated runtime secret is invalid"):
		diagnosis = "目标 runtime 中已有业务密钥不是合法的 32 字节 base64 值；为避免误轮换，Agent 不会覆盖非占位值，请先备份并修正该异常值"
	case strings.Contains(message, "runtime secrets are incomplete"):
		diagnosis = "目标 runtime 仍缺少必须由前置子系统接入产生的凭据；请先完成依赖子系统接入，再重试当前目标"
	case strings.Contains(message, "runtime template"), strings.Contains(message, "runtime directory"):
		diagnosis = "Agent 无法从随发布包审核的模板初始化 runtime；请确认最新 *.env.example 已部署且 runtime 目录可写，Agent 会自动创建文件并收紧为 0600"
	case strings.Contains(message, "production environment is unavailable"):
		diagnosis = "目标子系统 runtime 尚不可用；新版 Agent 会从审核模板自动初始化，请确认 platform-api、subsystem-provisioner 和生产部署资产版本一致"
	case strings.Contains(message, "release environment is unavailable"):
		diagnosis = "目标子系统的 .release.env 不可读或缺失；请确认部署目录的 .release.env 存在且权限为 0600"
	case strings.Contains(message, "immutable digest"):
		diagnosis = "该子系统镜像尚未发布到服务器：.release.env 中对应镜像仍是占位值。请在部署目录依次执行 ./bin/deploy.sh import packages/<组件>-*.tar.gz 与 ./bin/deploy.sh prepare <子系统>（import 只登记摘要，把不可变 digest 写入 .release.env 的是 prepare），再在当前环境点击“重试”"
	case strings.Contains(message, "initial administrator role"):
		diagnosis = "目标运行时已启动，但权限目录中缺少可用的初始角色；请检查目标 API 的目录同步日志"
	case strings.Contains(message, "production deployment file"), strings.Contains(message, "production deployment directory"), strings.Contains(message, "production compose configuration"):
		diagnosis = "服务器生产部署资产缺失、路径不规范或 Compose 校验失败；请重新发布平台生产部署资产并确认 Agent 健康"
	case strings.Contains(message, "contract_summary_client_id"), strings.Contains(message, "contract_summary_client_secret"), strings.Contains(message, "contract_summary_url"):
		diagnosis = "合同摘要校验已开启但运行时凭据不完整；重试不会从平台数据库恢复 OAuth 明文，请先同步包含合同摘要服务绑定的生产接入资产，或关闭合同校验后在当前环境重试"
	case strings.Contains(message, "contract_client_secret"):
		diagnosis = "客户与商机系统的合同签约用途凭据未写入 runtime；该键来自 service.contract_opportunity_signed_write。请先将基础平台更新到包含该用途受控重试下发修复的版本并重新部署 platform，再在原环境点击“重试”；不要手工写入 runtime 或重复创建应用环境"
	case strings.Contains(message, "runtime environment") || strings.Contains(message, "runtime configuration"):
		diagnosis = "Agent 无法安全更新目标运行配置；请确认 runtime 目录可写且文件不是符号链接，普通权限过宽会由 Agent 自动收紧为 0600"
	case strings.Contains(message, "start production subsystem dependencies"):
		diagnosis = "目标子系统的数据库或依赖服务未通过健康检查；请查看 subsystem-provisioner、目标数据库和依赖容器日志"
	case strings.Contains(message, "backup production subsystem database"), strings.Contains(message, "prepare production subsystem backup"):
		diagnosis = "目标子系统数据库备份失败；请检查数据库健康状态与 backups 目录权限"
	case strings.Contains(message, "migrate production subsystem database"):
		diagnosis = "目标子系统数据库迁移失败；请查看对应 migrate 容器日志并处理迁移错误，必要时使用接入前备份恢复"
	case strings.Contains(message, "start production subsystem services"):
		diagnosis = "目标 API 未能启动或通过健康检查；请查看目标 API 容器日志、runtime 配置和目录同步日志"
	case strings.Contains(message, "write production subsystem runtime configuration"):
		diagnosis = "Agent 无法安全写入目标 runtime 环境文件；请检查文件属主、0600 权限和 runtime 目录可写性"
	case strings.Contains(message, "production deployment lock is unavailable"):
		diagnosis = "服务器上已有发布或接入任务占用部署锁；请等待该任务结束，不要并行执行发布"
	case strings.Contains(message, "service credential is incomplete"), strings.Contains(message, "integration credential is incomplete"):
		diagnosis = "服务器审核清单引用的用途凭据尚未由平台控制面创建或交付；请同步最新平台镜像与清单，并确认 Agent 与 platform-api 版本一致"
	case strings.Contains(message, "compose file"):
		diagnosis = "部署 Agent 未找到生产 docker-compose.yml；请重新发布完整 platform/deploy/production 资产，并同时重建 platform-api、subsystem-provisioner"
	case strings.Contains(message, "environment template"):
		diagnosis = "部署 Agent 未找到目标运行配置模板；请重新发布完整生产部署资产并确认 runtime/*.env 已初始化"
	case strings.Contains(message, "docker service"):
		diagnosis = "Docker 服务不可用；请启动 Docker 并确认部署 Agent 可以访问 Docker Socket"
	case strings.Contains(message, "start subsystem containers"), strings.Contains(message, "rebuild subsystem containers"):
		diagnosis = "子系统构建或启动失败；请查看 subsystem-provisioner 与目标 API 容器日志"
	}
	if len(stages) > 0 && stages[0] == "preflight" {
		return diagnosis + "；修复后重新提交本次接入。该失败发生在预检阶段，平台尚未创建应用环境"
	}
	return diagnosis + "；修复后在当前环境点击“重试”，不要重复创建应用环境"
}
