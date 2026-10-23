package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// 阶段执行器：把既有目录/预检/部署（Agent）能力编排成固定阶段。
// 端口返回 SubsystemAccessStageOutcome 表达"成功/等待/失败"，用 error 表达"结果未知（可重试）"，
// 从而让 Worker 区分"已知缺项"与"结果未知"，避免重复执行外部副作用。

// ErrSubsystemAccessStageNotWired 表示该动作或阶段尚未接线：**不可重试**，
// 操作应直接判定失败并提示改用既有受控入口（避免无意义的重试与"结果未知"混淆）。
var ErrSubsystemAccessStageNotWired = errors.New("subsystem access stage is not wired")

// SubsystemAccessRunState 是一次运行内的共享状态（Worker 持有，**不落库、不打日志**）。
// 目录与凭据由既有原子流程一次创建后，凭据经此交接给部署阶段；运行结束即丢弃。
type SubsystemAccessRunState struct {
	Credentials *SubsystemProvisioningCredentials
}

// SubsystemProvisioningCredentials 是既有流程解析出的部署凭据（仅内存传递）。
type SubsystemProvisioningCredentials struct {
	ClientID                     string
	ClientSecret                 string
	CatalogPublisherClientID     string
	CatalogPublisherClientSecret string
	ServiceCredentials           []SubsystemServiceCredential
	RelatedServiceClients        []RelatedSubsystemServiceClient
	RedirectURI                  string
	PublicURL                    string
}

// SubsystemAccessOnboardingPort 提供预检与目录登记（既有 onboarding 服务实现）。
type SubsystemAccessOnboardingPort interface {
	// PreflightSubsystemAccess 执行条件检查；不可满足时返回 WAITING/FAILED 的 outcome。
	PreflightSubsystemAccess(ctx context.Context, operation SubsystemAccessOperation) (SubsystemAccessStageOutcome, error)
	// RegisterSubsystemAccessDirectory 登记或复用应用/环境/登录目标（幂等）。
	RegisterSubsystemAccessDirectory(ctx context.Context, operation SubsystemAccessOperation) (SubsystemAccessStageOutcome, error)
}

// SubsystemAccessAuthenticationPort 是可选端口：由"目录与凭据一次原子创建"的既有流程实现，
// 负责在认证阶段创建/复用 OAuth Client 与凭据，并把凭据写入运行状态供部署阶段使用（不落库）。
type SubsystemAccessAuthenticationPort interface {
	ConfigureSubsystemAccessAuthentication(ctx context.Context, operation SubsystemAccessOperation, run *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error)
}

// SubsystemAccessDeploymentPort 提供部署与就绪核对（由部署 Agent 的 provision/update/状态实现）。
type SubsystemAccessDeploymentPort interface {
	// ProvisionSubsystemAccess 首次接入的部署（含 Agent 侧的认证配置与启动完整运行集合）。
	// run 携带本次运行内由认证阶段交接的凭据（仅内存）。
	ProvisionSubsystemAccess(ctx context.Context, operation SubsystemAccessOperation, run *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error)
	// UpdateSubsystemAccess 受控更新（接管/更新/恢复走同一入口）。
	UpdateSubsystemAccess(ctx context.Context, operation SubsystemAccessOperation, run *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error)
	// SubsystemAccessDeploymentReady 核对部署已就绪（健康、组件、镜像一致）。
	SubsystemAccessDeploymentReady(ctx context.Context, operation SubsystemAccessOperation) (SubsystemAccessStageOutcome, error)
	// AuthHandledByProvisioning 表示认证配置由部署操作一并完成（Agent 契约）。
	AuthHandledByProvisioning() bool
}

// SubsystemAccessFinalizerPort 是可选完成收口端口（结果阶段）：初始管理员授权等写操作。
type SubsystemAccessFinalizerPort interface {
	FinalizeSubsystemAccess(ctx context.Context, operation SubsystemAccessOperation, run *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error)
}

