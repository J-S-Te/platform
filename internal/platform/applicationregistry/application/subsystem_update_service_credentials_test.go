package application

import (
	"context"
	"errors"
	"testing"
)

type updateCredentialPortStub struct {
	created      OAuthClientCreateResult
	createCalls  int
	lastCreate   OAuthClientCreateInput
	clients      []OAuthClientView
	secretResult OAuthClientSecretResult
	repairResult OAuthClientView
	scopeCalls   int
	secretCalls  int
	lastScopes   OAuthClientScopesUpdateInput
	lastSecret   OAuthClientSecretCreateInput
	secretErr    error
	repairErr    error
	listErr      error
}

func (stub *updateCredentialPortStub) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	return stub.clients, stub.listErr
}

func (stub *updateCredentialPortStub) ReplaceOAuthClientScopes(_ context.Context, input OAuthClientScopesUpdateInput) (OAuthClientView, error) {
	stub.scopeCalls++
	stub.lastScopes = input
	if stub.repairErr != nil {
		return OAuthClientView{}, stub.repairErr
	}
	return stub.repairResult, nil
}

func (stub *updateCredentialPortStub) CreateOAuthClient(_ context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	stub.createCalls++
	stub.lastCreate = input
	return stub.created, nil
}

func (stub *updateCredentialPortStub) CreateOAuthClientSecret(_ context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	stub.secretCalls++
	stub.lastSecret = input
	if stub.secretErr != nil {
		return OAuthClientSecretResult{}, stub.secretErr
	}
	return stub.secretResult, nil
}

func updateCredentialClient() OAuthClientView {
	return OAuthClientView{
		ID: "01CLI000000000000000000001", ClientID: "settlement-dev-audit", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic",
		GrantTypes: []string{"client_credentials"}, Scopes: []string{"settlement.contract.ingest"}, Version: 7,
	}
}

func updateCredentialRequest(operation string, rotate bool) SubsystemUpdateServiceCredentialRequest {
	return SubsystemUpdateServiceCredentialRequest{
		TenantID: "tenant-1", OperatorID: "operator-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		ApplicationCode: "settlement", Environment: "dev", Operation: operation,
		Requirements: []SubsystemUpdateServiceCredentialRequirement{{
			Purpose: ServiceCredentialSettlementContractIngest, Suffix: "audit", ClientName: "合同结算事件投递器", Rotate: rotate,
			Scopes: []string{"settlement.contract.ingest"},
		}},
	}
}

func updateCredentialServiceFor(t *testing.T, stub *updateCredentialPortStub) *SubsystemUpdateServiceCredentialsService {
	t.Helper()
	service, err := NewSubsystemUpdateServiceCredentialsService(stub)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// 普通更新且范围一致：不写任何东西，只回传既有客户端（不含明文）。
func TestSubsystemUpdateServiceCredentialsKeepsExisting(t *testing.T) {
	stub := &updateCredentialPortStub{clients: []OAuthClientView{updateCredentialClient()}}
	service := updateCredentialServiceFor(t, stub)
	credentials, err := service.Ensure(context.Background(), updateCredentialRequest("UPDATE", false))
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
	if stub.scopeCalls != 0 || stub.secretCalls != 0 {
		t.Fatalf("no writes expected: scopes=%d secrets=%d", stub.scopeCalls, stub.secretCalls)
	}
	if credentials[0].PlaintextSecret != "" || credentials[0].OAuthClient.ClientID != "settlement-dev-audit" {
		t.Fatalf("credential = %+v", credentials[0])
	}
}

// 范围漂移：先整体替换范围，再按轮换规则决定是否签发密钥；严格用途需带期望版本号。
func TestSubsystemUpdateServiceCredentialsRepairsScopesThenRotates(t *testing.T) {
	client := updateCredentialClient()
	// 制造范围漂移：既有客户端缺少文件下载范围，触发整体替换。
	client.Scopes = []string{"settlement.contract.read"}
	stub := &updateCredentialPortStub{
		clients:      []OAuthClientView{client},
		repairResult: func() OAuthClientView { repaired := client; repaired.Version = 8; return repaired }(),
		secretResult: OAuthClientSecretResult{PlaintextSecret: "fresh-secret"},
	}
	service := updateCredentialServiceFor(t, stub)
	credentials, err := service.Ensure(context.Background(), updateCredentialRequest("ADOPT", false))
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
	if stub.scopeCalls != 1 || stub.lastScopes.OAuthClientID != client.ID || len(stub.lastScopes.Scopes) != 1 {
		t.Fatalf("scope repair = %+v calls=%d", stub.lastScopes, stub.scopeCalls)
	}
	if stub.secretCalls != 1 {
		t.Fatalf("ADOPT must rotate the secret, calls=%d", stub.secretCalls)
	}
	// 期望版本号必须是**修复后**的版本（8），否则会出现"替换范围后仍按旧版本签发"的丢失更新。
	if stub.lastSecret.ExpectedClientVersion == nil || *stub.lastSecret.ExpectedClientVersion != 8 {
		t.Fatalf("strict purpose must carry the repaired version: %+v", stub.lastSecret.ExpectedClientVersion)
	}
	if credentials[0].PlaintextSecret != "fresh-secret" {
		t.Fatalf("plaintext must be returned for delivery: %+v", credentials[0])
	}
}

// 既有客户端缺失：按既有字段创建并交付明文（与既有实现一致）。
func TestSubsystemUpdateServiceCredentialsCreatesMissingClient(t *testing.T) {
	stub := &updateCredentialPortStub{created: OAuthClientCreateResult{
		Client:          OAuthClientView{ID: "01CLI000000000000000000009", ClientID: "settlement-dev-audit"},
		PlaintextSecret: "created-secret",
	}}
	service := updateCredentialServiceFor(t, stub)
	credentials, err := service.Ensure(context.Background(), updateCredentialRequest("UPDATE", false))
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials=%+v err=%v", credentials, err)
	}
	if credentials[0].PlaintextSecret != "created-secret" {
		t.Fatalf("created credential must deliver the plaintext: %+v", credentials[0])
	}
	input := stub.lastCreate
	if stub.createCalls != 1 || input.ClientID != "settlement-dev-audit" ||
		input.ClientName != "合同结算事件投递器" || input.ClientType != "service" ||
		input.TokenAuthMethod != "client_secret_basic" || input.AccessTokenTTLSeconds != 15*60 ||
		len(input.GrantTypes) != 1 || input.GrantTypes[0] != "client_credentials" ||
		len(input.Scopes) != 1 || input.Scopes[0] != "settlement.contract.ingest" {
		t.Fatalf("creation input = %+v", input)
	}
}

// 严格用途的元数据漂移：拒绝且不产生任何写入。
func TestSubsystemUpdateServiceCredentialsRejectsDriftedClient(t *testing.T) {
	client := updateCredentialClient()
	client.Status = "DISABLED"
	stub := &updateCredentialPortStub{clients: []OAuthClientView{client}}
	service := updateCredentialServiceFor(t, stub)
	if _, err := service.Ensure(context.Background(), updateCredentialRequest("UPDATE", false)); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("drifted client err = %v, want invalid", err)
	}
	if stub.scopeCalls != 0 || stub.secretCalls != 0 {
		t.Fatalf("drifted client must not trigger writes: scopes=%d secrets=%d", stub.scopeCalls, stub.secretCalls)
	}
}
