package http

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

func TestP1ControlledRetryPinsVersionOperatorAndPreservesMetadataOnUpdate(t *testing.T) {
	for _, app := range []string{"contract_management", "customer_portal"} {
		t.Run(app, func(t *testing.T) {
			manager := &serviceCredentialManagerStub{}
			for _, requirement := range updateServiceCredentialRequirements(app) {
				manager.clients = append(manager.clients, application.OAuthClientView{ID: "uid-" + requirement.Suffix, TenantID: "tenant", ApplicationID: "app", EnvironmentID: "env", ClientID: app + "-prod-" + requirement.Suffix, Status: "ACTIVE", Version: 7, ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: serviceCredentialScopes(requirement)})
			}
			handler := &SubsystemOnboardingHandler{serviceCredentials: manager}
			metadata, err := handler.ensureUpdateServiceCredentials(context.Background(), "tenant", "app", "env", app, "prod", "operator", "UPDATE")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, credential := range metadata {
				if p1ServicePurpose(credential.Purpose) {
					count++
					if credential.PlaintextSecret != "" || credential.OAuthClient.Version != 7 {
						t.Fatal("normal update did not preserve registered metadata/secret")
					}
				}
			}
			want := 1
			if app == "customer_portal" {
				want = 2
			}
			if count != want {
				t.Fatal("new purpose metadata lost")
			}
			for _, input := range manager.secretInputs {
				for _, client := range manager.clients {
					if client.ID == input.OAuthClientID && ((app == "contract_management" && client.ClientID == "contract_management-prod-settlement-integration") || (app == "customer_portal" && (client.ClientID == "customer_portal-prod-portal-report-source" || client.ClientID == "customer_portal-prod-portal-report-identity"))) {
						t.Fatal("ordinary UPDATE rotated new purpose")
					}
				}
			}
			manager.secretInputs = nil
			delivered, err := handler.ensureUpdateServiceCredentials(context.Background(), "tenant", "app", "env", app, "prod", "operator", "RETRY")
			if err != nil {
				t.Fatal(err)
			}
			for _, credential := range delivered {
				if !p1ServicePurpose(credential.Purpose) {
					continue
				}
				matched := false
				for _, input := range manager.secretInputs {
					if input.OAuthClientID == credential.OAuthClient.ID {
						matched = true
						if input.TenantID != "tenant" || input.OperatorID != "operator" || input.ExpectedClientVersion == nil || *input.ExpectedClientVersion != 7 {
							t.Fatal("targeted retry lacks persisted target/operator/CAS")
						}
					}
				}
				if !matched || credential.PlaintextSecret == "" {
					t.Fatal("explicit retry failed secret redelivery")
				}
			}
		})
	}
}

func TestP1MissingContractSourceDependencyHasSafeControlledPreparationGuidance(t *testing.T) {
	handler := &SubsystemOnboardingHandler{service: &stubSubsystemOnboardingService{}, serviceCredentials: &serviceCredentialManagerStub{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := handler.relatedServiceClients(context.Background(), "tenant", "settlement", "prod")
	if err == nil {
		t.Fatal("missing registered source allowed")
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/api/v1/subsystem-update", nil)
	handler.writeError(response, request, err)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "IAM_SUBSYSTEM_CONTRACT_DEPENDENCY_REQUIRED") || !strings.Contains(response.Body.String(), "settlement_contract_ingest") {
		t.Fatal("dependency response lacks safe actionable guidance")
	}
	if len(handler.serviceCredentials.(*serviceCredentialManagerStub).secretInputs) != 0 {
		t.Fatal("dependency preparation minted a secret")
	}
}
