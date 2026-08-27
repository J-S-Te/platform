package http

import (
	"context"
	"errors"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type brokerProvisionerStub struct {
	clientID, secret                   string
	recoveredClientID, recoveredSecret string
	recoverCalls                       int
}

func (stub *brokerProvisionerStub) EnsureKeycloakBroker(context.Context, string) (string, string, error) {
	return stub.clientID, stub.secret, nil
}

func (stub *brokerProvisionerStub) RecoverKeycloakBroker(context.Context, string, string) (string, string, error) {
	stub.recoverCalls++
	return stub.recoveredClientID, stub.recoveredSecret, nil
}

type keycloakControlStub struct {
	verifyErr   error
	ensureCalls []struct{ clientID, secret string }
}

func (stub *keycloakControlStub) EnsureBroker(_ context.Context, clientID, secret string) error {
	stub.ensureCalls = append(stub.ensureCalls, struct{ clientID, secret string }{clientID, secret})
	return nil
}

func (stub *keycloakControlStub) VerifyBrokerExists(context.Context) error { return stub.verifyErr }

func (stub *keycloakControlStub) EnsureClient(context.Context, string, string, string) (keycloakClientResult, error) {
	return keycloakClientResult{}, nil
}

func (stub *keycloakControlStub) EnsureClientRoles(context.Context, string, []string) error {
	return nil
}

func (stub *keycloakControlStub) DetectSubsystemKeycloakDrift(context.Context, string, string, []string) (KeycloakDriftReport, error) {
	return KeycloakDriftReport{}, nil
}

type serviceCredentialManagerStub struct {
	clients       []application.OAuthClientView
	createdInput  application.OAuthClientCreateInput
	secretInput   application.OAuthClientSecretCreateInput
	createdInputs []application.OAuthClientCreateInput
	secretInputs  []application.OAuthClientSecretCreateInput
}

func (stub *serviceCredentialManagerStub) ListOAuthClients(context.Context, string) ([]application.OAuthClientView, error) {
	return stub.clients, nil
}

func (stub *serviceCredentialManagerStub) CreateOAuthClient(_ context.Context, input application.OAuthClientCreateInput) (application.OAuthClientCreateResult, error) {
	stub.createdInput = input
	stub.createdInputs = append(stub.createdInputs, input)
	return application.OAuthClientCreateResult{
		Client:          application.OAuthClientView{ID: input.ClientID + "-id", ClientID: input.ClientID, Status: "ACTIVE"},
		PlaintextSecret: "new-secret",
	}, nil
}

func (stub *serviceCredentialManagerStub) CreateOAuthClientSecret(_ context.Context, input application.OAuthClientSecretCreateInput) (application.OAuthClientSecretResult, error) {
	stub.secretInput = input
	stub.secretInputs = append(stub.secretInputs, input)
	return application.OAuthClientSecretResult{PlaintextSecret: "retry-secret"}, nil
}

func TestEnsureUpdateServiceCredentialsRepairsCustomerAuditAndNotification(t *testing.T) {
	manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{{
		ID: "audit-client-id", ClientID: "customer_and_opportunity-dev-audit-publisher", Status: "ACTIVE",
	}}}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	credentials, err := handler.ensureUpdateServiceCredentials(
		context.Background(), "tenant-1", "application-1", "environment-1",
		"customer_and_opportunity", "dev", "operator-1", "UPDATE",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.secretInputs) != 1 || manager.secretInputs[0].OAuthClientID != "audit-client-id" {
		t.Fatalf("audit credential was not rotated: %#v", manager.secretInputs)
	}
	if len(manager.createdInputs) != 1 || manager.createdInputs[0].ClientID != "customer_and_opportunity-dev-notification-publisher" ||
		len(manager.createdInputs[0].Scopes) != 1 || manager.createdInputs[0].Scopes[0] != "notification.ingest" {
		t.Fatalf("notification credential was not created: %#v", manager.createdInputs)
	}
	if len(credentials) != 2 || credentials[0].Purpose != application.ServiceCredentialAuditIngest ||
		credentials[1].Purpose != application.ServiceCredentialNotificationIngest {
		t.Fatalf("unexpected customer credentials: %#v", credentials)
	}
}

