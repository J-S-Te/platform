package infrastructure

import (
	"context"
	"errors"
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"strings"
	"testing"
	"time"
)

type deploymentOnboardingStub struct {
	result application.SubsystemOnboardingResult
	err    error
	seen   application.SubsystemOnboardingInput
	calls  int
}

func (stub *deploymentOnboardingStub) OnboardSubsystem(_ context.Context, input application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error) {
	stub.calls++
	stub.seen = input
	return stub.result, stub.err
}

type deploymentProvisionerStub struct {
	seen           application.SubsystemProvisioningInput
	provisionCalls int
	err            error
}

func (stub *deploymentProvisionerStub) Preflight(context.Context, application.SubsystemPreflightInput) error {
	return nil
}

func (stub *deploymentProvisionerStub) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	stub.provisionCalls++
	stub.seen = input
	return stub.err
}

func (stub *deploymentProvisionerStub) Update(context.Context, application.SubsystemProvisioningInput) error {
	return nil
}
func (stub *deploymentProvisionerStub) Teardown(context.Context, string, string, string) error {
	return nil
}

type relatedClientsStub struct {
	clients []application.RelatedSubsystemServiceClient
	err     error
}

func (stub relatedClientsStub) ResolveSubsystemRelatedServiceClients(context.Context, string, string, string) ([]application.RelatedSubsystemServiceClient, error) {
	return stub.clients, stub.err
}

type deploymentStateStub struct {
	status     string
	generation uint64
	attempts   uint32
	found      bool
	err        error
}

func (stub deploymentStateStub) ReadSubsystemAccessDeploymentState(context.Context, string, string, string) (string, uint64, uint32, bool, error) {
	return stub.status, stub.generation, stub.attempts, stub.found, stub.err
}

type issuerStub struct {
	issuer string
	err    error
}

func (stub issuerStub) ResolveSubsystemAccessIssuer(context.Context, string) (string, error) {
	return stub.issuer, stub.err
}

func deploymentAdapterFor(t *testing.T, service *deploymentOnboardingStub, provisioner *deploymentProvisionerStub, related relatedClientsStub, state deploymentStateStub, issuers issuerStub) *SubsystemAccessDeploymentAdapter {
	t.Helper()
	adapter, err := NewSubsystemAccessDeploymentAdapter(service, provisioner, accessCatalogWithTarget(), related, state, issuers, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func onboardingOperation() application.SubsystemAccessOperation {
	return application.SubsystemAccessOperation{ID: "01SAOP000000000000000000001", TenantID: "tenant-1", OperatorID: "operator-1",
		ApplicationCode: "settlement", EnvironmentCode: "dev", Action: application.SubsystemAccessActionOnboard,
		PublicOrigin: "https://settlement.example.com", AuthProvider: "platform", InitialAdminID: "01USR000000000000000000001"}
}

// 认证阶段：调用既有原子流程，凭据只进入运行状态；证据里不含任何秘密。
func TestSubsystemAccessDeploymentAuthenticationStashesCredentials(t *testing.T) {
	service := &deploymentOnboardingStub{result: application.SubsystemOnboardingResult{
		Application:     application.Application{ID: "01APP000000000000000000001", Code: "settlement"},
		Environment:     application.Environment{ID: "01ENV000000000000000000001", Environment: "dev"},
		OAuthClient:     application.OAuthClientView{ClientID: "settlement-dev-web"},
		PlaintextSecret: "super-secret-value",
		RedirectURI:     "https://settlement.example.com/callback", PublicURL: "https://settlement.example.com"}}
	adapter := deploymentAdapterFor(t, service, &deploymentProvisionerStub{}, relatedClientsStub{}, deploymentStateStub{}, issuerStub{issuer: "https://sso.example.com/realms/basic"})
	run := &application.SubsystemAccessRunState{}

	outcome, err := adapter.ConfigureSubsystemAccessAuthentication(context.Background(), onboardingOperation(), run)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("outcome = %+v err=%v", outcome, err)
	}
	if outcome.EvidenceRef != "onboarding:01APP000000000000000000001/01ENV000000000000000000001" {
		t.Fatalf("evidence = %s", outcome.EvidenceRef)
	}
	if strings.Contains(outcome.EvidenceRef, "secret") {
		t.Fatal("evidence must not leak credentials")
	}
	if run.Credentials == nil || run.Credentials.ClientSecret != "super-secret-value" || run.Credentials.ClientID != "settlement-dev-web" {
		t.Fatalf("credentials = %+v", run.Credentials)
	}
	if service.seen.InitialAdminUserID != "01USR000000000000000000001" || service.seen.UpstreamURL != "http://settlement-api:8080" {
		t.Fatalf("onboarding input = %+v", service.seen)
	}

	// 非首次接入：凭据必须走受控更新的既有服务。未登记时明确失败（可执行，不 500）。
	other := onboardingOperation()
	other.Action = application.SubsystemAccessActionUpdate
	updateOutcome, updateErr := adapter.ConfigureSubsystemAccessAuthentication(context.Background(), other, &application.SubsystemAccessRunState{})
	if updateErr != nil {
		t.Fatalf("非首次接入应返回阶段失败而不是内部错误：%v", updateErr)
	}
	if updateOutcome.Status != application.SubsystemAccessStatusFailed || updateOutcome.ErrorCode != "REGISTRATION_MISSING" {
		t.Fatalf("outcome = %+v，want failed/REGISTRATION_MISSING", updateOutcome)
	}

	// 已登记：AUTH 通过并明确"凭据由受控更新阶段准备"，不在两个阶段重复轮换密钥。
	registered := onboardingOperation()
	registered.Action = application.SubsystemAccessActionUpdate
	registered.ApplicationID = "01APP000000000000000000001"
	registered.EnvironmentID = "01ENV000000000000000000001"
	updateAdapter, adapterErr := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{}, accessCatalogWithTarget(), relatedClientsStub{}, deploymentStateStub{}, issuerStub{}, nil, updateCredentialStub{}, updateBrowserStub{}, nil)
	if adapterErr != nil {
		t.Fatalf("构建受控更新适配器：%v", adapterErr)
	}
	registeredOutcome, registeredErr := updateAdapter.ConfigureSubsystemAccessAuthentication(context.Background(), registered, &application.SubsystemAccessRunState{})
	if registeredErr != nil {
		t.Fatalf("已登记时不应返回内部错误：%v", registeredErr)
	}
	if registeredOutcome.Status != application.SubsystemAccessStatusSucceeded || registeredOutcome.EvidenceRef != "delegated:update-credentials" {
		t.Fatalf("outcome = %+v，want succeeded/delegated:update-credentials", registeredOutcome)
	}
}

