// subsystem_onboarding_recovery_handler.go：部署编排与恢复（recovery）——失败部署清理、陈旧状态收口、异步部署编排与失败兜底。
package http

import (
	"context"
	"errors"
	"fmt"
	stdhttp "net/http"
	"strings"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
)

// DiscardFailedSubsystemDeployment 处理 POST /api/v1/subsystem-deployment-discard。
// 这是半成品子系统的清理路径：控制面记录存在、部署失败，运维需要释放
// code+environment 组合以便重新接入。
//
// 顺序是安全边界：先按 status+generation 原子占用记录（PROVISION_FAILED → DISCARDING），只有
// 抢占成功的请求才执行 Agent Teardown。旧实现先 Teardown、再按 status=FAILED 删除记录，并发
// 的「重试」把记录推进为 UPDATING 后删除会被拒绝，但 Teardown 已经拆掉了重试刚部署的环境。
// Teardown 仍是 best-effort（失败部署可能从未创建容器或文件）；应用/环境记录刻意保留，随后
// 用 DELETE /applications 与 DELETE /environments 释放路径前缀。
func (handler *SubsystemOnboardingHandler) DiscardFailedSubsystemDeployment(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	extendSubsystemDeploymentWriteDeadline(writer)
	principal, ok := subsystemPrincipal(writer, request)
	if !ok {
		return
	}
	var payload subsystemLifecycleRequest
	if !decodeApplicationManagementJSON(writer, request, &payload) {
		return
	}
	if err := validateLifecycleRequest(payload); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	applicationCode := strings.TrimSpace(payload.ApplicationCode)
	environment := strings.ToLower(strings.TrimSpace(payload.Environment))
	state, err := handler.deploymentState.GetSubsystemDeploymentState(request.Context(), principal.Tenant.ID, applicationCode, environment)
	if err != nil {
		handler.writeError(writer, request, err)
		return
	}
	state = handler.recoverStaleSubsystemDeployment(request.Context(), state)
	if state.Status != application.SubsystemDeploymentStatusFailed {
		handler.writeError(writer, request, fmt.Errorf("deployment is not in a failed state: %s", state.Status))
		return
	}
	now := time.Now().UTC()
	// 先占用再销毁：占用失败说明状态已被并发推进（通常是对同一环境的「重试」已经接管），
	// 此时继续 Teardown 会拆掉重试刚部署起来的基础设施，因此直接按冲突返回且不做任何清理。
	claimed, claimErr := handler.deploymentState.ClaimFailedSubsystemDeploymentForDiscard(request.Context(), principal.Tenant.ID, applicationCode, environment, state.Generation, now)
	if claimErr != nil {
		handler.writeError(writer, request, claimErr)
		return
	}
	if !claimed {
		handler.logger.Warn("failed subsystem deployment discard skipped because the state advanced",
			"path", request.URL.Path, "application_code", applicationCode, "environment", environment,
			"generation", state.Generation, "actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
		)
		handler.writeError(writer, request, fmt.Errorf("%w: 该环境的部署状态已被并发操作推进（可能已开始重试），本次未清理任何基础设施；请刷新状态后确认", application.ErrConflict))
		return
	}
	// 基础设施清理是尽力而为：失败部署可能在数据库提交后才停止，容器、.env.local
	// 或网关 include 可能根本不存在。
	if teardownErr := handler.provisioner.Teardown(request.Context(), principal.Tenant.ID, applicationCode, environment); teardownErr != nil {
		handler.logger.Warn("discard teardown left infrastructure residue",
			"application_code", applicationCode, "environment", environment, "error", teardownErr)
	}
	// 占用时 generation 自增 1，删除必须带上占用后的代号，避免误删被看护任务收口后的记录。
	if err := handler.deploymentState.DiscardClaimedSubsystemDeployment(request.Context(), principal.Tenant.ID, applicationCode, environment, state.Generation+1); err != nil {
		handler.writeError(writer, request, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store, private")
	writer.Header().Set("Pragma", "no-cache")
	httpresponse.WriteSuccess(writer, request, stdhttp.StatusOK, "失败部署已清理", map[string]string{
		"status":           "discarded",
		"application_code": applicationCode,
		"environment":      environment,
		"next_action":      "DELETE /applications 与 /environments 以释放路径前缀后重新接入",
	})
	handler.logger.Warn("failed subsystem deployment discarded", "path", request.URL.Path,
		"application_code", applicationCode,
		"environment", environment,
		"actor_user_id", principal.User.ID, "actor_tenant_id", principal.Tenant.ID,
	)
}

// recoverStaleSubsystemDeployment 只收口 API/Agent 重启可能遗留的生命周期状态。
// 转移刻意从已认证的状态读取路径执行，运维刷新页面即可恢复界面，
// 无需直接操作数据库或重新发起接入。
func (handler *SubsystemOnboardingHandler) recoverStaleSubsystemDeployment(ctx context.Context, state application.SubsystemDeploymentState) application.SubsystemDeploymentState {
	if !staleSubsystemDeployment(state, time.Now().UTC()) {
		return state
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	recovered, err := handler.deploymentState.RecoverStaleSubsystemDeployment(writeCtx, state, time.Now().UTC().Add(-subsystemDeploymentStaleAfter), time.Now().UTC())
	if err != nil {
		handler.logger.Warn("stale subsystem deployment could not be recovered",
			"application_code", state.ApplicationCode, "environment", state.Environment, "error", err)
		return state
	}
	if !recovered {
		current, getErr := handler.deploymentState.GetSubsystemDeploymentState(ctx, state.TenantID, state.ApplicationCode, state.Environment)
		if getErr == nil {
			return current
		}
		return state
	}
	state.Status = application.SubsystemDeploymentStatusFailed
	state.LastErrorCode = "DEPLOYMENT_INTERRUPTED"
	state.LastError = "部署请求中断，请点击重试"
	state.CompletedAt = pointerToTime(time.Now().UTC())
	state.UpdatedAt = time.Now().UTC()
	return state
}

func staleSubsystemDeployment(state application.SubsystemDeploymentState, now time.Time) bool {
	if state.Status != application.SubsystemDeploymentStatusProvisioning &&
		state.Status != application.SubsystemDeploymentStatusUpdating &&
		state.Status != application.SubsystemDeploymentStatusVerifying &&
		state.Status != application.SubsystemDeploymentStatusDiscarding {
		return false
	}
	if state.StartedAt == nil {
		return false
	}
	return now.Sub(state.StartedAt.UTC()) > subsystemDeploymentStaleAfter
}

func pointerToTime(value time.Time) *time.Time {
	return &value
}

// subsystemDeploymentJob 携带一次已受理的部署编排脱离 HTTP 请求执行。所有持久标识都在
// 请求返回前捕获完成；后台步骤绝不读取 request 作用域的状态。
type subsystemDeploymentJob struct {
	Generation         uint64
	Operation          string
	TenantID           string
	OperatorUserID     string
	ApplicationCode    string
	ApplicationName    string
	ApplicationID      string
	EnvironmentID      string
	Environment        string
	Input              application.SubsystemProvisioningInput
	InitialAdminUserID string
	NeedsInitialAccess bool
	KeycloakClientID   string
	KeycloakCutover    bool
	KeycloakRollback   bool
	ManifestChecksum   string
}

// subsystemDeploymentAsyncTimeout 约束一次脱离请求的部署编排，与 Agent 自身 16 分钟部署
// 窗口对齐：终态由本编排关闭，而不是落到 20 分钟的 stale 恢复路径（后者只兜进程死亡）。
const subsystemDeploymentAsyncTimeout = 16 * time.Minute

// startSubsystemDeploymentJob 在与 HTTP 请求解除关联的上下文上运行已受理的部署。客户端
// 断连、浏览器刷新或 CLI 超时因此不再能杀死进行中的 Agent，也不会把生命周期留在
// UPDATING；后台内的失败收口用 closeFailedDeployment（再二次脱离上下文），专供编排
// 超时后仍能把状态落进 PROVISION_FAILED。
func (handler *SubsystemOnboardingHandler) startSubsystemDeploymentJob(request *stdhttp.Request, job subsystemDeploymentJob) {
	timeout := handler.deploymentTimeout
	if timeout <= 0 {
		timeout = subsystemDeploymentAsyncTimeout
	}
	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), timeout)
	handler.deploymentJobs.Add(1)
	go func() {
		defer handler.deploymentJobs.Done()
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				handler.logger.Error("subsystem deployment job panicked",
					"application_code", job.ApplicationCode, "environment", job.Environment, "panic", recovered)
				handler.markDeploymentFailureDetached(job, "DEPLOYMENT_JOB_PANICKED", "部署编排异常终止；请修复后点击重试")
			}
		}()
		if runErr := handler.runSubsystemDeploymentCompletion(bgCtx, job); runErr != nil {
			handler.logger.Error("subsystem deployment job finished with error",
				"application_code", job.ApplicationCode, "environment", job.Environment,
				"operation", job.Operation, "error", runErr)
		}
	}()
}