func TestEnsureWebOAuthClientCreatesMissingAuthorizationCodeClient(t *testing.T) {
	manager := &serviceCredentialManagerStub{}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	client, err := handler.ensureWebOAuthClient(context.Background(), "tenant-1", "app-1", "env-1", "operator-1", "data_analysis-prod-web", "data_analysis", "http://dashboard.example/data_analysis/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientID != "data_analysis-prod-web" || len(manager.createdInputs) != 1 {
		t.Fatalf("unexpected created web client: client=%#v inputs=%#v", client, manager.createdInputs)
	}
	input := manager.createdInputs[0]
	if input.ClientType != "confidential" || input.TokenAuthMethod != "client_secret_basic" || !input.RequirePKCE || len(input.GrantTypes) != 1 || input.GrantTypes[0] != "authorization_code" || len(input.RedirectURIs) != 1 {
		t.Fatalf("web client must use authorization code + PKCE: %#v", input)
	}
}

func TestEnsureWebOAuthClientReusesActiveBoundClient(t *testing.T) {
	manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{{
		ID: "web-client-id", ClientID: "data_analysis-prod-web", ApplicationID: "app-1", EnvironmentID: "env-1", Status: "ACTIVE",
	}}}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	client, err := handler.ensureWebOAuthClient(context.Background(), "tenant-1", "app-1", "env-1", "operator-1", "data_analysis-prod-web", "data_analysis", "http://dashboard.example/data_analysis/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if client.ID != "web-client-id" || len(manager.createdInputs) != 0 {
		t.Fatalf("active web client should be reused: client=%#v inputs=%#v", client, manager.createdInputs)
	}
}

func TestReconcileKeycloakBrokerRotatesOnlyAfterConfirmedIncompleteConfiguration(t *testing.T) {
	broker := &brokerProvisionerStub{clientID: "keycloak-broker", recoveredClientID: "keycloak-broker", recoveredSecret: "one-time-secret"}
	control := &keycloakControlStub{verifyErr: &keycloakBrokerConfigurationError{alias: "basic-platform", missingKey: "clientSecret"}}
	handler := &SubsystemOnboardingHandler{keycloakBroker: broker, keycloakControl: control}

	if err := handler.reconcileKeycloakBroker(context.Background(), "tenant-1", "operator-1"); err != nil {
		t.Fatal(err)
	}
	if broker.recoverCalls != 1 {
		t.Fatalf("recovery calls = %d, want 1", broker.recoverCalls)
	}
	if len(control.ensureCalls) != 1 || control.ensureCalls[0].clientID != "keycloak-broker" || control.ensureCalls[0].secret != "one-time-secret" {
		t.Fatalf("recovered secret was not reconciled: %#v", control.ensureCalls)
	}
}

func TestReconcileKeycloakBrokerRecreatesMissingIdentityProvider(t *testing.T) {
	broker := &brokerProvisionerStub{clientID: "keycloak-broker", recoveredClientID: "keycloak-broker", recoveredSecret: "one-time-secret"}
	control := &keycloakControlStub{verifyErr: &keycloakBrokerConfigurationError{alias: "basic-platform", missingKey: "identity-provider"}}
	handler := &SubsystemOnboardingHandler{keycloakBroker: broker, keycloakControl: control}

	if err := handler.reconcileKeycloakBroker(context.Background(), "tenant-1", "operator-1"); err != nil {
		t.Fatal(err)
	}
	if broker.recoverCalls != 1 {
		t.Fatalf("recovery calls = %d, want 1", broker.recoverCalls)
	}
	if len(control.ensureCalls) != 1 || control.ensureCalls[0].clientID != "keycloak-broker" || control.ensureCalls[0].secret != "one-time-secret" {
		t.Fatalf("missing IdP was not recreated: %#v", control.ensureCalls)
	}
}

