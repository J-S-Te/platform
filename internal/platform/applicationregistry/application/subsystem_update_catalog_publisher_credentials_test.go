package application

import (
	"context"
	"errors"
	"testing"
)

type catalogPublisherPortStub struct {
	clients     []OAuthClientView
	secret      OAuthClientSecretResult
	created     OAuthClientCreateResult
	secretCalls int
	createCalls int
	lastCreate  OAuthClientCreateInput
}

func (stub *catalogPublisherPortStub) ListOAuthClients(context.Context, string) ([]OAuthClientView, error) {
	return stub.clients, nil
}

func (stub *catalogPublisherPortStub) CreateOAuthClientSecret(context.Context, OAuthClientSecretCreateInput) (OAuthClientSecretResult, error) {
	stub.secretCalls++
	return stub.secret, nil
}

func (stub *catalogPublisherPortStub) CreateOAuthClient(_ context.Context, input OAuthClientCreateInput) (OAuthClientCreateResult, error) {
	stub.createCalls++
	stub.lastCreate = input
	return stub.created, nil
}

func catalogPublisherServiceFor(t *testing.T, stub *catalogPublisherPortStub) *SubsystemUpdateCatalogPublisherCredentialsService {
	t.Helper()
	service, err := NewSubsystemUpdateCatalogPublisherCredentialsService(stub)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func catalogPublisherClientFixture() OAuthClientView {
	return OAuthClientView{
		ID: "01CLI000000000000000000003", ClientID: "settlement-dev-catalog-publisher", TenantID: "tenant-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001", Status: "ACTIVE",
	}
}

// 已存在且合规：总是签发新密钥（密钥只写不可回读）。
func TestSubsystemUpdateCatalogPublisherCredentialsRotatesExisting(t *testing.T) {
	stub := &catalogPublisherPortStub{clients: []OAuthClientView{catalogPublisherClientFixture()}, secret: OAuthClientSecretResult{PlaintextSecret: "publisher-secret"}}
	credential, err := catalogPublisherServiceFor(t, stub).Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if credential.ClientID != "settlement-dev-catalog-publisher" || credential.ClientSecret != "publisher-secret" {
		t.Fatalf("credential = %+v", credential)
	}
	if stub.secretCalls != 1 || stub.createCalls != 0 {
		t.Fatalf("secret=%d create=%d", stub.secretCalls, stub.createCalls)
	}
}

// 不存在：按既有字段创建（字段逐条断言）。
func TestSubsystemUpdateCatalogPublisherCredentialsCreatesMissing(t *testing.T) {
	stub := &catalogPublisherPortStub{created: OAuthClientCreateResult{
		Client:          OAuthClientView{ID: "01CLI000000000000000000004", ClientID: "settlement-dev-catalog-publisher"},
		PlaintextSecret: "created-secret",
	}}
	credential, err := catalogPublisherServiceFor(t, stub).Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if credential.ClientSecret != "created-secret" {
		t.Fatalf("credential = %+v", credential)
	}
	input := stub.lastCreate
	if stub.createCalls != 1 || input.ClientID != "settlement-dev-catalog-publisher" ||
		input.ClientName != "settlement Authorization Catalog Publisher" || input.ClientType != "service" ||
		input.TokenAuthMethod != "client_secret_basic" || input.AccessTokenTTLSeconds != 15*60 ||
		len(input.GrantTypes) != 1 || input.GrantTypes[0] != "client_credentials" ||
		len(input.Scopes) != 1 || input.Scopes[0] != "authorization.catalog.sync" {
		t.Fatalf("creation input = %+v", input)
	}
}

// 状态或归属漂移：拒绝且零写入；缺失必填：校验错误。
func TestSubsystemUpdateCatalogPublisherCredentialsRejectsDrift(t *testing.T) {
	disabled := catalogPublisherClientFixture()
	disabled.Status = "DISABLED"
	stub := &catalogPublisherPortStub{clients: []OAuthClientView{disabled}}
	if _, err := catalogPublisherServiceFor(t, stub).Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev"); !errors.Is(err, ErrSubsystemUpdateServiceClientInvalid) {
		t.Fatalf("disabled client err = %v", err)
	}
	if stub.secretCalls != 0 || stub.createCalls != 0 {
		t.Fatalf("drifted client must not write: secret=%d create=%d", stub.secretCalls, stub.createCalls)
	}

	if _, err := catalogPublisherServiceFor(t, &catalogPublisherPortStub{}).Ensure(context.Background(), "", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev"); !errors.Is(err, ErrValidation) {
		t.Fatalf("missing tenant err = %v", err)
	}
}
