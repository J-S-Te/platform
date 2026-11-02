package application

import (
	"context"
	"errors"
	"strings"
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

// backchannelLogoutPortStub 是存量回填端的替身：模拟已登记/未登记两种状态，
// 并记录写入，用于断言"只补缺失、不覆盖人工登记、幂等"。
type backchannelLogoutPortStub struct {
	registered map[string]string
	readErr    error
	upsertErr  error
	upserts    []string
	readCalls  int
}

func newBackchannelLogoutPortStub(registered map[string]string) *backchannelLogoutPortStub {
	if registered == nil {
		registered = map[string]string{}
	}
	return &backchannelLogoutPortStub{registered: registered}
}

func (stub *backchannelLogoutPortStub) ReadBackchannelLogoutURI(_ context.Context, _ string, oauthClientID string) (string, bool, error) {
	stub.readCalls++
	if stub.readErr != nil {
		return "", false, stub.readErr
	}
	uri, found := stub.registered[oauthClientID]
	return uri, found, nil
}

func (stub *backchannelLogoutPortStub) UpsertBackchannelLogoutURI(_ context.Context, tenantID, operatorID, oauthClientID, uri string) error {
	if stub.upsertErr != nil {
		return stub.upsertErr
	}
	stub.upserts = append(stub.upserts, tenantID+"|"+operatorID+"|"+oauthClientID+"|"+uri)
	stub.registered[oauthClientID] = uri
	return nil
}

func backfillBrowserCredentialService(t *testing.T, stub *browserCredentialPortStub, port *backchannelLogoutPortStub, options ...SubsystemUpdateBrowserCredentialsOption) *SubsystemUpdateBrowserCredentialsService {
	t.Helper()
	all := append([]SubsystemUpdateBrowserCredentialsOption{WithBackchannelLogoutBackfill(port)}, options...)
	service, err := NewSubsystemUpdateBrowserCredentialsService(stub, all...)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// 存量回填入口：更新路径在凭据就绪后按与接入一致的默认规则补登记登出端点。
func TestSubsystemUpdateBrowserCredentialsBackfillsBackchannelLogoutURI(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(nil)
	service := backfillBrowserCredentialService(t, stub, port)

	if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err != nil {
		t.Fatal(err)
	}
	want := "tenant-1|operator-1|01CLI000000000000000000002|https://settlement.example.com/settlement/auth/backchannel-logout"
	if len(port.upserts) != 1 || port.upserts[0] != want {
		t.Fatalf("backfill upserts = %v, want [%s]", port.upserts, want)
	}
}

// 幂等：第二次更新时已有登记，不得再次写入（也不会产生第二条登记行）。
func TestSubsystemUpdateBrowserCredentialsBackfillIsIdempotent(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(nil)
	service := backfillBrowserCredentialService(t, stub, port)

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if len(port.upserts) != 1 {
		t.Fatalf("idempotent backfill upserts = %d, want 1", len(port.upserts))
	}
	if port.readCalls != 2 {
		t.Fatalf("backfill reads = %d, want 2", port.readCalls)
	}
}

// 人工登记的定制端点不被默认路径覆盖：回填只补"完全没有登记"的客户端。
func TestSubsystemUpdateBrowserCredentialsBackfillKeepsManualRegistration(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(map[string]string{client.ID: "https://rp.example.com/custom/logout"})
	service := backfillBrowserCredentialService(t, stub, port)

	if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err != nil {
		t.Fatal(err)
	}
	if len(port.upserts) != 0 {
		t.Fatalf("manual registration overwritten: %v", port.upserts)
	}
	if port.registered[client.ID] != "https://rp.example.com/custom/logout" {
		t.Fatalf("registered URI = %q", port.registered[client.ID])
	}
}

// 显式 opt-out（空路径）：不写、不删既有登记（删除必须走带审计的受控流程）。
func TestSubsystemUpdateBrowserCredentialsBackfillRespectsOptOut(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(map[string]string{client.ID: "https://rp.example.com/custom/logout"})
	optedOut := ""
	service := backfillBrowserCredentialService(t, stub, port, WithBackchannelLogoutPath(&optedOut))

	if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err != nil {
		t.Fatal(err)
	}
	if len(port.upserts) != 0 || port.readCalls != 0 {
		t.Fatalf("opt-out touched registrations: upserts=%v reads=%d", port.upserts, port.readCalls)
	}
	if port.registered[client.ID] != "https://rp.example.com/custom/logout" {
		t.Fatalf("opt-out deleted existing registration: %q", port.registered[client.ID])
	}
}

// 自定义路径按接入规则归一化后使用。
func TestSubsystemUpdateBrowserCredentialsBackfillUsesConfiguredPath(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(nil)
	path := "auth/oidc/backchannel-logout/"
	service := backfillBrowserCredentialService(t, stub, port, WithBackchannelLogoutPath(&path))

	if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err != nil {
		t.Fatal(err)
	}
	if len(port.upserts) != 1 || !strings.HasSuffix(port.upserts[0], "|https://settlement.example.com/settlement/auth/oidc/backchannel-logout") {
		t.Fatalf("configured path upserts = %v", port.upserts)
	}
}

// HTTPS 校验不放宽：明文 HTTP 端点不写登记，也不因此阻断凭据重建。
func TestSubsystemUpdateBrowserCredentialsBackfillSkipsInsecureEndpoint(t *testing.T) {
	client := browserClientFixture()
	client.RedirectURIs = []string{"http://settlement.example.test/settlement/auth/callback"}
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(nil)
	service := backfillBrowserCredentialService(t, stub, port)

	if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err != nil {
		t.Fatal(err)
	}
	if len(port.upserts) != 0 || port.readCalls != 0 {
		t.Fatalf("insecure endpoint reached storage: upserts=%v reads=%d", port.upserts, port.readCalls)
	}
}

// 写失败必须向上返回：不允许"凭据已下发、登出登记静默丢失"。
func TestSubsystemUpdateBrowserCredentialsBackfillWriteFailureFailsUpdate(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(nil)
	port.upsertErr = errors.New("registration unavailable")
	service := backfillBrowserCredentialService(t, stub, port)

	if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err == nil {
		t.Fatal("backfill write failure was swallowed")
	}
}

// 读失败同样失败关闭，不能把"读不到登记"当成"没有登记"去覆盖。
func TestSubsystemUpdateBrowserCredentialsBackfillReadFailureFailsUpdate(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	port := newBackchannelLogoutPortStub(nil)
	port.readErr = errors.New("database unavailable")
	service := backfillBrowserCredentialService(t, stub, port)

	if _, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0]); err == nil {
		t.Fatal("backfill read failure was swallowed")
	}
}