// runSubsystemDeploymentCompletion 执行长耗时的 Agent 调用与全部持久终态写。每个失败分支
// 都自行把生命周期收口（脱离 ctx），编排超时也只会落在 PROVISION_FAILED，绝不冻结在
// UPDATING。
func (handler *SubsystemOnboardingHandler) runSubsystemDeploymentCompletion(ctx context.Context, job subsystemDeploymentJob) error {
	if handler.runtimeLicenses != nil {
		if handler.runtimeLicenseSettings == nil {
			handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_BINDING_FAILED", "授权实例绑定读取未配置", application.ErrValidation, false)
			return application.ErrValidation
		}
		settings, settingsErr := handler.runtimeLicenseSettings(ctx)
		if settingsErr != nil && !errors.Is(settingsErr, application.ErrRuntimeLicenseNotInitialized) {
			handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_BINDING_FAILED", "授权实例绑定读取失败", settingsErr, false)
			return settingsErr
		}
		if settingsErr == nil {
			job.Input.RuntimeLicenseSettings = settings
			credentials, err := handler.runtimeLicenses.PrepareWithSettings(ctx, job.TenantID, job.ApplicationID, job.EnvironmentID, job.ApplicationCode, job.Environment, job.OperatorUserID, settings)
			if err != nil {
				handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_ENROLLMENT_FAILED", "授权运行组件登记失败；请检查批准发布清单后重试", err, false)
				return err
			}
			job.Input.RuntimeLicenseCredentials = credentials
		}
	}
	if job.Operation == "ONBOARD" {
		if err := handler.provisioner.Provision(ctx, job.Input); err != nil {
			handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败", err, true)
			return err
		}
		if err := handler.waitLocalRuntimeConfirmation(ctx, job); err != nil {
			return err
		}
		if err := handler.markManifestApplied(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.ManifestChecksum); err != nil {
			handler.closeFailedDeployment(ctx, job, "MANIFEST_STATE_FAILED", "部署清单状态保存失败", err, true)
			return err
		}
		if _, err := handler.access.AssignInitialAdministrator(ctx, job.TenantID, job.ApplicationCode, job.InitialAdminUserID, job.OperatorUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_ASSIGNMENT_FAILED", "初始管理员授权失败", err, true)
			return err
		}
		if err := handler.markInitialAccessAssigned(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.InitialAdminUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_STATE_FAILED", "初始管理员授权状态保存失败", err, true)
			return err
		}
		if err := handler.completeOnboardKeycloakProjection(ctx, job); err != nil {
			return err
		}
		if err := handler.completeDeployment(ctx, job, application.SubsystemDeploymentStatusReady, "", ""); err != nil {
			// 编排本身已完成而终态写失败：必须收口为失败，不能把状态机冻结在 UPDATING。
			handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_STATE_FAILED", "部署完成但状态写入失败", err, true)
			return err
		}
		handler.notifySubsystemLifecycle(ctx, job.TenantID, job.OperatorUserID, job.ApplicationName, job.ApplicationCode, job.Environment, true, "")
		return nil
	}
	if err := handler.provisioner.Update(ctx, job.Input); err != nil {
		handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_AGENT_FAILED", "部署 Agent 执行失败", err, true)
		return err
	}
	if err := handler.waitLocalRuntimeConfirmation(ctx, job); err != nil {
		return err
	}
	if err := handler.markManifestApplied(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.ManifestChecksum); err != nil {
		handler.closeFailedDeployment(ctx, job, "MANIFEST_STATE_FAILED", "部署清单状态保存失败", err, false)
		return err
	}
	if job.NeedsInitialAccess {
		if _, err := handler.access.AssignInitialAdministrator(ctx, job.TenantID, job.ApplicationCode, job.InitialAdminUserID, job.OperatorUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_ASSIGNMENT_FAILED", "初始管理员授权失败", err, false)
			return err
		}
		if err := handler.markInitialAccessAssigned(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.InitialAdminUserID); err != nil {
			handler.closeFailedDeployment(ctx, job, "INITIAL_ACCESS_STATE_FAILED", "初始管理员授权状态保存失败", err, false)
			return err
		}
	}
	if job.KeycloakCutover || job.KeycloakRollback {
		var lifecycleErr error
		if job.KeycloakCutover {
			_, lifecycleErr = handler.keycloakCutover.ConfirmKeycloakCutover(ctx, job.TenantID, job.ApplicationID, job.EnvironmentID, job.OperatorUserID, keycloakRollbackWindow)
		} else {
			_, lifecycleErr = handler.keycloakCutover.RecordKeycloakRollback(ctx, job.TenantID, job.ApplicationID, job.EnvironmentID, job.OperatorUserID)
		}
		if lifecycleErr != nil {
			handler.logger.Error("Keycloak runtime change completed but lifecycle evidence could not be recorded",
				"application_code", job.ApplicationCode, "environment", job.Environment, "error", lifecycleErr)
			handler.closeFailedDeployment(ctx, job, "KEYCLOAK_LIFECYCLE_RECORD_FAILED", "Keycloak 切换证据记录失败", lifecycleErr, true)
			return lifecycleErr
		}
	}
	if err := handler.completeDeployment(ctx, job, application.SubsystemDeploymentStatusReady, "", ""); err != nil {
		handler.closeFailedDeployment(ctx, job, "DEPLOYMENT_STATE_FAILED", "部署完成但状态写入失败", err, true)
		return err
	}
	handler.notifySubsystemLifecycle(ctx, job.TenantID, job.OperatorUserID, job.ApplicationName, job.ApplicationCode, job.Environment, true, "")
	return nil
}

