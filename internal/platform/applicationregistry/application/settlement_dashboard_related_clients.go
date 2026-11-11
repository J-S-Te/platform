package application

import (
	"context"
	"errors"
	"fmt"
)

// resolveSettlementDashboardSource 读取结算看板的独立数析来源公开身份。
// 参数：ctx控制查询，tenant和environment限定结算所在租户与环境。
// 返回值：来源尚未安装或凭据尚未创建时为空；读取失败或身份漂移时返回错误。
func (resolver *RelatedServiceClientsResolver) resolveSettlementDashboardSource(ctx context.Context, tenant, environment string) ([]RelatedSubsystemServiceClient, error) {
	app, env, err := resolver.registry.ResolveApplicationEnvironment(ctx, tenant, "data_analysis", environment)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelatedServiceClientUnavailable, err)
	}
	if app == "" || env == "" {
		return nil, ErrRelatedServiceClientUnavailable
	}
	clients, err := resolver.clients.ListOAuthClients(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelatedServiceClientUnavailable, err)
	}
	var found *OAuthClientView
	for index := range clients {
		client := &clients[index]
		if client.ClientID != "data_analysis-"+environment+"-settlement-dashboard" {
			continue
		}
		if found != nil {
			return nil, ErrRelatedServiceClientUnavailable
		}
		found = client
	}
	if found == nil {
		return nil, nil
	}
	c := *found
	if c.ID == "" || c.TenantID != tenant || c.ApplicationID != app || c.EnvironmentID != env ||
		c.Status != "ACTIVE" || c.ClientType != "service" || c.TokenAuthMethod != "client_secret_basic" ||
		!sameStringSetValues(c.GrantTypes, []string{"client_credentials"}) ||
		!sameStringSetValues(c.Scopes, []string{"dashboard.settlement.read"}) {
		return nil, ErrRelatedServiceClientUnavailable
	}
	return []RelatedSubsystemServiceClient{{ApplicationCode: "data_analysis", ApplicationID: app,
		EnvironmentID: env, Environment: environment, Purpose: ServiceCredentialSettlementDashboardRead, Client: c}}, nil
}