// 部署阶段：使用运行状态里的凭据 + 清单目标 + issuer + 相关服务客户端调用部署 Agent。
func TestSubsystemAccessDeploymentProvisionUsesRunCredentials(t *testing.T) {
	provisioner := &deploymentProvisionerStub{}
	related := relatedClientsStub{clients: []application.RelatedSubsystemServiceClient{{ApplicationCode: "contract_management", Purpose: "settlement.contract.ingest"}}}
	adapter := deploymentAdapterFor(t, &deploymentOnboardingStub{}, provisioner, related, deploymentStateStub{}, issuerStub{issuer: "https://sso.example.com/realms/basic"})
	run := &application.SubsystemAccessRunState{Credentials: &application.SubsystemProvisioningCredentials{
		ClientID: "settlement-dev-web", ClientSecret: "secret", RedirectURI: "https://settlement.example.com/callback",
		PublicURL: "https://settlement.example.com"}}

	outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), run)
	if err != nil || outcome.EvidenceRef != "provision:manifest-aaa" {
		t.Fatalf("outcome = %+v err=%v", outcome, err)
	}
	if provisioner.provisionCalls != 1 || provisioner.seen.TenantID != "tenant-1" || provisioner.seen.ApplicationCode != "settlement" {
		t.Fatalf("provision input = %+v calls=%d", provisioner.seen, provisioner.provisionCalls)
	}
	if provisioner.seen.ClientSecret != "secret" || provisioner.seen.Issuer != "https://sso.example.com/realms/basic" {
		t.Fatalf("credentials/issuer not forwarded: %+v", provisioner.seen)
	}
	if len(provisioner.seen.RelatedServiceClients) != 1 {
		t.Fatalf("related clients = %+v", provisioner.seen.RelatedServiceClients)
	}
	if provisioner.seen.PathPrefix != "/settlement" || provisioner.seen.ManifestChecksum != "manifest-aaa" {
		t.Fatalf("manifest fields = %+v", provisioner.seen)
	}

	// 缺少运行内凭据（例如进程重启后继续）→ 明确失败码，提示重新执行。
	missing, err := adapter.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), &application.SubsystemAccessRunState{})
	if err != nil || missing.ErrorCode != "CREDENTIALS_MISSING" || missing.Status != application.SubsystemAccessStatusFailed {
		t.Fatalf("missing credentials = %+v err=%v", missing, err)
	}

	// 部署调用报错 → 视为"结果未知"（返回 error，由 Worker 保留租约下一轮核对）。
	failing := deploymentAdapterFor(t, &deploymentOnboardingStub{}, &deploymentProvisionerStub{err: errors.New("agent timeout")},
		relatedClientsStub{}, deploymentStateStub{}, issuerStub{issuer: "issuer"})
	if _, err := failing.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), run); err == nil {
		t.Fatal("provision error must be retryable, not silently succeeded")
	}

	// 更新路径未接线：明确报错。
	if _, err := adapter.UpdateSubsystemAccess(context.Background(), onboardingOperation(), run); err == nil {
		t.Fatal("update path must report not-wired")
	}
}

// 就绪核验：只有 READY 才算成功；失败与进行中分别映射为 FAILED / RUNNING。
func TestSubsystemAccessDeploymentReadinessMapping(t *testing.T) {
	operation := onboardingOperation()
	cases := []struct {
		state      deploymentStateStub
		wantStatus application.SubsystemAccessStatus
		wantCode   string
	}{
		{deploymentStateStub{status: "READY", generation: 2, attempts: 1, found: true}, application.SubsystemAccessStatusSucceeded, ""},
		// READY 但没有受控代次：不能宣告成功，只能继续等待/核对。
		{deploymentStateStub{status: "READY", found: true}, application.SubsystemAccessStatusRunning, "READINESS_UNVERIFIED"},
		{deploymentStateStub{status: "PROVISION_FAILED", found: true}, application.SubsystemAccessStatusFailed, "DEPLOYMENT_FAILED"},
		{deploymentStateStub{status: "PROVISIONING", found: true}, application.SubsystemAccessStatusRunning, ""},
		{deploymentStateStub{found: false}, application.SubsystemAccessStatusRunning, ""},
	}
	for _, item := range cases {
		adapter := deploymentAdapterFor(t, &deploymentOnboardingStub{}, &deploymentProvisionerStub{}, relatedClientsStub{}, item.state, issuerStub{})
		outcome, err := adapter.SubsystemAccessDeploymentReady(context.Background(), operation)
		if err != nil {
			t.Fatalf("readiness err = %v", err)
		}
		if outcome.Status != item.wantStatus || outcome.ErrorCode != item.wantCode {
			t.Fatalf("readiness(%s) = %+v", item.state.status, outcome)
		}
	}
}

type observationGateStub struct {
	allowed bool
	reason  string
	err     error
}

func (stub observationGateStub) CanProceedWithUpdate(context.Context, string, string, string) (bool, string, error) {
	return stub.allowed, stub.reason, stub.err
}