// completeOnboardKeycloakProjection 完成 Keycloak 优先接入的角色目录投影与就绪标记。
//
// 具体步骤由共享应用服务 KeycloakAuthorizationProjectionService 实现：新旧接入入口
// 调用同一份逻辑，避免"新入口迁移了主步骤、却漏掉旧流程必要处理"的漂移。
func (handler *SubsystemOnboardingHandler) completeOnboardKeycloakProjection(ctx context.Context, job subsystemDeploymentJob) error {
	if job.KeycloakClientID == "" {
		return nil
	}
	if handler.keycloakCatalog == nil || handler.keycloakMappings == nil {
		handler.closeFailedDeployment(ctx, job, application.KeycloakProjectionCodeMappingUnavailable, "Keycloak 授权映射组件不可用", nil, false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	var readiness application.KeycloakProjectionReadinessUpdater
	if updater, ok := handler.keycloakReadiness.(keycloakSwitchReadinessUpdater); ok {
		readiness = updater
	}
	service, err := application.NewKeycloakAuthorizationProjectionService(handler.keycloakCatalog, handler.keycloakControl, handler.keycloakMappings, readiness)
	if err != nil {
		handler.closeFailedDeployment(ctx, job, application.KeycloakProjectionCodeMappingUnavailable, "Keycloak 授权映射组件不可用", err, false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	result, err := service.Project(ctx, application.KeycloakProjectionInput{
		TenantID: job.TenantID, ApplicationID: job.ApplicationID, EnvironmentID: job.EnvironmentID,
		ClientID: job.KeycloakClientID, Realm: handler.keycloakRealm,
	})
	if err != nil {
		handler.closeFailedDeployment(ctx, job, application.KeycloakProjectionCodeBackfillFail, "Keycloak 授权投影失败", err, false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	if result.ErrorCode != "" {
		handler.closeFailedDeployment(ctx, job, result.ErrorCode, keycloakProjectionFailureMessage(result.ErrorCode), errors.New(result.Diagnostic), false)
		return application.ErrSubsystemProvisioningUnavailable
	}
	return nil
}

// keycloakProjectionFailureMessage 把共享服务的错误码翻译成面向操作员的描述。
func keycloakProjectionFailureMessage(code string) string {
	switch code {
	case application.KeycloakProjectionCodeRoleCatalogSyncFail:
		return "Keycloak 角色目录同步失败"
	case application.KeycloakProjectionCodeClientMappingFail:
		return "Keycloak Client 映射保存失败"
	case application.KeycloakProjectionCodeBackfillFail:
		return "Keycloak 授权投影回填失败"
	case application.KeycloakProjectionCodeReadinessFail:
		return "Keycloak 就绪状态写入失败"
	default:
		return "Keycloak 授权投影失败"
	}
}

// closeFailedDeployment 用独立超时的上下文把失败收口进生命周期状态机：即使编排 ctx 已因
// 超时取消，状态机也必须落在 PROVISION_FAILED。通知沿用原始 ctx 尽力而为。
func (handler *SubsystemOnboardingHandler) closeFailedDeployment(ctx context.Context, job subsystemDeploymentJob, errorCode, errorMessage string, cause error, notify bool) {
	if code := application.RuntimeLicenseApprovalErrorCode(cause); code != "" {
		errorCode = code
		errorMessage, _ = runtimeLicenseApprovalDiagnostic(code)
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	if err := handler.completeDeployment(writeCtx, job, application.SubsystemDeploymentStatusFailed, errorCode, errorMessage); err != nil {
		handler.logger.Error("failed to persist subsystem deployment failure",
			"application_code", job.ApplicationCode, "environment", job.Environment, "error", err)
	}
	if notify {
		handler.notifySubsystemLifecycle(ctx, job.TenantID, job.OperatorUserID, job.ApplicationName, job.ApplicationCode, job.Environment, false, subsystemLifecycleFailureDetail(cause))
	}
}

// waitForDeploymentJobs 等待全部已受理的后台部署完成。本包测试用它使异步编排
// 可以确定性收口。
func (handler *SubsystemOnboardingHandler) waitForDeploymentJobs() {
	handler.deploymentJobs.Wait()
}

// markDeploymentFailureDetached 是 panic 兜底专用的失败收口：完全脱离任何可能已取消的
// 上下文。
func (handler *SubsystemOnboardingHandler) markDeploymentFailureDetached(job subsystemDeploymentJob, errorCode, errorMessage string) {
	writeCtx, cancel := context.WithTimeout(context.Background(), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	if err := handler.completeDeployment(writeCtx, job, application.SubsystemDeploymentStatusFailed, errorCode, errorMessage); err != nil {
		handler.logger.Error("failed to persist subsystem deployment failure after panic",
			"application_code", job.ApplicationCode, "environment", job.Environment, "error", err)
	}
}

func (handler *SubsystemOnboardingHandler) completeDeployment(ctx context.Context, job subsystemDeploymentJob, status, code, message string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.CompleteSubsystemDeployment(ctx, job.TenantID, job.ApplicationCode, job.Environment, job.Generation, status, job.Operation, code, message, time.Now().UTC())
}

// waitLocalRuntimeConfirmation 只对本地模式生效：本地开发允许在购买强制校验未完成时
// 先行部署，改为观察真实的消费者确认，而不是要求业务活动证明。
func (handler *SubsystemOnboardingHandler) waitLocalRuntimeConfirmation(ctx context.Context, job subsystemDeploymentJob) error {
	if len(job.Input.RuntimeLicenseCredentials) == 0 {
		return nil
	}
	provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider)
	if !ok || provider.Capabilities().Mode != "local" {
		return nil
	}
	if handler.runtimeLicenses == nil {
		return application.ErrValidation
	}
	err := handler.runtimeLicenses.WaitForRuntimeConfirmation(ctx, job.ApplicationCode, job.Environment, job.Input.RuntimeLicenseCredentials)
	if err != nil {
		handler.closeFailedDeployment(ctx, job, "LICENSE_RUNTIME_CONFIRMATION_FAILED", "运行组件尚未完成当期授权确认；请检查组件同步状态后重试", err, false)
	}
	return err
}
