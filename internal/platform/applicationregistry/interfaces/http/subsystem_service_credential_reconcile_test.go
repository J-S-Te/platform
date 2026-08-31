package http

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func requireCreatedClient(t *testing.T, inputs []application.OAuthClientCreateInput, clientID string, scopes []string) application.OAuthClientCreateInput {
	t.Helper()
	for _, input := range inputs {
		if input.ClientID != clientID {
			continue
		}
		if !reflect.DeepEqual(input.Scopes, scopes) {
			t.Fatalf("client %q scopes = %#v, want %#v", clientID, input.Scopes, scopes)
		}
		return input
	}
	t.Fatalf("client %q was not created: %#v", clientID, inputs)
	return application.OAuthClientCreateInput{}
}

func requireDeliveredCredential(t *testing.T, credentials []application.SubsystemServiceCredential, purpose, secret string) application.SubsystemServiceCredential {
	t.Helper()
	for _, credential := range credentials {
		if credential.Purpose != purpose {
			continue
		}
		if secret != "" && credential.PlaintextSecret != secret {
			t.Fatalf("credential %q secret marker = %q, want %q", purpose, credential.PlaintextSecret, secret)
		}
		return credential
	}
	t.Fatalf("credential purpose %q was not delivered: %#v", purpose, credentials)
	return application.SubsystemServiceCredential{}
}

func requireOnlyCreatedClients(t *testing.T, inputs []application.OAuthClientCreateInput, want map[string][]string) {
	t.Helper()
	seen := make(map[string]bool, len(inputs))
	for _, input := range inputs {
		scopes, ok := want[input.ClientID]
		if !ok {
			t.Fatalf("unexpected client %q was created: %#v", input.ClientID, inputs)
		}
		if seen[input.ClientID] {
			t.Fatalf("client %q was created more than once: %#v", input.ClientID, inputs)
		}
		seen[input.ClientID] = true
		if !reflect.DeepEqual(input.Scopes, scopes) {
			t.Fatalf("client %q scopes = %#v, want %#v", input.ClientID, input.Scopes, scopes)
		}
	}
	for clientID, scopes := range want {
		if !seen[clientID] {
			requireCreatedClient(t, inputs, clientID, scopes)
		}
	}
}

func requireOnlyCredentialPurposes(t *testing.T, credentials []application.SubsystemServiceCredential, want map[string]string) {
	t.Helper()
	seen := make(map[string]bool, len(credentials))
	for _, credential := range credentials {
		secret, ok := want[credential.Purpose]
		if !ok {
			t.Fatalf("unexpected credential purpose %q: %#v", credential.Purpose, credentials)
		}
		if seen[credential.Purpose] {
			t.Fatalf("credential purpose %q was delivered more than once: %#v", credential.Purpose, credentials)
		}
		seen[credential.Purpose] = true
		if secret != "" && credential.PlaintextSecret != secret {
			t.Fatalf("credential %q secret marker = %q, want %q", credential.Purpose, credential.PlaintextSecret, secret)
		}
	}
	for purpose, secret := range want {
		if !seen[purpose] {
			requireDeliveredCredential(t, credentials, purpose, secret)
		}
	}
}

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
	scopeInputs   []application.OAuthClientScopesUpdateInput
}

func (stub *serviceCredentialManagerStub) ReplaceOAuthClientScopes(_ context.Context, input application.OAuthClientScopesUpdateInput) (application.OAuthClientView, error) {
	stub.scopeInputs = append(stub.scopeInputs, input)
	for _, client := range stub.clients {
		if client.ID == input.OAuthClientID {
			client.Scopes = append([]string(nil), input.Scopes...)
			client.Version++
			return client, nil
		}
	}
	return application.OAuthClientView{}, application.ErrNotFound
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
		ID: "audit-client-id", ApplicationID: "application-1", EnvironmentID: "environment-1",
		ClientID: "customer_and_opportunity-dev-audit-publisher", Status: "ACTIVE", Version: 1,
		Scopes: []string{"audit.ingest"},
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
	requireOnlyCreatedClients(t, manager.createdInputs, map[string][]string{
		"customer_and_opportunity-dev-notification-publisher": {"notification.ingest"},
		"customer_and_opportunity-dev-owner-directory":        {"owner_directory.read"},
		"customer_and_opportunity-dev-file-gateway-writer":    {"platform:file:upload", "platform:file:bind", "platform:file:download"},
	})
	requireOnlyCredentialPurposes(t, credentials, map[string]string{
		application.ServiceCredentialAuditIngest:        "retry-secret",
		application.ServiceCredentialNotificationIngest: "new-secret",
		application.ServiceCredentialOwnerDirectoryRead: "new-secret",
		application.ServiceCredentialFileGatewayWrite:   "new-secret",
	})
}