// SubsystemAccessStageExecutorAdapter 实现 Worker 的阶段执行端口。
type SubsystemAccessStageExecutorAdapter struct {
	onboarding SubsystemAccessOnboardingPort
	deployment SubsystemAccessDeploymentPort
}

// NewSubsystemAccessStageExecutor 构造执行器；依赖缺失即失败。
func NewSubsystemAccessStageExecutor(onboarding SubsystemAccessOnboardingPort, deployment SubsystemAccessDeploymentPort) (*SubsystemAccessStageExecutorAdapter, error) {
	if onboarding == nil || deployment == nil {
		return nil, errors.New("subsystem access stage executor requires onboarding and deployment ports")
	}
	return &SubsystemAccessStageExecutorAdapter{onboarding: onboarding, deployment: deployment}, nil
}

// ExecuteSubsystemAccessStage 执行单个阶段；run 携带本次运行内的凭据交接（可为 nil）。
func (executor *SubsystemAccessStageExecutorAdapter) ExecuteSubsystemAccessStage(ctx context.Context, operation SubsystemAccessOperation, stage SubsystemAccessStage, run *SubsystemAccessRunState) (SubsystemAccessStageOutcome, error) {
	if run == nil {
		run = &SubsystemAccessRunState{}
	}
	switch stage {
	case SubsystemAccessStageCheck:
		return executor.onboarding.PreflightSubsystemAccess(ctx, operation)
	case SubsystemAccessStageDirectory:
		if operation.Action == SubsystemAccessActionOnboard {
			if _, ok := executor.deployment.(SubsystemAccessAuthenticationPort); ok {
				// 首次接入的目录与凭据由既有原子流程一次创建（认证阶段承担），此处不重复登记。
				return SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: "delegated:legacy-onboarding"}, nil
			}
		}
		return executor.onboarding.RegisterSubsystemAccessDirectory(ctx, operation)
	case SubsystemAccessStageAuth:
		// 既有原子流程（目录+凭据一次创建）通过该可选端口实现，并把凭据放进运行状态。
		if authentication, ok := executor.deployment.(SubsystemAccessAuthenticationPort); ok {
			return authentication.ConfigureSubsystemAccessAuthentication(ctx, operation, run)
		}
		if executor.deployment.AuthHandledByProvisioning() {
			// 认证配置由部署操作一并完成：此处只声明"随部署生效"，不重复创建 Client。
			return SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded,
				EvidenceRef: "delegated:provisioning"}, nil
		}
		return SubsystemAccessStageOutcome{}, fmt.Errorf("auth stage is not wired: no non-delegated auth executor configured")
	case SubsystemAccessStageDeploy:
		switch operation.Action {
		case SubsystemAccessActionOnboard:
			return executor.deployment.ProvisionSubsystemAccess(ctx, operation, run)
		case SubsystemAccessActionAdopt, SubsystemAccessActionUpdate, SubsystemAccessActionRetry:
			return executor.deployment.UpdateSubsystemAccess(ctx, operation, run)
		default:
			return SubsystemAccessStageOutcome{}, fmt.Errorf("action %s cannot be deployed: %w", operation.Action, ErrSubsystemAccessInvalidRequest)
		}
	case SubsystemAccessStageReadiness:
		return executor.deployment.SubsystemAccessDeploymentReady(ctx, operation)
	case SubsystemAccessStageResult:
		// 完成收口：注入端口时由受控服务执行（初始管理员授权等）；否则只汇总证据引用。
		if finalizer, ok := executor.deployment.(SubsystemAccessFinalizerPort); ok {
			return finalizer.FinalizeSubsystemAccess(ctx, operation, run)
		}
		reference := strings.TrimSpace(operation.ResultRef)
		if reference == "" {
			reference = "operation:" + operation.ID + "/result"
		}
		return SubsystemAccessStageOutcome{Status: SubsystemAccessStatusSucceeded, EvidenceRef: reference}, nil
	default:
		return SubsystemAccessStageOutcome{}, fmt.Errorf("unknown stage %q: %w", stage, ErrSubsystemAccessInvalidRequest)
	}
}
