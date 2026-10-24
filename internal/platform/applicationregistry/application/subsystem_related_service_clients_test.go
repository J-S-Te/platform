package application

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type relatedRegistryStub struct {
	applicationID string
	environmentID string
	err           error
	calls         int
}

func (stub *relatedRegistryStub) ResolveApplicationEnvironment(context.Context, string, string, string) (string, string, error) {
	stub.calls++
	return stub.applicationID, stub.environmentID, stub.err
}

type relatedClientListerStub struct {
	clients []OAuthClientView
	err     error
}

func (stub relatedClientListerStub) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	return stub.clients, stub.err
}

func relatedResolverFor(t *testing.T, registry *relatedRegistryStub, clients relatedClientListerStub) *RelatedServiceClientsResolver {
	t.Helper()
	resolver, err := NewRelatedServiceClientsResolver(registry, clients)
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func canonicalRelatedClient() OAuthClientView {
	return OAuthClientView{
		ID: "01CLI000000000000000000001", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000002", EnvironmentID: "01ENV000000000000000000001",
		ClientID: "contract_management-dev-settlement-integration", Status: "ACTIVE", ClientType: "service",
		TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"},
		Scopes: []string{"settlement.contract.ingest"},
	}
}

// 非结算应用不需要相关客户端：不查库、不报错。
func TestRelatedServiceClientsResolverSkipsOtherApplications(t *testing.T) {
	registry := &relatedRegistryStub{}
	resolver := relatedResolverFor(t, registry, relatedClientListerStub{})
	clients, err := resolver.Resolve(context.Background(), "tenant-1", "contract_management", "dev")
	if err != nil || clients != nil || registry.calls != 0 {
		t.Fatalf("clients=%v err=%v calls=%d", clients, err, registry.calls)
	}
}

// 结算应用：命中原生集成客户端时返回完整元数据。
func TestRelatedServiceClientsResolverReturnsCanonicalClient(t *testing.T) {
	registry := &relatedRegistryStub{applicationID: "01APP000000000000000000002", environmentID: "01ENV000000000000000000001"}
	resolver := relatedResolverFor(t, registry, relatedClientListerStub{clients: []OAuthClientView{canonicalRelatedClient()}})
	clients, err := resolver.Resolve(context.Background(), "tenant-1", "settlement", "dev")
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients=%+v err=%v", clients, err)
	}
	item := clients[0]
	if item.ApplicationCode != "contract_management" || item.Purpose != ServiceCredentialSettlementContractIngest ||
		item.ApplicationID != "01APP000000000000000000002" || item.EnvironmentID != "01ENV000000000000000000001" ||
		item.Environment != "dev" || item.Client.ClientID != "contract_management-dev-settlement-integration" {
		t.Fatalf("related client = %+v", item)
	}
}

// 校验规则（逐条与既有实现一致）：缺失、重复、类型/状态/授权类型/范围不符都必须拒绝。
func TestRelatedServiceClientsResolverRejectsUnavailableOrDriftedClients(t *testing.T) {
	mutations := map[string]func(*OAuthClientView){
		"missing":           nil,
		"inactive":          func(c *OAuthClientView) { c.Status = "DISABLED" },
		"wrong client type": func(c *OAuthClientView) { c.ClientType = "confidential" },
		"wrong auth method": func(c *OAuthClientView) { c.TokenAuthMethod = "client_secret_post" },
		"wrong grant types": func(c *OAuthClientView) { c.GrantTypes = []string{"authorization_code"} },
		"wrong scopes":      func(c *OAuthClientView) { c.Scopes = []string{"settlement.contract.read"} },
		"foreign tenant":    func(c *OAuthClientView) { c.TenantID = "tenant-2" },
		"foreign app":       func(c *OAuthClientView) { c.ApplicationID = "01APP000000000000000000009" },
		"missing id":        func(c *OAuthClientView) { c.ID = "" },
	}
	for name, mutate := range mutations {
		registry := &relatedRegistryStub{applicationID: "01APP000000000000000000002", environmentID: "01ENV000000000000000000001"}
		clients := []OAuthClientView{}
		if mutate != nil {
			client := canonicalRelatedClient()
			mutate(&client)
			clients = append(clients, client)
		}
		resolver := relatedResolverFor(t, registry, relatedClientListerStub{clients: clients})
		if _, err := resolver.Resolve(context.Background(), "tenant-1", "settlement", "dev"); !errors.Is(err, ErrRelatedServiceClientUnavailable) {
			t.Fatalf("%s: err = %v, want ErrRelatedServiceClientUnavailable", name, err)
		}
	}

	// 同名重复：拒绝（不使用"第一个"）。
	duplicate := canonicalRelatedClient()
	second := canonicalRelatedClient()
	second.ID = "01CLI000000000000000000002"
	resolver := relatedResolverFor(t, &relatedRegistryStub{applicationID: "01APP000000000000000000002", environmentID: "01ENV000000000000000000001"},
		relatedClientListerStub{clients: []OAuthClientView{duplicate, second}})
	if _, err := resolver.Resolve(context.Background(), "tenant-1", "settlement", "dev"); !errors.Is(err, ErrRelatedServiceClientUnavailable) {
		t.Fatalf("duplicate client id: err = %v", err)
	}

	// 依赖读取失败：同样包装为"相关客户端不可用"。
	failing := relatedResolverFor(t, &relatedRegistryStub{err: errors.New("database unavailable")}, relatedClientListerStub{})
	if _, err := failing.Resolve(context.Background(), "tenant-1", "settlement", "dev"); !errors.Is(err, ErrRelatedServiceClientUnavailable) ||
		!strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("registry failure err = %v", err)
	}
}

// 多重集比较：顺序无关、重复计数敏感（与既有 sameStringSet 语义一致）。
func TestSameStringSetValuesSemantics(t *testing.T) {
	if !sameStringSetValues([]string{"client_credentials"}, []string{"client_credentials"}) {
		t.Fatal("identical sets must match")
	}
	if sameStringSetValues([]string{"a", "b"}, []string{"a"}) || sameStringSetValues([]string{"a"}, []string{"a", "a"}) {
		t.Fatal("different cardinality must not match")
	}
	if !sameStringSetValues([]string{"a", "a", "b"}, []string{"b", "a", "a"}) {
		t.Fatal("order must not matter")
	}
}