// 未接线（未注入回填端）保持既有语义：只重建凭据，不触达任何登记。
func TestSubsystemUpdateBrowserCredentialsWithoutBackfillKeepsExistingBehavior(t *testing.T) {
	client := browserClientFixture()
	stub := &browserCredentialPortStub{clients: []OAuthClientView{client}, secret: OAuthClientSecretResult{PlaintextSecret: "browser-secret"}}
	service := browserCredentialServiceFor(t, stub)

	credential, err := service.Ensure(context.Background(), client.TenantID, "operator-1", client.ApplicationID, client.EnvironmentID, "settlement", "dev", client.RedirectURIs[0])
	if err != nil || credential.ClientSecret != "browser-secret" {
		t.Fatalf("credential = %+v err=%v", credential, err)
	}
}

// 新建客户端分支同样补登记：否则新接入/修复出来的 RP 仍是"能登录但收不到登出"。
func TestSubsystemUpdateBrowserCredentialsBackfillCoversCreatedClient(t *testing.T) {
	created := OAuthClientView{ID: "01CLI000000000000000000010", ClientID: "settlement-dev-web"}
	stub := &browserCredentialPortStub{created: OAuthClientCreateResult{Client: created, PlaintextSecret: "created-browser-secret"}}
	port := newBackchannelLogoutPortStub(nil)
	service := backfillBrowserCredentialService(t, stub, port)

	if _, err := service.Ensure(context.Background(), "tenant-1", "operator-1", "01APP000000000000000000001", "01ENV000000000000000000001", "settlement", "dev", "https://settlement.example.com/settlement/auth/callback"); err != nil {
		t.Fatal(err)
	}
	if len(port.upserts) != 1 || port.upserts[0] != "tenant-1|operator-1|01CLI000000000000000000010|https://settlement.example.com/settlement/auth/backchannel-logout" {
		t.Fatalf("created-client backfill upserts = %v", port.upserts)
	}
}
