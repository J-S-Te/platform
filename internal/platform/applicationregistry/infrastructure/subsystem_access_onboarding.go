package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// SubsystemAccessOnboardingService 是既有 onboarding 服务中本端口使用的方法（窄接口）。
type SubsystemAccessOnboardingService interface {
	PreflightValidate(context.Context, application.SubsystemOnboardingInput) error
	RegisterSubsystemDirectory(context.Context, application.SubsystemDirectoryRegistrationInput) (application.SubsystemDirectoryRegistrationResult, error)
}

// SubsystemAccessRegistrationLookup 查询目标是否已登记（幂等与"结果未知先核对"的依据）。
type SubsystemAccessRegistrationLookup interface {
	LookupSubsystemAccessRegistration(ctx context.Context, tenantID, applicationCode, environmentCode string) (applicationID string, environmentID string, found bool, err error)
}

// SubsystemAccessOnboardingAdapter 实现阶段执行器的 CHECK 与 DIRECTORY 端口。
// 语义：已登记则复用（不重复写目录）；清单未命中即失败；既有校验失败即失败；其余错误视为"结果未知"（可重试）。
type SubsystemAccessOnboardingAdapter struct {
	service  SubsystemAccessOnboardingService
	catalog  SubsystemAccessCatalogSource
	registry SubsystemAccessRegistrationLookup
}

// NewSubsystemAccessOnboardingAdapter 构造适配器；依赖缺失即失败。
func NewSubsystemAccessOnboardingAdapter(service SubsystemAccessOnboardingService, catalog SubsystemAccessCatalogSource, registry SubsystemAccessRegistrationLookup) (*SubsystemAccessOnboardingAdapter, error) {
	if service == nil || catalog == nil || registry == nil {
		return nil, errors.New("subsystem access onboarding adapter requires service, catalog and registry lookup")
	}
	return &SubsystemAccessOnboardingAdapter{service: service, catalog: catalog, registry: registry}, nil
}

// PreflightSubsystemAccess 条件检查：先确认目标在清单内，再调用既有预检。
func (adapter *SubsystemAccessOnboardingAdapter) PreflightSubsystemAccess(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemAccessStageOutcome, error) {
	target, ok, err := adapter.resolveTarget(ctx, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !ok {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "OUT_OF_SCOPE",
			Message: "目标不在当前清单的可接入范围内"}, nil
	}
	if operation.Action != application.SubsystemAccessActionOnboard {
		// 非首次接入（接管/更新/重试）：绝不能执行"首次接入预检"——它会因为环境已存在
		// 而直接拒绝，导致 CHECK 阶段永远失败、统一入口只剩首次接入可用。
		// 这里只断言目标仍在当前清单内（上面已 resolve），并留下可追溯证据。
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
			EvidenceRef: "preflight:existing:" + strings.TrimSpace(target.ManifestChecksum)}, nil
	}
	input := adapter.onboardingInput(operation, target)
	if err := adapter.service.PreflightValidate(ctx, input); err != nil {
		if errors.Is(err, application.ErrValidation) {
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
				ErrorCode: "PREFLIGHT_REJECTED", Message: err.Error()}, nil
		}
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("preflight subsystem access: %w", err)
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "preflight:" + strings.TrimSpace(target.ManifestChecksum)}, nil
}

// RegisterSubsystemAccessDirectory 目录登记：已登记则复用既有登记（幂等，不重复写）。
func (adapter *SubsystemAccessOnboardingAdapter) RegisterSubsystemAccessDirectory(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemAccessStageOutcome, error) {
	if strings.TrimSpace(operation.ApplicationID) != "" && strings.TrimSpace(operation.EnvironmentID) != "" {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
			EvidenceRef: "directory:existing:" + operation.ApplicationID + "/" + operation.EnvironmentID}, nil
	}
	applicationID, environmentID, found, err := adapter.registry.LookupSubsystemAccessRegistration(ctx,
		operation.TenantID, operation.ApplicationCode, operation.EnvironmentCode)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if found {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
			EvidenceRef: "directory:existing:" + applicationID + "/" + environmentID}, nil
	}
	target, ok, err := adapter.resolveTarget(ctx, operation)
	if err != nil {
		return application.SubsystemAccessStageOutcome{}, err
	}
	if !ok {
		return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed, ErrorCode: "OUT_OF_SCOPE",
			Message: "目标不在当前清单的可接入范围内"}, nil
	}
	result, err := adapter.service.RegisterSubsystemDirectory(ctx, application.SubsystemDirectoryRegistrationInput{
		TenantID: operation.TenantID, OperatorID: operation.OperatorID, ApplicationCode: operation.ApplicationCode,
		ApplicationName: target.ApplicationName, Environment: operation.EnvironmentCode,
		PublicBaseURL: strings.TrimSpace(operation.PublicOrigin), UpstreamURL: target.UpstreamURL,
		PathPrefix: target.PathPrefix, IssuerAlias: optionalAccessString(operation.AuthProvider),
	})
	if err != nil {
		if errors.Is(err, application.ErrValidation) {
			return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusFailed,
				ErrorCode: "DIRECTORY_REJECTED", Message: err.Error()}, nil
		}
		return application.SubsystemAccessStageOutcome{}, fmt.Errorf("register subsystem directory: %w", err)
	}
	return application.SubsystemAccessStageOutcome{Status: application.SubsystemAccessStatusSucceeded,
		EvidenceRef: "directory:created:" + result.Application.ID + "/" + result.Environment.ID}, nil
}

func (adapter *SubsystemAccessOnboardingAdapter) onboardingInput(operation application.SubsystemAccessOperation, target application.SubsystemProvisioningTarget) application.SubsystemOnboardingInput {
	return application.SubsystemOnboardingInput{
		TenantID: operation.TenantID, OperatorID: operation.OperatorID, InitialAdminUserID: operation.InitialAdminID,
		ApplicationCode: operation.ApplicationCode, ApplicationName: target.ApplicationName, Environment: operation.EnvironmentCode,
		PublicBaseURL: strings.TrimSpace(operation.PublicOrigin), UpstreamURL: target.UpstreamURL,
		PathPrefix: target.PathPrefix, ClientType: target.ClientType,
		IssuerAlias: optionalAccessString(operation.AuthProvider), AllowedServiceBindings: target.AllowedServiceBindings,
	}
}

// resolveTarget 从清单解析目标；未命中返回 ok=false（调用方据此判定"范围外"，不是错误）。
func (adapter *SubsystemAccessOnboardingAdapter) resolveTarget(ctx context.Context, operation application.SubsystemAccessOperation) (application.SubsystemProvisioningTarget, bool, error) {
	capabilities, err := adapter.catalog.AvailableCapabilities(ctx)
	if err != nil {
		return application.SubsystemProvisioningTarget{}, false, fmt.Errorf("read subsystem capability catalog: %w", err)
	}
	for _, target := range capabilities.Targets {
		if strings.EqualFold(strings.TrimSpace(target.ApplicationCode), strings.TrimSpace(operation.ApplicationCode)) &&
			strings.EqualFold(strings.TrimSpace(target.Environment), strings.TrimSpace(operation.EnvironmentCode)) {
			return target, true, nil
		}
	}
	return application.SubsystemProvisioningTarget{}, false, nil
}

func optionalAccessString(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
