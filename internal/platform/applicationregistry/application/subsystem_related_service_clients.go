package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRelatedServiceClientUnavailable 表示已登记的"相关集成"服务客户端不可用：
// 首次接入/受控更新把它作为部署输入依赖，缺失时必须失败而不是用空值继续。
var ErrRelatedServiceClientUnavailable = errors.New("registered related service client is unavailable")

// SubsystemRelatedApplicationRegistry 解析相关应用的环境 ID（既有目录服务实现）。
type SubsystemRelatedApplicationRegistry interface {
	ResolveApplicationEnvironment(ctx context.Context, tenantID, applicationCode, environment string) (string, string, error)
}

// SubsystemOAuthClientLister 列出 OAuth Client 元数据（既有凭据管理器实现；不含明文凭据）。
type SubsystemOAuthClientLister interface {
	ListOAuthClients(ctx context.Context, tenantID string) ([]OAuthClientView, error)
}

// RelatedServiceClientsResolver 解析"相关服务客户端"（当前为结算 → 合同目录发布的集成客户端）。
// 由既有实现提取而来：**校验规则逐条保持一致**，统一入口与旧路径共用，避免两套口径漂移。
type RelatedServiceClientsResolver struct {
	registry SubsystemRelatedApplicationRegistry
	clients  SubsystemOAuthClientLister
}

// NewRelatedServiceClientsResolver 构造解析器；依赖缺失即失败。
func NewRelatedServiceClientsResolver(registry SubsystemRelatedApplicationRegistry, clients SubsystemOAuthClientLister) (*RelatedServiceClientsResolver, error) {
	if registry == nil || clients == nil {
		return nil, errors.New("related service clients resolver requires registry and client lister")
	}
	return &RelatedServiceClientsResolver{registry: registry, clients: clients}, nil
}

// Resolve 读取接收应用所需的已登记来源身份，不读取来源密钥。
// 参数：ctx 控制读取，tenantID、applicationCode和environment限定接收环境。
// 返回值：同租户应用环境的最小权限身份；缺失或漂移返回错误。
func (resolver *RelatedServiceClientsResolver) Resolve(ctx context.Context, tenantID, applicationCode, environment string) ([]RelatedSubsystemServiceClient, error) {
	source, purpose, suffix, scope := "contract_management", ServiceCredentialSettlementContractIngest, "settlement-integration", "settlement.contract.ingest"
	switch strings.TrimSpace(applicationCode) {
	case "settlement":
	case "project_management":
		source, purpose, suffix, scope = "settlement", ServiceCredentialSettlementProjectReferenceRead, "project-reference", "settlement.project_reference.read"
	default:
		return nil, nil
	}
	applicationID, environmentID, err := resolver.registry.ResolveApplicationEnvironment(ctx, tenantID, source, environment)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelatedServiceClientUnavailable, err)
	}
	clients, err := resolver.clients.ListOAuthClients(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRelatedServiceClientUnavailable, err)
	}
	name := source + "-" + environment + "-" + suffix
	var found *OAuthClientView
	for index := range clients {
		client := clients[index]
		if client.ClientID != name {
			continue
		}
		if found != nil {
			return nil, ErrRelatedServiceClientUnavailable
		}
		found = &client
	}
	if found == nil {
		return nil, ErrRelatedServiceClientUnavailable
	}
	canonical := *found
	if canonical.ID == "" || applicationID == "" || environmentID == "" || canonical.TenantID != tenantID ||
		canonical.ApplicationID != applicationID || canonical.EnvironmentID != environmentID ||
		!strings.EqualFold(canonical.Status, "ACTIVE") || canonical.ClientType != "service" ||
		canonical.TokenAuthMethod != "client_secret_basic" ||
		!sameStringSetValues(canonical.GrantTypes, []string{"client_credentials"}) ||
		!sameStringSetValues(canonical.Scopes, []string{scope}) {
		return nil, ErrRelatedServiceClientUnavailable
	}
	return []RelatedSubsystemServiceClient{{
		ApplicationCode: source, ApplicationID: applicationID, Environment: environment,
		EnvironmentID: environmentID, Purpose: purpose, Client: canonical,
	}}, nil
}

// sameStringSetValues 判断两个字符串切片是否互为同一多重集（与既有实现一致）。
func sameStringSetValues(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]int, len(left))
	for _, value := range left {
		seen[value]++
	}
	for _, value := range right {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}
