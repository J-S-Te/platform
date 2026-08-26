package http

import (
	"context"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

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
