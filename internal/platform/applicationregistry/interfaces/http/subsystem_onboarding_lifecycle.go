package http

import (
	"context"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httperror"
	"github.com/J-S-Te/Basic-Platform/internal/shared/httpresponse"
	stdhttp "net/http"
	"strings"
	"time"
)

// allowedServiceBindingsForTarget 返回服务器清单为该应用/环境声明的服务用途白名单；
// 未声明时返回 nil，应用层回退到平台硬编码默认。
func (handler *SubsystemOnboardingHandler) allowedServiceBindingsForTarget(applicationCode, environment string) []string {
	if provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider); ok {
		for _, target := range provider.Capabilities().Targets {
			if target.ApplicationCode == strings.TrimSpace(applicationCode) && target.Environment == strings.TrimSpace(environment) {
				if target.AllowedServiceBindings == nil {
					return nil
				}
				return append([]string(nil), target.AllowedServiceBindings...)
			}
		}
	}
	return nil
}

func (handler *SubsystemOnboardingHandler) manifestChecksumForTarget(applicationCode, environment string) string {
	provider, ok := handler.provisioner.(subsystemProvisioningCapabilityProvider)
	if !ok {
		return ""
	}
	for _, target := range provider.Capabilities().Targets {
		if strings.EqualFold(target.ApplicationCode, strings.TrimSpace(applicationCode)) && strings.EqualFold(target.Environment, strings.TrimSpace(environment)) {
			return strings.TrimSpace(target.ManifestChecksum)
		}
	}
	return ""
}