func TestEnsureUpdateServiceCredentialsRepairsExistingFileGatewayScopes(t *testing.T) {
	manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{{
		ID: "file-client-id", ApplicationID: "application-1", EnvironmentID: "environment-1",
		ClientID: "settlement_and_invoicing-prod-file-gateway-writer", Status: "ACTIVE", Version: 7,
		Scopes: []string{"platform:file:upload", "platform:file:bind"},
	}}}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}

	credentials, err := handler.ensureUpdateServiceCredentials(
		context.Background(), "tenant-1", "application-1", "environment-1",
		"settlement_and_invoicing", "prod", "operator-1", "UPDATE",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(manager.scopeInputs) != 1 {
		t.Fatalf("scope repairs = %#v, want one repair", manager.scopeInputs)
	}
	if got := manager.scopeInputs[0]; got.Version != 7 || !reflect.DeepEqual(got.Scopes, []string{"platform:file:upload", "platform:file:bind", "platform:file:download"}) {
		t.Fatalf("scope repair = %#v", got)
	}
	if len(manager.secretInputs) != 1 || len(credentials) != 1 || credentials[0].OAuthClient.Scopes[2] != "platform:file:download" {
		t.Fatalf("repaired credential was not rotated and delivered: inputs=%#v credentials=%#v", manager.secretInputs, credentials)
	}
}

func TestEnsureUpdateServiceCredentialsBackfillsSeparatedDashboardReaders(t *testing.T) {
	manager := &serviceCredentialManagerStub{}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	credentials, err := handler.ensureUpdateServiceCredentials(
		context.Background(), "tenant-1", "application-1", "environment-1",
		"data_analysis", "prod", "operator-1", "UPDATE",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"data_analysis-prod-audit-publisher":     {"audit.ingest"},
		"data_analysis-prod-contract-dashboard":  {"dashboard.contract.read"},
		"data_analysis-prod-project-dashboard":   {"dashboard.project.read"},
		"data_analysis-prod-file-gateway-writer": {"platform:file:upload", "platform:file:bind", "platform:file:download"},
	}
	requireOnlyCreatedClients(t, manager.createdInputs, want)
	requireOnlyCredentialPurposes(t, credentials, map[string]string{
		application.ServiceCredentialAuditIngest:           "new-secret",
		application.ServiceCredentialContractDashboardRead: "new-secret",
		application.ServiceCredentialProjectDashboardRead:  "new-secret",
		application.ServiceCredentialFileGatewayWrite:      "new-secret",
	})
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
	requireOnlyCreatedClients(t, manager.createdInputs, map[string][]string{
		"contract_management-prod-owner-directory":     {"owner_directory.read"},
		"contract_management-prod-file-gateway-writer": {"platform:file:upload", "platform:file:bind", "platform:file:download"},
	})
	requireOnlyCredentialPurposes(t, credentials, map[string]string{
		application.ServiceCredentialOwnerDirectoryRead: "new-secret",
		application.ServiceCredentialFileGatewayWrite:   "new-secret",
	})
}

func TestEnsureUpdateServiceCredentialsRetryIssuesRecoverableSecret(t *testing.T) {
	manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{{
		ID: "client-1", ApplicationID: "application-1", EnvironmentID: "environment-1",
		ClientID: "contract_management-prod-owner-directory", Status: "ACTIVE", Version: 1,
		Scopes: []string{"owner_directory.read"},
	}}}
	handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
	credentials, err := handler.ensureUpdateServiceCredentials(
		context.Background(), "tenant-1", "application-1", "environment-1",
		"contract_management", "prod", "operator-1", "RETRY",
	)
	if err != nil {
		t.Fatal(err)
	}
	if manager.secretInput.OAuthClientID != "client-1" {
		t.Fatalf("retry did not issue a replacement delivery secret: input=%#v credentials=%#v", manager.secretInput, credentials)
	}
	requireOnlyCreatedClients(t, manager.createdInputs, map[string][]string{
		"contract_management-prod-file-gateway-writer": {"platform:file:upload", "platform:file:bind", "platform:file:download"},
	})
	requireOnlyCredentialPurposes(t, credentials, map[string]string{
		application.ServiceCredentialOwnerDirectoryRead: "retry-secret",
		application.ServiceCredentialFileGatewayWrite:   "new-secret",
	})
}

func TestEnsureUpdateServiceCredentialsRepairsCustomerPortalBindings(t *testing.T) {
	manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{
		{ID: "mapping-client-id", ApplicationID: "application-1", EnvironmentID: "environment-1",
			ClientID: "customer_portal-prod-portal-mapping-provision", Status: "ACTIVE", Version: 1,
			Scopes: []string{"portal.identity_mapping.provision"}},
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
	wantPurposes := map[string]bool{
		application.ServiceCredentialExternalUserProvision:  false,
		application.ServiceCredentialApplicationRoleAssign:  false,
		application.ServiceCredentialApplicationRoleRevoke:  false,
		application.ServiceCredentialPortalMappingProvision: true,
		application.ServiceCredentialPortalMappingDisable:   false,
		application.ServiceCredentialPortalInviteVerify:     false,
		application.ServiceCredentialFileGatewayWrite:       false,
	}
	requireOnlyCreatedClients(t, manager.createdInputs, map[string][]string{
		"customer_portal-prod-external-user-provision": {"external_user.provision"},
		"customer_portal-prod-role-assign":             {"application_role.assign"},
		"customer_portal-prod-role-revoke":             {"application_role.revoke"},
		"customer_portal-prod-portal-mapping-disable":  {"portal.identity_mapping.disable"},
		"customer_portal-prod-portal-invite-verify":    {"portal.invite.verify"},
		"customer_portal-prod-file-gateway-writer":     {"platform:file:upload", "platform:file:bind", "platform:file:download"},
	})
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
