package application

import (
	"context"
	"errors"
)

// resolveReimbursementSources 为项目接收端读取同租户同环境两个专用公开身份。
// 参数：ctx为查询上下文，tenant/environment为接收边界。
// 返回值：未接入报销时为空；部分缺失、重复或scope/身份漂移时失败关闭。
func (resolver *RelatedServiceClientsResolver) resolveReimbursementSources(ctx context.Context, tenant, environment string) ([]RelatedSubsystemServiceClient, error) {
	app, env, err := resolver.registry.ResolveApplicationEnvironment(ctx, tenant, "reimbursement", environment)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if app == "" || env == "" {
		return nil, nil
	}
	clients, err := resolver.clients.ListOAuthClients(ctx, tenant)
	if err != nil {
		return nil, err
	}
	result := []RelatedSubsystemServiceClient{}
	for _, binding := range []struct{ purpose, suffix, scope string }{
		{ServiceCredentialReimbursementProjectReferenceRead, "project-reference", "reimbursement.project_reference.read"},
		{ServiceCredentialProjectReimbursementCostWrite, "project-cost-writer", "project.reimbursement_cost.write"},
	} {
		var matches []OAuthClientView
		for _, client := range clients {
			if client.ClientID == "reimbursement-"+environment+"-"+binding.suffix {
				matches = append(matches, client)
			}
		}
		if len(matches) == 0 {
			continue
		}
		if len(matches) != 1 {
			return nil, ErrRelatedServiceClientUnavailable
		}
		c := matches[0]
		if c.ID == "" || c.TenantID != tenant || c.ApplicationID != app || c.EnvironmentID != env || c.Status != "ACTIVE" || c.ClientType != "service" || c.TokenAuthMethod != "client_secret_basic" || !sameStringSetValues(c.GrantTypes, []string{"client_credentials"}) || !sameStringSetValues(c.Scopes, []string{binding.scope}) {
			return nil, ErrRelatedServiceClientUnavailable
		}
		result = append(result, RelatedSubsystemServiceClient{ApplicationCode: "reimbursement", ApplicationID: app, EnvironmentID: env, Environment: environment, Purpose: binding.purpose, Client: c})
	}
	if len(result) == 1 {
		return nil, ErrRelatedServiceClientUnavailable
	}
	return result, nil
}
