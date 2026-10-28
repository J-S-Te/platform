package http

import (
	"context"
	"errors"
	"fmt"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

var errRelatedContractDependency = errors.New("registered contract ingest dependency is unavailable")
var errRelatedSettlementReferenceDependency = errors.New("registered settlement reference dependency is unavailable")

// p1ServicePurpose 标识必须核验登记身份的集成用途。
// 参数：purpose 为机器凭据的用途。
// 返回值：是否启用严格身份校验及版本保护。
func p1ServicePurpose(purpose string) bool {
	switch purpose {
	case application.ServiceCredentialSettlementProjectReferenceRead,
		application.ServiceCredentialSettlementContractIngest,
		application.ServiceCredentialPortalReportFileRead,
		application.ServiceCredentialPortalReportDownloadIdentityRead:
		return true
	default:
		return false
	}
}

// relatedServiceClients 解析接收应用的已登记来源身份，不接触来源密钥。
// 参数：ctx 控制请求，tenant、app和environment限定接收环境。
// 返回值：已验证的同租户来源身份；缺依赖或身份漂移返回错误。
func (handler *SubsystemOnboardingHandler) relatedServiceClients(ctx context.Context, tenant, app, environment string) ([]application.RelatedSubsystemServiceClient, error) {
	if app != "settlement" && app != "project_management" {
		return nil, nil
	}
	dependencyErr := errRelatedContractDependency
	if app == "project_management" {
		dependencyErr = errRelatedSettlementReferenceDependency
	}
	if handler.serviceCredentials == nil {
		return nil, fmt.Errorf("%w: credential manager unavailable", dependencyErr)
	}
	resolver, err := application.NewRelatedServiceClientsResolver(handler.service, handler.serviceCredentials)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", dependencyErr, err)
	}
	clients, err := resolver.Resolve(ctx, tenant, app, environment)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", dependencyErr, err)
	}
	return clients, nil
}