func deploymentAdapterWithGate(t *testing.T, gate SubsystemAccessObservationGate) *SubsystemAccessDeploymentAdapter {
	t.Helper()
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{},
		accessCatalogWithTarget(), relatedClientsStub{}, deploymentStateStub{}, issuerStub{}, gate, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// 更新路径的观察期门禁：未通过 → 等待（不是失败、不是重试）；通过 → 明确未接线；读取失败 → 可重试错误。
func TestSubsystemAccessDeploymentUpdateHonoursObservationGate(t *testing.T) {
	pending := deploymentAdapterWithGate(t, observationGateStub{allowed: false, reason: "OBSERVATION_WINDOW_NOT_COMPLETED"})
	outcome, err := pending.UpdateSubsystemAccess(context.Background(), onboardingOperation(), nil)
	if err != nil || outcome.Status != application.SubsystemAccessStatusWaiting || outcome.ErrorCode != "AUTH_OBSERVATION_PENDING" {
		t.Fatalf("pending gate outcome = %+v err=%v", outcome, err)
	}
	if !strings.Contains(outcome.Message, "OBSERVATION_WINDOW_NOT_COMPLETED") {
		t.Fatalf("pending gate message = %q", outcome.Message)
	}

	allowed := deploymentAdapterWithGate(t, observationGateStub{allowed: true})
	if _, err := allowed.UpdateSubsystemAccess(context.Background(), onboardingOperation(), nil); !errors.Is(err, application.ErrSubsystemAccessStageNotWired) {
		t.Fatalf("allowed gate must report not-wired, got %v", err)
	}

	withoutGate := deploymentAdapterWithGate(t, nil)
	if _, err := withoutGate.UpdateSubsystemAccess(context.Background(), onboardingOperation(), nil); !errors.Is(err, application.ErrSubsystemAccessStageNotWired) {
		t.Fatalf("nil gate must report not-wired, got %v", err)
	}

	failing := deploymentAdapterWithGate(t, observationGateStub{err: errors.New("database unavailable")})
	if _, err := failing.UpdateSubsystemAccess(context.Background(), onboardingOperation(), nil); err == nil || errors.Is(err, application.ErrSubsystemAccessStageNotWired) {
		t.Fatalf("gate read failure must be retryable, got %v", err)
	}
}

type updateCredentialPreparerStub struct {
	credentials []application.SubsystemServiceCredential
	err         error
	calls       int
	seen        application.SubsystemUpdateServiceCredentialRequest
}

func (stub *updateCredentialPreparerStub) Ensure(_ context.Context, request application.SubsystemUpdateServiceCredentialRequest) ([]application.SubsystemServiceCredential, error) {
	stub.calls++
	stub.seen = request
	return stub.credentials, stub.err
}

type browserCredentialPreparerStub struct {
	credential application.SubsystemUpdateBrowserCredential
	err        error
	calls      int
}

func (stub *browserCredentialPreparerStub) Ensure(context.Context, string, string, string, string, string, string, string) (application.SubsystemUpdateBrowserCredential, error) {
	stub.calls++
	return stub.credential, stub.err
}

type updateProvisionerStub struct {
	calls int
	seen  application.SubsystemProvisioningInput
	err   error
}

func (stub *updateProvisionerStub) Preflight(context.Context, application.SubsystemPreflightInput) error {
	return nil
}
func (stub *updateProvisionerStub) Provision(context.Context, application.SubsystemProvisioningInput) error {
	return nil
}
func (stub *updateProvisionerStub) Update(_ context.Context, input application.SubsystemProvisioningInput) error {
	stub.calls++
	stub.seen = input
	return stub.err
}
func (stub *updateProvisionerStub) Teardown(context.Context, string, string, string) error {
	return nil
}

func updateAdapterFor(t *testing.T, serviceCredentials *updateCredentialPreparerStub, browserCredentials *browserCredentialPreparerStub, provisioner *updateProvisionerStub) *SubsystemAccessDeploymentAdapter {
	return updateAdapterWithPublisher(t, serviceCredentials, browserCredentials, &catalogPublisherPreparerStub{credential: application.SubsystemUpdateCatalogPublisherCredential{ClientID: "settlement-dev-catalog-publisher", ClientSecret: "publisher-secret"}}, provisioner)
}

func updateAdapterWithPublisher(t *testing.T, serviceCredentials *updateCredentialPreparerStub, browserCredentials *browserCredentialPreparerStub, publisher *catalogPublisherPreparerStub, provisioner *updateProvisionerStub) *SubsystemAccessDeploymentAdapter {
	t.Helper()
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, provisioner, accessCatalogWithTarget(),
		relatedClientsStub{clients: []application.RelatedSubsystemServiceClient{{
			ApplicationCode: "settlement", Environment: "dev", Purpose: "settlement_contract_ingest",
			Client: application.OAuthClientView{ID: "01CLI000000000000000000011", ClientID: "settlement-dev-settlement-integration"},
		}}}, deploymentStateStub{}, issuerStub{issuer: "https://sso.example.test/realms/basic"},
		observationGateStub{allowed: true}, serviceCredentials, browserCredentials, publisher)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func updateOperationFixture() application.SubsystemAccessOperation {
	return application.SubsystemAccessOperation{
		ID: "01SAOP00000000000000000001", TenantID: "tenant-1", OperatorID: "operator-1",
		ApplicationID: "01APP000000000000000000001", EnvironmentID: "01ENV000000000000000000001",
		ApplicationCode: "settlement", EnvironmentCode: "dev", Action: application.SubsystemAccessActionUpdate,
		PublicOrigin: "https://settlement.example.com",
	}
}

// 更新路径：门禁通过 → 服务凭据（需求来自需求表）→ 浏览器凭据 → 装配（含网关派生）→ Agent 更新。
func TestSubsystemAccessDeploymentUpdatePathWired(t *testing.T) {
	serviceCredentials := &updateCredentialPreparerStub{credentials: []application.SubsystemServiceCredential{{Purpose: application.ServiceCredentialFileGatewayWrite}}}
	browserCredentials := &browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web", ClientSecret: "browser-secret"}}
	provisioner := &updateProvisionerStub{}
	adapter := updateAdapterFor(t, serviceCredentials, browserCredentials, provisioner)

	outcome, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), &application.SubsystemAccessRunState{})
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if outcome.EvidenceRef != "update:manifest-aaa" {
		t.Fatalf("evidence = %s", outcome.EvidenceRef)
	}
	if serviceCredentials.calls != 1 || browserCredentials.calls != 1 || provisioner.calls != 1 {
		t.Fatalf("calls: service=%d browser=%d provisioner=%d", serviceCredentials.calls, browserCredentials.calls, provisioner.calls)
	}
	if len(serviceCredentials.seen.Requirements) == 0 {
		t.Fatal("service credential requirements must come from the extracted table")
	}
	if serviceCredentials.seen.Operation != "UPDATE" {
		t.Fatalf("operation = %s", serviceCredentials.seen.Operation)
	}
	if provisioner.seen.ClientID != "settlement-dev-web" || provisioner.seen.ClientSecret != "browser-secret" {
		t.Fatalf("browser credential not forwarded: %+v", provisioner.seen)
	}
	if len(provisioner.seen.ServiceCredentials) != 1 {
		t.Fatalf("service credentials = %+v", provisioner.seen.ServiceCredentials)
	}
	// 网关派生：公开地址 + 清单里的路径前缀。
	if provisioner.seen.PublicURL != "https://settlement.example.com/settlement/" ||
		provisioner.seen.RedirectURI != "https://settlement.example.com/settlement/auth/callback" ||
		provisioner.seen.PathPrefix != "/settlement" {
		t.Fatalf("gateway derivation = %+v", provisioner.seen)
	}
	// 相关服务客户端元数据必须随更新输入下发（与首次接入同一取数）。
	if len(provisioner.seen.RelatedServiceClients) != 1 ||
		provisioner.seen.RelatedServiceClients[0].Client.ClientID != "settlement-dev-settlement-integration" {
		t.Fatalf("related service clients = %+v", provisioner.seen.RelatedServiceClients)
	}
	// 目录发布者凭据必须随更新输入下发（settlement 属于需要它的六个应用之一）。
	if provisioner.seen.CatalogPublisherClientID != "settlement-dev-catalog-publisher" || provisioner.seen.CatalogPublisherClientSecret != "publisher-secret" {
		t.Fatalf("catalog publisher credential = %q/%q", provisioner.seen.CatalogPublisherClientID, provisioner.seen.CatalogPublisherClientSecret)
	}
	if provisioner.seen.Issuer != "https://sso.example.test/realms/basic" || provisioner.seen.ManifestChecksum != "manifest-aaa" {
		t.Fatalf("issuer/manifest = %+v", provisioner.seen)
	}
}

