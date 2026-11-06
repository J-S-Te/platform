package http

import (
	"context"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// relatedServiceResolutionPolicy 从实际部署Agent读取精确结算模式，绝不采信请求或context声明。
// 参数：ctx为操作期限，app/environment为控制面已校验接收目标。
// 返回值：窄模式策略；旧Agent未实现投影时保持严格，读取失败返回错误。
func (handler *SubsystemOnboardingHandler) relatedServiceResolutionPolicy(ctx context.Context, app, environment string) (application.SubsystemRelatedServicePolicy, error) {
	if app != "settlement" {
		return application.SubsystemRelatedServicePolicy{}, nil
	}
	provider, ok := handler.provisioner.(interface {
		AvailableCapabilities(context.Context) (application.SubsystemProvisioningCapabilities, error)
	})
	if !ok {
		return application.SubsystemRelatedServicePolicy{}, nil
	}
	capabilities, err := provider.AvailableCapabilities(ctx)
	if err != nil {
		return application.SubsystemRelatedServicePolicy{}, err
	}
	return application.RelatedServicePolicyFromCapabilities(capabilities, app, environment)
}
