package http

import (
	"context"
	"errors"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

var errRelatedContractDependency = errors.New("registered contract ingest dependency is unavailable")

func p1ServicePurpose(purpose string) bool {
	return purpose == application.ServiceCredentialSettlementContractIngest || purpose == application.ServiceCredentialPortalReportFileRead || purpose == application.ServiceCredentialPortalReportDownloadIdentityRead
}

// relatedServiceClients 解析实际持久化的源客户端元数据。接收方绝不接触源 secret，
// 规范名仅作为查找键，不能当回退 ID 使用。
//
// 校验逻辑已提取到应用层 RelatedServiceClientsResolver（统一入口与旧路径共用，避免口径漂移）；
// 本方法只负责 handler 侧的依赖检查与错误包装，行为保持不变。
func (handler *SubsystemOnboardingHandler) relatedServiceClients(ctx context.Context, tenant, app, environment string) ([]application.RelatedSubsystemServiceClient, error) {
	if app != "settlement" {
		return nil, nil
	}
	if handler.serviceCredentials == nil {
		return nil, fmt.Errorf("%w: credential manager unavailable", errRelatedContractDependency)
	}
	resolver, err := application.NewRelatedServiceClientsResolver(handler.service, handler.serviceCredentials)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRelatedContractDependency, err)
	}
	clients, err := resolver.Resolve(ctx, tenant, app, environment)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRelatedContractDependency, err)
	}
	return clients, nil
}
