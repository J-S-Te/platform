package http

import (
	"context"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type serviceCredentialManagerStub struct {
	clients      []application.OAuthClientView
	createdInput application.OAuthClientCreateInput
	secretInput  application.OAuthClientSecretCreateInput
}

func (stub *serviceCredentialManagerStub) ListOAuthClients(context.Context, string) ([]application.OAuthClientView, error) {
	return stub.clients, nil
}

func (stub *serviceCredentialManagerStub) CreateOAuthClient(_ context.Context, input application.OAuthClientCreateInput) (application.OAuthClientCreateResult, error) {
	stub.createdInput = input
	return application.OAuthClientCreateResult{
		Client:          application.OAuthClientView{ID: "client-1", ClientID: input.ClientID, Status: "ACTIVE"},
		PlaintextSecret: "new-secret",
	}, nil
}

func (stub *serviceCredentialManagerStub) CreateOAuthClientSecret(_ context.Context, input application.OAuthClientSecretCreateInput) (application.OAuthClientSecretResult, error) {
	stub.secretInput = input
	return application.OAuthClientSecretResult{PlaintextSecret: "retry-secret"}, nil
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