// 凭据不可用（缺失客户端/漂移）→ 明确失败码；Agent 报错 → 可重试。
func TestSubsystemAccessDeploymentUpdatePathFailureMapping(t *testing.T) {
	missing := &updateCredentialPreparerStub{err: application.ErrSubsystemAccessStageNotWired}
	adapter := updateAdapterFor(t, missing, &browserCredentialPreparerStub{}, &updateProvisionerStub{})
	outcome, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil)
	if err != nil || outcome.Status != application.SubsystemAccessStatusFailed || outcome.ErrorCode != "UPDATE_CREDENTIALS_UNAVAILABLE" {
		t.Fatalf("missing credentials = %+v err=%v", outcome, err)
	}

	drifted := &updateCredentialPreparerStub{err: application.ErrSubsystemUpdateServiceClientInvalid}
	adapter = updateAdapterFor(t, drifted, &browserCredentialPreparerStub{}, &updateProvisionerStub{})
	outcome, err = adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil)
	if err != nil || outcome.ErrorCode != "UPDATE_CREDENTIALS_UNAVAILABLE" {
		t.Fatalf("drifted credentials = %+v err=%v", outcome, err)
	}

	agentFailure := &updateProvisionerStub{err: errors.New("agent timeout")}
	adapter = updateAdapterFor(t, &updateCredentialPreparerStub{}, &browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web"}}, agentFailure)
	if _, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil); err == nil {
		t.Fatal("agent failure must be retryable, not silently succeeded")
	}
}

type catalogPublisherPreparerStub struct {
	credential application.SubsystemUpdateCatalogPublisherCredential
	err        error
	calls      int
}

func (stub *catalogPublisherPreparerStub) Ensure(context.Context, string, string, string, string, string, string) (application.SubsystemUpdateCatalogPublisherCredential, error) {
	stub.calls++
	return stub.credential, stub.err
}

// 目录发布者凭据漂移 → 明确失败码（不静默、不重试）。
func TestSubsystemAccessDeploymentUpdatePathPublisherFailure(t *testing.T) {
	adapter := updateAdapterWithPublisher(t, &updateCredentialPreparerStub{},
		&browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web"}},
		&catalogPublisherPreparerStub{err: application.ErrSubsystemUpdateServiceClientInvalid}, &updateProvisionerStub{})
	outcome, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil)
	if err != nil || outcome.Status != application.SubsystemAccessStatusFailed || outcome.ErrorCode != "UPDATE_CREDENTIALS_UNAVAILABLE" {
		t.Fatalf("publisher drift = %+v err=%v", outcome, err)
	}
}

type deploymentContextStub struct {
	context SubsystemAccessDeploymentContext
	err     error
}

func (stub deploymentContextStub) ReadSubsystemAccessDeploymentContext(context.Context, string, string, string) (SubsystemAccessDeploymentContext, error) {
	return stub.context, stub.err
}