func (handler *SubsystemOnboardingHandler) setDesiredManifest(ctx context.Context, tenantID, applicationCode, environment, checksum string) error {
	if strings.TrimSpace(checksum) == "" {
		return nil
	}
	store, ok := handler.deploymentState.(application.SubsystemManifestStateStore)
	if !ok {
		return nil
	}
	return store.SetSubsystemDesiredManifest(ctx, tenantID, applicationCode, environment, checksum, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markManifestApplied(ctx context.Context, tenantID, applicationCode, environment, checksum string) error {
	if strings.TrimSpace(checksum) == "" {
		return nil
	}
	store, ok := handler.deploymentState.(application.SubsystemManifestStateStore)
	if !ok {
		return nil
	}
	return store.MarkSubsystemManifestApplied(ctx, tenantID, applicationCode, environment, checksum, time.Now().UTC())
}

// notifySubsystemLifecycle 在接入/重试/更新成功或失败时向操作人发送站内通知；通知失败只记日志，
// 不阻断接入结果（接入本身是否成功由控制面状态决定）。

// notifySubsystemLifecycle 在接入/重试/更新成功或失败时向操作人发送站内通知；通知失败只记日志，
// 不阻断接入结果（接入本身是否成功由控制面状态决定）。
func (handler *SubsystemOnboardingHandler) notifySubsystemLifecycle(ctx context.Context, tenantID, operatorID, applicationName, applicationCode, environment string, succeeded bool, detail string) {
	if handler.notifications == nil || strings.TrimSpace(operatorID) == "" {
		return
	}
	// 安全（SEC-B9）：入库前最后一道防线——即使调用方误传错误原文，含 SQL 关键词
	// 或文件路径的通知详情也只保留稳定错误码（或占位符），完整 cause 只进结构化日志。
	detail = sanitizeLifecycleNotificationDetail(detail)
	if err := handler.notifications.SendSubsystemLifecycle(ctx, SubsystemLifecycleNotification{
		TenantID: tenantID, OperatorID: operatorID, ApplicationName: applicationName,
		ApplicationCode: applicationCode, Environment: environment, Succeeded: succeeded, Detail: detail,
	}); err != nil {
		handler.logger.Warn("subsystem lifecycle notification failed", "application_code", applicationCode, "environment", environment, "error", err)
	}
}

func (handler *SubsystemOnboardingHandler) transitionDeployment(ctx context.Context, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.TransitionSubsystemDeployment(ctx, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markInitialAccessAssigned(ctx context.Context, tenantID, applicationCode, environment, userID string) error {
	if handler.deploymentState == nil {
		return nil
	}
	return handler.deploymentState.MarkSubsystemInitialAccessAssigned(ctx, tenantID, applicationCode, environment, userID, time.Now().UTC())
}

func (handler *SubsystemOnboardingHandler) markDeploymentFailed(ctx context.Context, tenantID, applicationCode, environment, operation, errorCode, errorMessage string) {
	if err := handler.transitionDeploymentFailure(ctx, tenantID, applicationCode, environment, operation, errorCode, errorMessage); err != nil {
		handler.logger.Error("failed to persist subsystem deployment failure", "application_code", applicationCode, "environment", environment, "error", err)
	}
}

func (handler *SubsystemOnboardingHandler) transitionDeploymentFailure(ctx context.Context, tenantID, applicationCode, environment, operation, errorCode, errorMessage string) error {
	if handler.deploymentState == nil {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subsystemDeploymentFailureWriteTimeout)
	defer cancel()
	return handler.transitionDeployment(writeCtx, tenantID, applicationCode, environment, application.SubsystemDeploymentStatusFailed, operation, errorCode, errorMessage)
}

func validateLifecycleRequest(payload subsystemLifecycleRequest) error {
	if strings.TrimSpace(payload.ApplicationCode) == "" {
		return application.ErrValidation
	}
	if strings.TrimSpace(payload.Environment) == "" {
		return application.ErrValidation
	}
	return nil
}

// isBuiltInPlatformApplication 判断目标应用是否是平台自身的控制面应用。
// 取值刻意镜像未导出的 application.builtInApplicationCode：HTTP 边界保留自己的
// 副本，使运行时生命周期守卫不因别处重命名而漂移。

// isBuiltInPlatformApplication 判断目标应用是否是平台自身的控制面应用。
// 取值刻意镜像未导出的 application.builtInApplicationCode：HTTP 边界保留自己的
// 副本，使运行时生命周期守卫不因别处重命名而漂移。
func isBuiltInPlatformApplication(applicationCode string) bool {
	switch strings.ToLower(strings.TrimSpace(applicationCode)) {
	case "platform", "basic_platform":
		return true
	default:
		return false
	}
}

// writeBuiltInPlatformRuntimeBlocked 拒绝针对平台应用自身的运行时生命周期操作。
// 平台运行时由平台发布流程部署，绝不由子系统部署 Agent 部署；
// 暴露该动作只会产生一个难以理解的 Agent 失败。

// writeBuiltInPlatformRuntimeBlocked 拒绝针对平台应用自身的运行时生命周期操作。
// 平台运行时由平台发布流程部署，绝不由子系统部署 Agent 部署；
// 暴露该动作只会产生一个难以理解的 Agent 失败。
func writeBuiltInPlatformRuntimeBlocked(writer stdhttp.ResponseWriter, request *stdhttp.Request, applicationCode, environment string) {
	httpresponse.WriteError(writer, request, stdhttp.StatusConflict, httperror.New(
		"IAM_PLATFORM_RUNTIME_NOT_MANAGED",
		"基础平台应用由平台发布流程管理，不能通过子系统部署 Agent 更新、重试、接管或下线运行时",
		map[string]string{
			"application_code": applicationCode,
			"environment":      environment,
			"next_action": "基础平台自身的升级请在服务器部署目录按发布流程执行；" +
				"子系统运行时的部署操作只适用于 subsystems.d 审核清单内的应用环境",
		},
	))
}

// writeProductionManifestTargetMissing 拒绝目标缺少已审核清单校验和的生产运行时操作。
// 触发场景：API 在 subsystems.d 中找不到该应用/环境，或 platform-api 与
// subsystem-provisioner 对部署模式的认定不一致。在此处失败可保持生命周期状态
// 不被触碰，并给操作者一个具体的修复方向。

// writeProductionManifestTargetMissing 拒绝目标缺少已审核清单校验和的生产运行时操作。
// 触发场景：API 在 subsystems.d 中找不到该应用/环境，或 platform-api 与
// subsystem-provisioner 对部署模式的认定不一致。在此处失败可保持生命周期状态
// 不被触碰，并给操作者一个具体的修复方向。
func writeProductionManifestTargetMissing(writer stdhttp.ResponseWriter, request *stdhttp.Request, applicationCode, environment string) {
	httpresponse.WriteError(writer, request, stdhttp.StatusUnprocessableEntity, httperror.New(
		"IAM_SUBSYSTEM_TARGET_NOT_IN_PRODUCTION_MANIFEST",
		"服务器审核清单中没有该应用环境的受控部署目标，无法自动更新运行时",
		map[string]string{
			"application_code": applicationCode,
			"environment":      environment,
			"next_action": "请确认 platform-api 与 subsystem-provisioner 都使用同一组已审核的 subsystems.d 清单和 SUBSYSTEM_ONBOARDING_MODE=production，" +
				"并同步重启两个服务；若该应用环境确实不在清单内，请先通过代码评审发布对应清单",
		},
	))
}

func subsystemPrincipal(writer stdhttp.ResponseWriter, request *stdhttp.Request) (authctx.Principal, bool) {
	principal, ok := authctx.PrincipalFromContext(request.Context())
	if !ok || strings.TrimSpace(principal.Tenant.ID) == "" || strings.TrimSpace(principal.User.ID) == "" {
		httpresponse.WriteError(writer, request, stdhttp.StatusUnauthorized, httperror.Unauthenticated)
		return authctx.Principal{}, false
	}
	return principal, true
}
