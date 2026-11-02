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

// canonicalProjectReferenceClient 返回项目接收端所需的结算只读服务身份（未漂移）。
func canonicalProjectReferenceClient() OAuthClientView {
	return OAuthClientView{
		ID: "01CLI000000000000000000003", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000003", EnvironmentID: "01ENV000000000000000000001",
		ClientID: "settlement-dev-project-reference", Status: "ACTIVE", ClientType: "service",
		TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"},
		Scopes: []string{"settlement.project_reference.read"},
	}
}

// 项目先接入、结算后接入：结算来源或来源客户端缺失时返回 (nil, nil)，不阻断项目。
func TestRelatedServiceClientsResolverAllowsProjectReferenceBeforeSettlement(t *testing.T) {
	// 结算应用/环境尚未登记（既有目录统一返回 ErrNotFound）。
	unregistered := relatedResolverFor(t, &relatedRegistryStub{err: ErrNotFound}, relatedClientListerStub{})
	clients, err := unregistered.Resolve(context.Background(), "tenant-1", "project_management", "dev")
	if err != nil || clients != nil {
		t.Fatalf("unregistered settlement source: clients=%v err=%v", clients, err)
	}

	// 结算环境已登记但 project-reference 客户端尚未创建。
	missing := relatedResolverFor(t,
		&relatedRegistryStub{applicationID: "01APP000000000000000000003", environmentID: "01ENV000000000000000000001"},
		relatedClientListerStub{clients: []OAuthClientView{canonicalRelatedClient()}})
	clients, err = missing.Resolve(context.Background(), "tenant-1", "project_management", "dev")
	if err != nil || clients != nil {
		t.Fatalf("missing settlement project reference client: clients=%v err=%v", clients, err)
	}

	// 结算来源存在时仍按同一规则返回完整元数据。
	ready := relatedResolverFor(t,
		&relatedRegistryStub{applicationID: "01APP000000000000000000003", environmentID: "01ENV000000000000000000001"},
		relatedClientListerStub{clients: []OAuthClientView{canonicalProjectReferenceClient()}})
	clients, err = ready.Resolve(context.Background(), "tenant-1", "project_management", "dev")
	if err != nil || len(clients) != 1 {
		t.Fatalf("ready settlement project reference: clients=%+v err=%v", clients, err)
	}
	item := clients[0]
	if item.ApplicationCode != "settlement" || item.Purpose != ServiceCredentialSettlementProjectReferenceRead ||
		item.ApplicationID != "01APP000000000000000000003" || item.EnvironmentID != "01ENV000000000000000000001" ||
		item.Environment != "dev" || item.Client.ClientID != "settlement-dev-project-reference" {
		t.Fatalf("related client = %+v", item)
	}

	// 非"未登记"的读取失败仍是错误，不能伪装成"结算未接入"。
	failing := relatedResolverFor(t, &relatedRegistryStub{err: errors.New("database unavailable")}, relatedClientListerStub{})
	if _, err := failing.Resolve(context.Background(), "tenant-1", "project_management", "dev"); !errors.Is(err, ErrRelatedServiceClientUnavailable) {
		t.Fatalf("registry failure err = %v", err)
	}
}

// 结算应用的合同投递客户端缺失仍必须失败：该依赖不因项目解耦而放宽。
func TestRelatedServiceClientsResolverStillRequiresSettlementContractClient(t *testing.T) {
	resolver := relatedResolverFor(t,
		&relatedRegistryStub{applicationID: "01APP000000000000000000002", environmentID: "01ENV000000000000000000001"},
		relatedClientListerStub{})
	if _, err := resolver.Resolve(context.Background(), "tenant-1", "settlement", "dev"); !errors.Is(err, ErrRelatedServiceClientUnavailable) {
		t.Fatalf("missing settlement contract client: err = %v", err)
	}
}

// 项目来源客户端已存在但状态/授权范围漂移时必须失败，不能静默关闭或接受错误配置。
func TestRelatedServiceClientsResolverRejectsDriftedProjectReferenceClients(t *testing.T) {
	mutations := map[string]func(*OAuthClientView){
		"inactive":     func(c *OAuthClientView) { c.Status = "DISABLED" },
		"wrong scopes": func(c *OAuthClientView) { c.Scopes = []string{"settlement.contract.ingest"} },
		"extra scopes": func(c *OAuthClientView) { c.Scopes = append(c.Scopes, "dashboard.project.read") },
		"wrong tenant": func(c *OAuthClientView) { c.TenantID = "tenant-2" },
		"wrong app":    func(c *OAuthClientView) { c.ApplicationID = "01APP000000000000000000009" },
	}
	for name, mutate := range mutations {
		client := canonicalProjectReferenceClient()
		mutate(&client)
		resolver := relatedResolverFor(t,
			&relatedRegistryStub{applicationID: "01APP000000000000000000003", environmentID: "01ENV000000000000000000001"},
			relatedClientListerStub{clients: []OAuthClientView{client}})
		if _, err := resolver.Resolve(context.Background(), "tenant-1", "project_management", "dev"); !errors.Is(err, ErrRelatedServiceClientUnavailable) {
			t.Fatalf("%s: err = %v, want ErrRelatedServiceClientUnavailable", name, err)
		}
	}

	// 同名来源重复同样拒绝。
	first := canonicalProjectReferenceClient()
	second := canonicalProjectReferenceClient()
	second.ID = "01CLI000000000000000000004"
	resolver := relatedResolverFor(t,
		&relatedRegistryStub{applicationID: "01APP000000000000000000003", environmentID: "01ENV000000000000000000001"},
		relatedClientListerStub{clients: []OAuthClientView{first, second}})
	if _, err := resolver.Resolve(context.Background(), "tenant-1", "project_management", "dev"); !errors.Is(err, ErrRelatedServiceClientUnavailable) {
		t.Fatalf("duplicate project reference client: err = %v", err)
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