func updateAdapterForInitialRuntime(t *testing.T, production bool, action application.SubsystemAccessAction, deploymentContext SubsystemAccessDeploymentContext) *updateProvisionerStub {
	t.Helper()
	provisioner := &updateProvisionerStub{}
	adapter := updateAdapterFor(t, &updateCredentialPreparerStub{}, &browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web"}}, provisioner)
	adapter.SetInitialRuntimeProvisioningContext(production, deploymentContextStub{context: deploymentContext})
	operationFixture := updateOperationFixture()
	operationFixture.Action = action
	if _, err := adapter.UpdateSubsystemAccess(context.Background(), operationFixture, nil); err != nil {
		t.Fatal(err)
	}
	return provisioner
}

// InitialRuntimeProvisioning 派生：仅 production + ADOPT/RETRY(FAILED) + 尚未应用清单时为真。
func TestSubsystemAccessDeploymentInitialRuntimeProvisioningDerivation(t *testing.T) {
	failing := SubsystemAccessDeploymentContext{Found: true, Status: string(application.SubsystemDeploymentStatusFailed)}
	applied := SubsystemAccessDeploymentContext{Found: true, Status: string(application.SubsystemDeploymentStatusFailed), AppliedManifestChecksum: "manifest-aaa"}
	healthy := SubsystemAccessDeploymentContext{Found: true, Status: "READY"}

	cases := []struct {
		name       string
		production bool
		action     application.SubsystemAccessAction
		context    SubsystemAccessDeploymentContext
		want       bool
	}{
		{"production+ADOPT+未应用", true, application.SubsystemAccessActionAdopt, failing, true},
		{"production+RETRY(FAILED)+未应用", true, application.SubsystemAccessActionRetry, failing, true},
		{"production+RETRY(READY)", true, application.SubsystemAccessActionRetry, healthy, false},
		{"production+UPDATE", true, application.SubsystemAccessActionUpdate, failing, false},
		{"production+已应用清单", true, application.SubsystemAccessActionAdopt, applied, false},
		{"非 production", false, application.SubsystemAccessActionAdopt, failing, false},
		{"无受控部署状态", true, application.SubsystemAccessActionAdopt, SubsystemAccessDeploymentContext{}, false},
	}
	for _, item := range cases {
		provisioner := updateAdapterForInitialRuntime(t, item.production, item.action, item.context)
		if provisioner.seen.InitialRuntimeProvisioning != item.want {
			t.Fatalf("%s: InitialRuntimeProvisioning = %v, want %v", item.name, provisioner.seen.InitialRuntimeProvisioning, item.want)
		}
	}
}

type brokerCredentialsStub struct {
	secret       string
	recovered    string
	ensureCalls  int
	recoverCalls int
}

func (stub *brokerCredentialsStub) EnsureKeycloakBroker(context.Context, string) (string, string, error) {
	stub.ensureCalls++
	return "platform-broker", stub.secret, nil
}

func (stub *brokerCredentialsStub) RecoverKeycloakBroker(context.Context, string, string) (string, string, error) {
	stub.recoverCalls++
	return "platform-broker", stub.recovered, nil
}

type brokerControlStub struct {
	verifyErr   error
	ensureCalls int
	verifyCalls int
}

func (stub *brokerControlStub) EnsureBroker(context.Context, string, string) error {
	stub.ensureCalls++
	return nil
}

func (stub *brokerControlStub) VerifyBrokerExists(context.Context) error {
	stub.verifyCalls++
	return stub.verifyErr
}

// broker 协调（既有 4 步语义）：有明文→直接协调；校验通过→不改动；
// 可恢复错误→轮换后协调；不可恢复→原样返回；轮换结果为空→错误。
func TestSubsystemAccessDeploymentReconcileKeycloakBroker(t *testing.T) {
	cases := []struct {
		name        string
		secret      string
		recovered   string
		verifyErr   error
		recoverable bool
		wantErr     bool
		wantEnsure  int
		wantRecover int
	}{
		{"已有明文 → 直接协调", "secret", "", nil, true, false, 1, 0},
		{"无明文但校验通过 → 不改动", "", "", nil, true, false, 0, 0},
		{"可恢复错误 → 轮换后协调", "", "rotated", errors.New("incomplete IdP config"), true, false, 1, 1},
		{"不可恢复错误 → 原样返回", "", "", errors.New("unreachable"), false, true, 0, 0},
		{"轮换结果为空 → 错误", "", "", errors.New("incomplete IdP config"), true, true, 0, 1},
	}
	for _, item := range cases {
		credentials := &brokerCredentialsStub{secret: item.secret, recovered: item.recovered}
		control := &brokerControlStub{verifyErr: item.verifyErr}
		adapter := updateAdapterFor(t, &updateCredentialPreparerStub{},
			&browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web"}},
			&updateProvisionerStub{})
		adapter.SetKeycloakBrokerReconciliation(SubsystemAccessKeycloakBrokerReconciliation{
			Credentials: credentials, Control: control,
			IsRecoverable: func(error) bool { return item.recoverable },
		})
		_, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil)
		if (err != nil) != item.wantErr {
			t.Fatalf("%s: err = %v, want error=%v", item.name, err, item.wantErr)
		}
		if control.ensureCalls != item.wantEnsure || credentials.recoverCalls != item.wantRecover {
			t.Fatalf("%s: ensureBroker=%d recover=%d, want %d/%d", item.name, control.ensureCalls, credentials.recoverCalls, item.wantEnsure, item.wantRecover)
		}
	}
}

type manifestStateStub struct {
	calls    int
	checksum string
	tenantID string
	err      error
}

func (stub *manifestStateStub) SetSubsystemDesiredManifest(_ context.Context, tenantID, _ string, _ string, checksum string, _ time.Time) error {
	stub.calls++
	stub.tenantID = tenantID
	stub.checksum = checksum
	return stub.err
}

// MarkSubsystemManifestApplied 用途：为既有期望清单测试提供完整状态存储端口。
// 参数：全部为测试部署上下文与已应用清单。
// 返回值：无持久化失败时返回 nil。
func (stub *manifestStateStub) MarkSubsystemManifestApplied(context.Context, string, string, string, string, time.Time) error {
	return nil
}

// 期望清单落库：未注入不落库；已注入且清单非空则落库；清单为空则不动。
func TestSubsystemAccessDeploymentRecordsDesiredManifest(t *testing.T) {
	// 未注入：不落库（行为与接线前一致）。
	unwired := &manifestStateStub{}
	adapter := updateAdapterFor(t, &updateCredentialPreparerStub{}, &browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web"}}, &updateProvisionerStub{})
	adapter.SetManifestStateStore(unwired)
	if _, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil); err != nil {
		t.Fatal(err)
	}
	if unwired.calls != 1 || unwired.checksum != "manifest-aaa" || unwired.tenantID != "tenant-1" {
		t.Fatalf("wired manifest store calls=%d checksum=%q tenant=%q", unwired.calls, unwired.checksum, unwired.tenantID)
	}

	// 未注入（nil）：调用不发生，且不报错。
	adapter = updateAdapterFor(t, &updateCredentialPreparerStub{}, &browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web"}}, &updateProvisionerStub{})
	if _, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), nil); err != nil {
		t.Fatalf("nil manifest store must be a no-op: %v", err)
	}
}

// 受控更新的凭据服务桩：AUTH 只校验其存在，不重复准备凭据。
type updateCredentialStub struct{}

func (updateCredentialStub) Ensure(context.Context, application.SubsystemUpdateServiceCredentialRequest) ([]application.SubsystemServiceCredential, error) {
	return nil, nil
}

type updateBrowserStub struct{}

func (updateBrowserStub) Ensure(context.Context, string, string, string, string, string, string, string) (application.SubsystemUpdateBrowserCredential, error) {
	return application.SubsystemUpdateBrowserCredential{ClientID: "settlement-dev-web", ClientSecret: "rotated-secret"}, nil
}

// 结果阶段收口：初始管理员授权必须落到受控服务；未注入端口时显式标注 skipped，
// 缺少初始管理员时标注 no-initial-admin —— 三种情况都不能静默成功而不留证据。
func TestFinalizeSubsystemAccessMarksInitialAdmin(t *testing.T) {
	marker := &initialAdminMarkerStub{}
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{}, accessCatalogWithTarget(), relatedClientsStub{}, deploymentStateStub{}, issuerStub{}, nil, updateCredentialStub{}, updateBrowserStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.initialAdmin = marker

	op := onboardingOperation()
	outcome, err := adapter.FinalizeSubsystemAccess(context.Background(), op, nil)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if outcome.Status != application.SubsystemAccessStatusSucceeded || marker.userID != op.InitialAdminID {
		t.Fatalf("outcome=%+v marker=%+v，必须把初始管理员落到受控服务", outcome, marker)
	}

	// 未配置初始管理员：不调用标记、但证据明确。
	op.InitialAdminID = ""
	outcome, _ = adapter.FinalizeSubsystemAccess(context.Background(), op, nil)
	if !strings.HasPrefix(outcome.EvidenceRef, "result:no-initial-admin") {
		t.Fatalf("evidence=%q", outcome.EvidenceRef)
	}

	// 端口未注入：显式 skipped，不静默。
	adapter.initialAdmin = nil
	op.InitialAdminID = "01USR000000000000000000001"
	outcome, _ = adapter.FinalizeSubsystemAccess(context.Background(), op, nil)
	if !strings.HasPrefix(outcome.EvidenceRef, "result:initial-admin-skipped") {
		t.Fatalf("evidence=%q，未注入端口时必须显式标注 skipped", outcome.EvidenceRef)
	}
}