func TestReconcileKeycloakBrokerDoesNotRotateForTransientFailures(t *testing.T) {
	transient := errors.New("Keycloak admin API unavailable")
	broker := &brokerProvisionerStub{clientID: "keycloak-broker", recoveredClientID: "keycloak-broker", recoveredSecret: "must-not-be-used"}
	control := &keycloakControlStub{verifyErr: transient}
	handler := &SubsystemOnboardingHandler{keycloakBroker: broker, keycloakControl: control}

	if err := handler.reconcileKeycloakBroker(context.Background(), "tenant-1", "operator-1"); !errors.Is(err, transient) {
		t.Fatalf("error = %v, want transient failure", err)
	}
	if broker.recoverCalls != 0 || len(control.ensureCalls) != 0 {
		t.Fatalf("transient failure must not rotate or write broker: recoveries=%d writes=%#v", broker.recoverCalls, control.ensureCalls)
	}
}

func TestEnsureUpdateServiceCredentialsBackfillsContractOwnerDirectory(t *testing.T) {
	manager := &serviceCredentialManagerStub{}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	credentials, err := handler.ensureUpdateServiceCredentials(
		context.Background(), "tenant-1", "application-1", "environment-1",
		"contract_management", "prod", "operator-1", "UPDATE",
	)
	if err != nil {
		t.Fatal(err)
	}
	if manager.createdInput.ClientID != "contract_management-prod-owner-directory" ||
		len(manager.createdInput.Scopes) != 1 || manager.createdInput.Scopes[0] != "owner_directory.read" {
		t.Fatalf("unexpected client input: %#v", manager.createdInput)
	}
	if len(credentials) != 1 || credentials[0].Purpose != application.ServiceCredentialOwnerDirectoryRead || credentials[0].PlaintextSecret != "new-secret" {
		t.Fatalf("unexpected credentials: %#v", credentials)
	}
}

func TestEnsureUpdateServiceCredentialsRetryIssuesRecoverableSecret(t *testing.T) {
	manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{{
		ID: "client-1", ClientID: "contract_management-prod-owner-directory", Status: "ACTIVE",
	}}}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	credentials, err := handler.ensureUpdateServiceCredentials(
		context.Background(), "tenant-1", "application-1", "environment-1",
		"contract_management", "prod", "operator-1", "RETRY",
	)
	if err != nil {
		t.Fatal(err)
	}
	if manager.secretInput.OAuthClientID != "client-1" || len(credentials) != 1 || credentials[0].PlaintextSecret != "retry-secret" {
		t.Fatalf("retry did not issue a replacement delivery secret: input=%#v credentials=%#v", manager.secretInput, credentials)
	}
}

func TestEnsureUpdateServiceCredentialsRepairsCustomerPortalBindings(t *testing.T) {
	manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{
		{ID: "mapping-client-id", ClientID: "customer_portal-prod-portal-mapping-provision", Status: "ACTIVE"},
	}}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	credentials, err := handler.ensureUpdateServiceCredentials(
		context.Background(), "tenant-1", "application-1", "environment-1",
		"customer_portal", "prod", "operator-1", "UPDATE",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.secretInputs) != 1 || manager.secretInputs[0].OAuthClientID != "mapping-client-id" {
		t.Fatalf("existing portal mapping credential was not redelivered: %#v", manager.secretInputs)
	}
	if len(manager.createdInputs) != 5 {
		t.Fatalf("portal binding clients were not backfilled: %#v", manager.createdInputs)
	}
	wantPurposes := map[string]bool{
		application.ServiceCredentialExternalUserProvision:  false,
		application.ServiceCredentialApplicationRoleAssign:  false,
		application.ServiceCredentialApplicationRoleRevoke:  false,
		application.ServiceCredentialPortalMappingProvision: true,
		application.ServiceCredentialPortalMappingDisable:   false,
		application.ServiceCredentialPortalInviteVerify:     false,
	}
	for _, credential := range credentials {
		if _, ok := wantPurposes[credential.Purpose]; !ok {
			t.Fatalf("unexpected portal credential purpose: %#v", credential)
		}
		wantPurposes[credential.Purpose] = true
	}
	for purpose, present := range wantPurposes {
		if !present {
			t.Fatalf("portal credential %q was not delivered: %#v", purpose, credentials)
		}
	}
}
