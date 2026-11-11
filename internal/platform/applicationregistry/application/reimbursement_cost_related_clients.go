package application

import (
	"context"
	"errors"
	"fmt"
)

// resolveReimbursementCostSource 查找报销候选入口的独立结算读取身份。
// 参数：ctx控制查询，tenant和environment限定接收端租户与环境。
// 返回值：已核验公开身份；未安装返回空，重复、漂移或查询失败返回错误。
func (resolver *RelatedServiceClientsResolver) resolveReimbursementCostSource(
	ctx context.Context, tenant, environment string,
) ([]RelatedSubsystemServiceClient, error) {
	app, env, err := resolver.registry.ResolveApplicationEnvironment(ctx, tenant, "settlement", environment)
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
		if client.ClientID != "settlement-"+environment+"-reimbursement-cost" {
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
		!sameStringSetValues(c.Scopes, []string{"reimbursement.cost_candidate.read"}) {
		return nil, ErrRelatedServiceClientUnavailable
	}
	return []RelatedSubsystemServiceClient{{ApplicationCode: "settlement", ApplicationID: app,
		EnvironmentID: env, Environment: environment,
		Purpose: ServiceCredentialSettlementReimbursementCostRead, Client: c}}, nil
}