type initialAdminMarkerStub struct{ userID string }

func (stub *initialAdminMarkerStub) MarkSubsystemInitialAccessAssigned(_ context.Context, _, _, _, userID string, _ time.Time) error {
	stub.userID = userID
	return nil
}

// 首次接入成功后必须落期望清单（漂移检测基准）；否则此后永远判不出配置漂移。
func TestProvisionSubsystemAccessRecordsDesiredManifest(t *testing.T) {
	manifest := &manifestStateStub{}
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{}, accessCatalogWithTarget(), relatedClientsStub{}, deploymentStateStub{}, issuerStub{issuer: "https://sso.example.com/realms/basic"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.SetManifestStateStore(manifest)
	run := &application.SubsystemAccessRunState{Credentials: &application.SubsystemProvisioningCredentials{ClientID: "c", ClientSecret: "s"}}

	if _, err := adapter.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), run); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if manifest.checksum == "" {
		t.Fatal("首次接入成功后未记录期望清单，后续无法检测漂移")
	}
}

// READY 只说明容器起来了；必须确认本次期望清单已应用。
// 三态：一致→成功；不一致→失败（需重新部署）；未上报/未应用→继续等待，不得谎报成功。
func TestReadinessVerifiesAppliedManifest(t *testing.T) {
	op := onboardingOperation()
	cases := []struct {
		name       string
		reader     appliedManifestStub
		wantStatus application.SubsystemAccessStatus
		wantCode   string
	}{
		{"一致", appliedManifestStub{checksum: "manifest-aaa", verified: true}, application.SubsystemAccessStatusSucceeded, ""},
		{"不一致", appliedManifestStub{checksum: "manifest-old", verified: true}, application.SubsystemAccessStatusFailed, "MANIFEST_MISMATCH"},
		{"未上报", appliedManifestStub{verified: false}, application.SubsystemAccessStatusRunning, "MANIFEST_UNVERIFIED"},
		{"未记录", appliedManifestStub{verified: true}, application.SubsystemAccessStatusRunning, "MANIFEST_NOT_APPLIED"},
	}
	for _, item := range cases {
		adapter := deploymentAdapterFor(t, &deploymentOnboardingStub{}, &deploymentProvisionerStub{}, relatedClientsStub{},
			deploymentStateStub{status: "READY", generation: 2, attempts: 1, found: true}, issuerStub{})
		adapter.SetAppliedManifestReader(item.reader)
		outcome, err := adapter.SubsystemAccessDeploymentReady(context.Background(), op)
		if err != nil {
			t.Fatalf("%s: readiness err = %v", item.name, err)
		}
		if outcome.Status != item.wantStatus || outcome.ErrorCode != item.wantCode {
			t.Fatalf("%s: outcome = %+v，want %s/%s", item.name, outcome, item.wantStatus, item.wantCode)
		}
	}
}

type appliedManifestStub struct {
	checksum string
	verified bool
}

func (stub appliedManifestStub) ReadSubsystemAccessAppliedManifest(context.Context, string, string, string) (string, bool, error) {
	return stub.checksum, stub.verified, nil
}

// Keycloak 提供方的结果阶段必须执行认证投影；缺 ClientID 时明确失败，
// 非 Keycloak 或未注入服务时显式 skipped（都不静默假装完成）。
func TestFinalizeSubsystemAccessProjectsKeycloakAuthorization(t *testing.T) {
	projection := &keycloakProjectionStub{}
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{}, accessCatalogWithTarget(), relatedClientsStub{}, deploymentStateStub{}, issuerStub{}, nil, updateCredentialStub{}, updateBrowserStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewKeycloakAuthorizationProjectionService(projection, projection, projection, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.SetKeycloakProjection(service, "basic")

	op := onboardingOperation()
	op.AuthProvider = "keycloak"
	op.ApplicationID = "01APP000000000000000000001"
	op.EnvironmentID = "01ENV000000000000000000001"
	run := &application.SubsystemAccessRunState{Credentials: &application.SubsystemProvisioningCredentials{ClientID: "settlement-dev-web"}}

	outcome, err := adapter.FinalizeSubsystemAccess(context.Background(), op, run)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if projection.clientID != "settlement-dev-web" || projection.realm != "basic" {
		t.Fatalf("projection client=%q realm=%q", projection.clientID, projection.realm)
	}

	// 缺 ClientID：必须失败并给出可执行错误码。
	empty := adapter
	outcome, _ = empty.FinalizeSubsystemAccess(context.Background(), op, &application.SubsystemAccessRunState{})
	if outcome.Status != application.SubsystemAccessStatusFailed || outcome.ErrorCode != "PROJECTION_CLIENT_MISSING" {
		t.Fatalf("outcome=%+v，缺 ClientID 必须失败", outcome)
	}
}

type keycloakProjectionStub struct {
	clientID, realm string
}

func (stub *keycloakProjectionStub) ListKeycloakRoleCodes(context.Context, string, string) ([]string, error) {
	return []string{"crm.admin"}, nil
}
func (stub *keycloakProjectionStub) EnsureClientRoles(context.Context, string, []string) error {
	return nil
}
func (stub *keycloakProjectionStub) SaveKeycloakClientMapping(_ context.Context, _, _, _, realm, clientID string) error {
	stub.clientID, stub.realm = clientID, realm
	return nil
}
func (stub *keycloakProjectionStub) BackfillKeycloakAuthorization(context.Context, string, string, string) error {
	return nil
}

// 进程重启后从 DEPLOY 继续：认证效果已发生（部署状态行存在）时应受控重建凭据，
// 而不是让用户重跑接入；状态行不存在时必须明确失败，绝不凭猜测创建客户端。
func TestProvisionSubsystemAccessRebuildsCredentialsAfterRestart(t *testing.T) {
	provisioner := &deploymentProvisionerStub{}
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, provisioner, accessCatalogWithTarget(), relatedClientsStub{},
		deploymentStateStub{status: "PROVISIONING", found: true}, issuerStub{issuer: "https://sso.example.com/realms/basic"}, nil,
		updateCredentialStub{}, updateBrowserStub{}, updateCatalogPublisherStub{})
	if err != nil {
		t.Fatal(err)
	}
	op := onboardingOperation()
	op.ApplicationID = "01APP000000000000000000001"
	op.EnvironmentID = "01ENV000000000000000000001"
	run := &application.SubsystemAccessRunState{}

	outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), op, run)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if outcome.Status != application.SubsystemAccessStatusSucceeded {
		t.Fatalf("outcome=%+v，状态行存在时应重建凭据并继续部署", outcome)
	}
	if run.Credentials == nil || run.Credentials.ClientID == "" {
		t.Fatal("重建后必须把凭据放进本次运行状态")
	}
	if provisioner.provisionCalls == 0 {
		t.Fatal("重建成功后必须继续调用部署")
	}

	// 状态行不存在：认证效果未发生，必须失败而不是自己造客户端。
	missingAdapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{}, accessCatalogWithTarget(), relatedClientsStub{},
		deploymentStateStub{found: false}, issuerStub{}, nil, updateCredentialStub{}, updateBrowserStub{}, updateCatalogPublisherStub{})
	if err != nil {
		t.Fatal(err)
	}
	outcome, _ = missingAdapter.ProvisionSubsystemAccess(context.Background(), op, &application.SubsystemAccessRunState{})
	if outcome.Status != application.SubsystemAccessStatusFailed || outcome.ErrorCode != "CREDENTIALS_MISSING" {
		t.Fatalf("outcome=%+v，状态缺失必须失败", outcome)
	}
}

