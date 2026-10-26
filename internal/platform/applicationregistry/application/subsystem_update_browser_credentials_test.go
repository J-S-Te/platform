package application

import (
	"context"
	"errors"
	"testing"
)

type browserCredentialPortStub struct {
	created     OAuthClientCreateResult
	createCalls int
	lastCreate  OAuthClientCreateInput
	clients     []OAuthClientView
	secret      OAuthClientSecretResult
	secretErr   error
	secretCalls int
	lastSecret  OAuthClientSecretCreateInput
}

func (stub *browserCredentialPortStub) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	return stub.clients, nil
}

func (stub *browserCredentialPortStub) CreateOAuthClient(_ context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	stub.createCalls++
	stub.lastCreate = input
	return stub.created, nil
}

func (stub *browserCredentialPortStub) CreateOAuthClientSecret(_ context.Context, input OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	stub.secretCalls++
	stub.lastSecret = input
	if stub.secretErr != nil {
		return OAuthClientSecretResult{}, stub.secretErr
	}
	return stub.secret, nil
}

func browserClientFixture() OAuthClientView {
	return OAuthClientView{
		ID: "01CLI000000000000000000002", ClientID: "settlement-dev-web", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		Status: "ACTIVE", ClientType: "confidential", TokenAuthMethod: "client_secret_basic",
		GrantTypes: []string{"authorization_code", "refresh_token"}, Scopes: []string{"openid"}, Version: 3,
		RequirePKCE: true, RedirectURIs: []string{"https://settlement.example.com/settlement/auth/callback"},
	}
}

func browserCredentialServiceFor(t *testing.T, stub *browserCredentialPortStub) *SubsystemUpdateBrowserCredentialsService {
	t.Helper()
	service, err := NewSubsystemUpdateBrowserCredentialsService(stub)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// 规范名命中且元数据合规：签发新密钥并带期望版本号。
func TestSubsystemUpdateBrowserCredentialsIssuesSecret(t *testing.T) {
	stub := &browserCredentialPortStub{clients: []OAuthClientView{browserClientFixture()}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	service := browserCredentialServiceFor(t, stub)
	credential, err := service.Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "https://settlement.example.com/settlement/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if credential.ClientID != "settlement-dev-web" || credential.ClientSecret != "browser-secret" {
		t.Fatalf("credential = %+v", credential)
	}
	if stub.secretCalls != 1 || stub.lastSecret.ExpectedClientVersion == nil || *stub.lastSecret.ExpectedClientVersion != 3 {
		t.Fatalf("secret call = %+v calls=%d", stub.lastSecret, stub.secretCalls)
	}
}

// 客户端缺失 → 未接线；元数据漂移 → 拒绝且零写入；同名重复 → 拒绝。
func TestSubsystemUpdateBrowserCredentialsRejectsMissingOrDrifted(t *testing.T) {
	// 缺失：按既有字段创建（而不是报未接线）。
	missingStub := &browserCredentialPortStub{created: OAuthClientCreateResult{
		Client:          OAuthClientView{ID: "01CLI000000000000000000010", ClientID: "settlement-dev-web"},
		PlaintextSecret: "created-browser-secret",
	}}
	created, err := browserCredentialServiceFor(t, missingStub).Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "https://settlement.example.com/settlement/auth/callback")
	if err != nil || created.ClientSecret != "created-browser-secret" {
		t.Fatalf("created browser credential = %+v err=%v", created, err)
	}
	input := missingStub.lastCreate
	if missingStub.createCalls != 1 || input.ClientID != "settlement-dev-web" || input.ClientName != "settlement Web" ||
		input.ClientType != "confidential" || !input.RequirePKCE || input.AccessTokenTTLSeconds != 15*60 ||
		len(input.Scopes) != 2 || len(input.RedirectURIs) != 1 || input.RedirectURIs[0] != "https://settlement.example.com/settlement/auth/callback" {
		t.Fatalf("browser creation input = %+v", input)
	}

	drifted := browserClientFixture()
	drifted.GrantTypes = []string{"authorization_code"}
	stub := &browserCredentialPortStub{clients: []OAuthClientView{drifted}}
	if _, err := browserCredentialServiceFor(t, stub).Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "https://settlement.example.com/settlement/auth/callback"); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("drifted client err = %v", err)
	}
	if stub.secretCalls != 0 {
		t.Fatalf("drifted client must not trigger writes: %d", stub.secretCalls)
	}

	duplicate := &browserCredentialPortStub{clients: []OAuthClientView{browserClientFixture(), browserClientFixture()}}
	if _, err := browserCredentialServiceFor(t, duplicate).Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "https://settlement.example.com/settlement/auth/callback"); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("duplicate client err = %v", err)
	}
}

func TestSubsystemAccessBrowserReferenceIsReadOnly(t *testing.T) {
	for _, invalid := range []string{"", "missing", "tenant", "disabled", "duplicate"} {
		t.Run(invalid, func(t *testing.T) {
			client := browserClientFixture()
			clients := []OAuthClientView{client}
			switch invalid {
			case "missing":
				clients = nil
			case "tenant":
				clients[0].TenantID = "other"
			case "disabled":
				clients[0].Status = "DISABLED"
			case "duplicate":
				clients = append(clients, client)
			}
			stub := &browserCredentialPortStub{clients: clients}
			id, err := browserCredentialServiceFor(t, stub).ReadSubsystemAccessBrowserClientID(context.Background(), "tenant-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev")
			if invalid == "" {
				if err != nil || id != client.ClientID {
					t.Fatalf("id=%q err=%v", id, err)
				}
			} else if err == nil {
				t.Fatal("invalid reference accepted")
			}
			if stub.secretCalls != 0 || stub.createCalls != 0 {
				t.Fatal("read-only recovery wrote credentials")
			}
		})
	}
}

func TestSubsystemAccessRecoversNewlyCreatedBrowserClient(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{created: OAuthClientCreateResult{Client: client, PlaintextSecret: "one-time-secret"}}
	service := browserCredentialServiceFor(t, stub)
	_, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0])
	if err != nil {
		t.Fatal(err)
	}
	client.GrantTypes = stub.lastCreate.GrantTypes
	stub.clients = []OAuthClientView{client}
	id, err := service.ReadSubsystemAccessBrowserClientID(context.Background(), client.TenantID, client.ApplicationID, client.EnvironmentID, "settlement", "dev")
	if err != nil || id != client.ClientID || stub.secretCalls != 0 || stub.createCalls != 1 {
		t.Fatalf("id=%q err=%v create=%d secrets=%d", id, err, stub.createCalls, stub.secretCalls)
	}
}
