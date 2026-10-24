package http

import (
	"context"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// ResolveSubsystemRelatedServiceClients 把既有的相关服务客户端解析（settlement ↔ contract_management 集成）
// 暴露为端口，供统一接入的执行器/部署适配器复用：**不搬移逻辑、不改变校验**，仅转调既有实现。
// 调用方不得据此获得任何明文凭据。
func (handler *SubsystemOnboardingHandler) ResolveSubsystemRelatedServiceClients(ctx context.Context, tenantID, applicationCode, environmentCode string) ([]application.RelatedSubsystemServiceClient, error) {
	return handler.relatedServiceClients(ctx, tenantID, applicationCode, environmentCode)
}

// ResolveSubsystemAccessIssuer 把既有的 issuer 解析（按认证提供方别名取平台配置的 issuer）
// 暴露为端口：统一入口不自行推断 issuer，保持与旧路径同一来源。
func (handler *SubsystemOnboardingHandler) ResolveSubsystemAccessIssuer(_ context.Context, authProvider string) (string, error) {
	return handler.issuerForAlias(authProvider)
}