type updateCatalogPublisherStub struct{}

func (updateCatalogPublisherStub) Ensure(context.Context, string, string, string, string, string, string) (application.SubsystemUpdateCatalogPublisherCredential, error) {
	return application.SubsystemUpdateCatalogPublisherCredential{ClientID: "settlement-dev-catalog-publisher", ClientSecret: "publisher-secret"}, nil
}

type accessBrowserReferenceStub struct {
	calls        int
	appID, envID string
	err          error
}

func (stub *accessBrowserReferenceStub) ReadSubsystemAccessBrowserClientID(_ context.Context, _, appID, envID, _, _ string) (string, error) {
	stub.calls++
	stub.appID, stub.envID = appID, envID
	return "settlement-dev-web", stub.err
}
func TestSubsystemAccessFinalizerRecoversIdentityAfterRestart(t *testing.T) {
	projection := &keycloakProjectionStub{}
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{}, accessCatalogWithTarget(), relatedClientsStub{}, deploymentStateStub{}, issuerStub{}, nil, updateCredentialStub{}, updateBrowserStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewKeycloakAuthorizationProjectionService(projection, projection, projection, nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.SetKeycloakProjection(service, "basic")
	references := &accessBrowserReferenceStub{}
	adapter.SetIdentityReaders(registryLookupStub{applicationID: "app-1", environmentID: "env-1", found: true}, references)
	op := onboardingOperation()
	op.ApplicationID = ""
	op.EnvironmentID = ""
	op.AuthProvider = "keycloak"
	for _, run := range []*application.SubsystemAccessRunState{{Credentials: &application.SubsystemProvisioningCredentials{ClientID: "settlement-dev-web"}}, {}} {
		outcome, err := adapter.FinalizeSubsystemAccess(context.Background(), op, run)
		if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
			t.Fatalf("outcome=%+v err=%v", outcome, err)
		}
	}
	if references.calls != 1 || references.appID != "app-1" || references.envID != "env-1" {
		t.Fatalf("references=%+v", references)
	}
	op.ApplicationID = "other-app"
	if _, err := adapter.FinalizeSubsystemAccess(context.Background(), op, &application.SubsystemAccessRunState{}); err == nil {
		t.Fatal("changed registration accepted")
	}
	references.err = errors.New("reference unavailable")
	op.ApplicationID = ""
	if _, err := adapter.FinalizeSubsystemAccess(context.Background(), op, &application.SubsystemAccessRunState{}); err == nil {
		t.Fatal("failed reference read accepted")
	}
}

type accessConfigurationEvidenceStub struct {
	snapshot                 string
	verified                 bool
	err                      error
	snapshots, verifications int
}

func (stub *accessConfigurationEvidenceStub) Snapshot(context.Context, application.SubsystemAccessOperation) (string, error) {
	stub.snapshots++
	return stub.snapshot, stub.err
}
func (stub *accessConfigurationEvidenceStub) Verify(context.Context, application.SubsystemAccessOperation) (bool, error) {
	stub.verifications++
	return stub.verified, stub.err
}
func TestSubsystemAccessFinalizerRejectsChangedConfiguration(t *testing.T) {
	adapter, err := NewSubsystemAccessDeploymentAdapter(&deploymentOnboardingStub{}, &deploymentProvisionerStub{}, accessCatalogWithTarget(), relatedClientsStub{}, deploymentStateStub{}, issuerStub{}, nil, updateCredentialStub{}, updateBrowserStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := &accessConfigurationEvidenceStub{verified: false}
	adapter.SetConfigurationEvidence(tracker)
	outcome, err := adapter.FinalizeSubsystemAccess(context.Background(), onboardingOperation(), &application.SubsystemAccessRunState{})
	if err != nil || outcome.Status != application.SubsystemAccessStatusFailed || outcome.ErrorCode != "CONFIGURATION_CHANGED" {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	tracker.err = errors.New("evidence unavailable")
	if _, err = adapter.FinalizeSubsystemAccess(context.Background(), onboardingOperation(), nil); err == nil {
		t.Fatal("unreadable evidence accepted")
	}
}

func TestSubsystemAccessProvisionCarriesResolvedIdentityAndCandidate(t *testing.T) {
	provisioner := &deploymentProvisionerStub{}
	adapter := deploymentAdapterFor(t, &deploymentOnboardingStub{}, provisioner, relatedClientsStub{}, deploymentStateStub{}, issuerStub{issuer: "https://sso.example.com"})
	adapter.SetIdentityReaders(registryLookupStub{applicationID: "app-1", environmentID: "env-1", found: true}, nil)
	tracker := &accessConfigurationEvidenceStub{snapshot: strings.Repeat("a", 64)}
	adapter.SetConfigurationEvidence(tracker)
	run := &application.SubsystemAccessRunState{Credentials: &application.SubsystemProvisioningCredentials{ClientID: "settlement-dev-web", ClientSecret: "secret", RedirectURI: "https://settlement.example.com/callback", PublicURL: "https://settlement.example.com"}}
	outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), run)
	if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded || accessCandidateFromEvidence(outcome.EvidenceRef) != tracker.snapshot {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if provisioner.seen.ApplicationID != "app-1" || provisioner.seen.EnvironmentID != "env-1" {
		t.Fatal("first onboarding IDs were not forwarded")
	}
	tracker.err = errors.New("snapshot unavailable")
	if _, err = adapter.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), run); err == nil || provisioner.provisionCalls != 1 {
		t.Fatal("deployment ran without candidate evidence")
	}
}

type accessRuntimeEnrollmentStub struct {
	fail bool
	seen application.RuntimeLicenseSettings
}

func (s *accessRuntimeEnrollmentStub) PrepareWithSettings(_ context.Context, tenant, appID, envID, app, env, operator string, settings application.RuntimeLicenseSettings) ([]application.RuntimeLicenseCredential, error) {
	s.seen = settings
	if s.fail {
		return nil, errors.New("enrollment rejected")
	}
	return []application.RuntimeLicenseCredential{{Approval: application.RuntimeLicenseApproval{ServiceID: "settlement-api"}, RegistrationProtocol: 2, ClientID: "runtime-client", ClientSecret: "runtime-secret"}}, nil
}
func (*accessRuntimeEnrollmentStub) WaitForRuntimeConfirmation(context.Context, string, string, []application.RuntimeLicenseCredential) error {
	return nil
}
func TestAccessDeploymentDeliversEnrollmentAndStopsOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		p := &deploymentProvisionerStub{}
		adapter := deploymentAdapterFor(t, &deploymentOnboardingStub{}, p, relatedClientsStub{}, deploymentStateStub{}, issuerStub{issuer: "https://platform.example"})
		enrollment := &accessRuntimeEnrollmentStub{fail: fail}
		adapter.SetRuntimeLicenseEnrollment(enrollment, func(context.Context) (application.RuntimeLicenseSettings, error) {
			return application.RuntimeLicenseSettings{InstanceID: "instance", Environment: "production"}, nil
		}, false)
		run := &application.SubsystemAccessRunState{Credentials: &application.SubsystemProvisioningCredentials{ClientID: "web", ClientSecret: "secret", RedirectURI: "https://settlement.example.com/callback", PublicURL: "https://settlement.example.com"}}
		outcome, err := adapter.ProvisionSubsystemAccess(context.Background(), onboardingOperation(), run)
		if fail {
			if err == nil {
				t.Fatal("enrollment failure ignored")
			}
			if p.seen.ApplicationCode != "" {
				t.Fatal("Agent invoked after enrollment failure")
			}
			continue
		}
		if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded {
			t.Fatalf("deployment: %v %v", outcome, err)
		}
		if len(p.seen.RuntimeLicenseCredentials) != 1 || p.seen.RuntimeLicenseSettings.InstanceID != "instance" {
			t.Fatal("runtime credentials not delivered to Agent")
		}
	}
}

func TestAccessUpdateDeliversEnrollmentAndStopsOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		provisioner := &updateProvisionerStub{}
		adapter := updateAdapterFor(t, &updateCredentialPreparerStub{}, &browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "web", ClientSecret: "secret"}}, provisioner)
		adapter.SetRuntimeLicenseEnrollment(&accessRuntimeEnrollmentStub{fail: fail}, func(context.Context) (application.RuntimeLicenseSettings, error) {
			return application.RuntimeLicenseSettings{InstanceID: "instance", Environment: "production"}, nil
		}, false)
		outcome, err := adapter.UpdateSubsystemAccess(context.Background(), updateOperationFixture(), &application.SubsystemAccessRunState{})
		if fail {
			if err == nil || provisioner.calls != 0 {
				t.Fatal("enrollment failure did not block update")
			}
			continue
		}
		if err != nil || outcome.Status != application.SubsystemAccessStatusSucceeded || len(provisioner.seen.RuntimeLicenseCredentials) != 1 {
			t.Fatalf("update enrollment missing: %v %v", outcome, err)
		}
	}
}

type accessProviderBrowserStub struct{ fail bool }

func (s accessProviderBrowserStub) EnsureForProvider(context.Context, application.SubsystemAccessOperation, string) (application.SubsystemUpdateBrowserCredential, error) {
	if s.fail {
		return application.SubsystemUpdateBrowserCredential{}, errors.New("provider unavailable")
	}
	return application.SubsystemUpdateBrowserCredential{ClientID: "actual-keycloak-rp", ClientSecret: "actual-keycloak-secret"}, nil
}
func TestAccessOnboardingAndRecoveryUseProviderBrowserCredentials(t *testing.T) {
	service := &deploymentOnboardingStub{result: application.SubsystemOnboardingResult{Application: application.Application{ID: "app"}, Environment: application.Environment{ID: "env"}, OAuthClient: application.OAuthClientView{ClientID: "platform-client"}, PlaintextSecret: "platform-secret", RedirectURI: "https://settlement.example.com/callback", PublicURL: "https://settlement.example.com"}}
	adapter := deploymentAdapterFor(t, service, &deploymentProvisionerStub{}, relatedClientsStub{}, deploymentStateStub{found: true}, issuerStub{})
	adapter.SetProviderBrowserCredentials(accessProviderBrowserStub{})
	op := onboardingOperation()
	op.AuthProvider = "keycloak"
	op.ApplicationID = "app"
	op.EnvironmentID = "env"
	run := &application.SubsystemAccessRunState{}
	if _, err := adapter.ConfigureSubsystemAccessAuthentication(context.Background(), op, run); err != nil {
		t.Fatal(err)
	}
	if run.Credentials.ClientID != "actual-keycloak-rp" || run.Credentials.ClientSecret != "actual-keycloak-secret" {
		t.Fatal("first enrollment used platform RP secret for Keycloak")
	}
	adapter.credentials = &updateCredentialPreparerStub{}
	adapter.browser = &browserCredentialPreparerStub{credential: application.SubsystemUpdateBrowserCredential{ClientID: "wrong-platform", ClientSecret: "wrong-secret"}}
	adapter.publisher = &catalogPublisherPreparerStub{}
	run.Credentials = nil
	if _, err := adapter.rebuildOnboardCredentials(context.Background(), op, run); err != nil {
		t.Fatal(err)
	}
	if run.Credentials.ClientID != "actual-keycloak-rp" || run.Credentials.ClientSecret != "actual-keycloak-secret" {
		t.Fatal("recovered deployment used platform RP secret for Keycloak")
	}
}
