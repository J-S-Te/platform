package http

import (
	"context"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

type initialKeycloakControl struct{ keycloakControlStub }

func (*initialKeycloakControl) EnsureClient(_ context.Context, id, _, _ string) (keycloakClientResult, error) {
	return keycloakClientResult{ClientID: id, ClientSecret: "keycloak-initial-secret"}, nil
}

func TestInitialPlatformBrowserCredentialIsBoundAndWriteOnly(t *testing.T) {
	for _, kind := range []string{"existing", "missing", "wrong-binding", "disabled", "service-client", "wrong-redirect", "no-pkce"} {
		t.Run(kind, func(t *testing.T) {
			client := application.OAuthClientView{ID: "web-id", ClientID: "contract_management-prod-web", ApplicationID: "app", EnvironmentID: "env", ClientType: "confidential", TokenAuthMethod: "client_secret_basic", Status: "ACTIVE", GrantTypes: []string{"authorization_code"}, RequirePKCE: true, RedirectURIs: []string{"https://platform.example.com/contract_management/auth/callback"}}
			manager := &serviceCredentialManagerStub{clients: []application.OAuthClientView{client}}
			switch kind {
			case "missing":
				manager.clients = nil
			case "wrong-binding":
				manager.clients[0].EnvironmentID = "other"
			case "disabled":
				manager.clients[0].Status = "DISABLED"
			case "service-client":
				manager.clients[0].ClientType = "service"
			case "wrong-redirect":
				manager.clients[0].RedirectURIs = []string{"https://other.example.com/callback"}
			case "no-pkce":
				manager.clients[0].RequirePKCE = false
			}
			h := &SubsystemOnboardingHandler{serviceCredentials: manager}
			_, secret, err := h.ensureInitialPlatformBrowserCredential(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator", "https://platform.example.com/contract_management/auth/callback")
			valid := kind == "existing" || kind == "missing"
			if (err == nil) != valid || (valid && secret == "") {
				t.Fatal("initial credential binding failure", err)
			}
			if !valid && (len(manager.secretInputs) != 0 || len(manager.createdInputs) != 0) {
				t.Fatal("invalid binding minted credential")
			}
			if kind == "missing" && (!manager.createdInput.RequirePKCE || manager.createdInput.ApplicationID != "app" || manager.createdInput.EnvironmentID != "env") {
				t.Fatal("new browser client not bound")
			}
			if kind == "missing" && (manager.createdInput.RefreshTokenTTLSeconds != 30*24*60*60 || !containsExactString(manager.createdInput.GrantTypes, "refresh_token")) {
				t.Fatal("managed browser client cannot issue the required refresh token")
			}
		})
	}
}

func TestReviewedInitialRuntimeFlagIsServerDerived(t *testing.T) {
	for _, operation := range []string{"ADOPT", "RETRY", "UPDATE"} {
		for _, published := range []bool{false, true} {
			state := application.SubsystemDeploymentState{ApplicationID: "app", EnvironmentID: "env", Status: application.SubsystemDeploymentStatusFailed}
			if published {
				state.AppliedManifestChecksum = "sha256:old"
			}
			store := &recordingSubsystemDeploymentStateStore{state: state}
			provisioner := &recordingHTTPSubsystemProvisioner{capabilities: application.SubsystemProvisioningCapabilities{Mode: "production", Targets: []application.SubsystemProvisioningTarget{{ApplicationCode: "contract_management", Environment: "prod", ManifestChecksum: "sha256:approved"}}}}
			h, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), store)
			if err != nil {
				t.Fatal(err)
			}
			h.serviceCredentials = &serviceCredentialManagerStub{clients: []application.OAuthClientView{{ID: "web", ClientID: "contract_management-prod-web", ApplicationID: "app", EnvironmentID: "env", ClientType: "confidential", TokenAuthMethod: "client_secret_basic", Status: "ACTIVE", GrantTypes: []string{"authorization_code"}, RequirePKCE: true, RedirectURIs: []string{"http://localhost:8081/contract_management/auth/callback"}}}}
			path := "/api/v1/subsystem-update"
			if operation == "RETRY" {
				path = "/api/v1/subsystem-retry"
			}
			r := lifecycletestRequest(t, path, `{"application_code":"contract_management","environment":"prod","public_base_url":"http://localhost:8081","upstream_url":"http://contract-api:8081","path_prefix":"/contract_management"}`)
			w := httptest.NewRecorder()
			if operation == "ADOPT" {
				h.AdoptSubsystem(w, r)
			} else {
				h.UpdateSubsystem(w, r)
			}
			h.waitForDeploymentJobs()
			if w.Code != stdhttp.StatusAccepted {
				t.Fatalf("%s published=%v: %d %s", operation, published, w.Code, w.Body.String())
			}
			want := !published && operation != "UPDATE"
			if provisioner.input.InitialRuntimeProvisioning != want {
				t.Fatal("untrusted initial flag", operation, published)
			}
			if provisioner.input.AuthenticationProvider != "platform" {
				t.Fatal("platform runtime provider not delivered", operation, published)
			}
			if want && provisioner.input.ClientSecret == "" {
				t.Fatal("initial secret not delivered")
			}
			if strings.Contains(w.Body.String(), "retry-secret") {
				t.Fatal("secret returned to browser")
			}
		}
	}
}

func TestReviewedKeycloakAdoptionSuppliesInitialRuntimeCredentials(t *testing.T) {
	store := &keycloakIssuerStateStore{recordingSubsystemDeploymentStateStore: recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{ApplicationID: "app", EnvironmentID: "env", Status: application.SubsystemDeploymentStatusFailed}}, issuerAlias: "keycloak"}
	provisioner := &recordingHTTPSubsystemProvisioner{capabilities: application.SubsystemProvisioningCapabilities{Mode: "production", Targets: []application.SubsystemProvisioningTarget{{ApplicationCode: "contract_management", Environment: "prod", ManifestChecksum: "sha256:approved"}}}}
	h, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), store)
	if err != nil {
		t.Fatal(err)
	}
	h.keycloakEnabled, h.keycloakIssuer, h.keycloakRealm = true, "http://sso.example.com/realms/test", "test"
	h.defaultIssuerAlias = "keycloak"
	h.keycloakControl = &initialKeycloakControl{}
	h.serviceCredentials = &serviceCredentialManagerStub{}
	w := httptest.NewRecorder()
	r := lifecycletestRequest(t, "/api/v1/subsystem-adopt", `{"application_code":"contract_management","environment":"prod","public_base_url":"http://localhost:8081","upstream_url":"http://contract-api:8081","path_prefix":"/contract_management"}`)
	h.AdoptSubsystem(w, r)
	h.waitForDeploymentJobs()
	if w.Code != stdhttp.StatusAccepted || !provisioner.input.InitialRuntimeProvisioning || provisioner.input.ClientSecret != "keycloak-initial-secret" {
		t.Fatalf("initial Keycloak adoption failed: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "keycloak-initial-secret") {
		t.Fatal("Keycloak secret exposed to browser")
	}
}

func TestBrowserCannotRequestInitialRuntimeProvisioning(t *testing.T) {
	h, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := lifecycletestRequest(t, "/api/v1/subsystem-update", `{"application_code":"contract_management","environment":"prod","initial_runtime_provisioning":true}`)
	h.UpdateSubsystem(w, r)
	if w.Code < 400 {
		t.Fatal("browser bound server-only field")
	}
}

func TestBrowserCannotRequestAuthenticationProvider(t *testing.T) {
	h, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/subsystem-update", "/api/v1/subsystem-retry", "/api/v1/subsystem-adoption"} {
		w := httptest.NewRecorder()
		r := lifecycletestRequest(t, path, `{"application_code":"contract_management","environment":"prod","authentication_provider":"keycloak"}`)
		h.UpdateSubsystem(w, r)
		if w.Code < 400 {
			t.Fatalf("%s browser bound server-only provider", path)
		}
	}
}

func TestAdoptionRedeliversEveryExistingServiceCredential(t *testing.T) {
	for _, operation := range []string{"ADOPT", "RETRY", "UPDATE"} {
		t.Run(operation, func(t *testing.T) {
			requirements := updateServiceCredentialRequirements("contract_management")
			manager := &serviceCredentialManagerStub{}
			for _, requirement := range requirements {
				manager.clients = append(manager.clients, application.OAuthClientView{ID: requirement.suffix,
					ClientID: "contract_management-prod-" + requirement.suffix, ApplicationID: "app", EnvironmentID: "env",
					TenantID: "tenant", ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"},
					Status: "ACTIVE", Scopes: serviceCredentialScopes(requirement)})
			}
			h := &SubsystemOnboardingHandler{serviceCredentials: manager}
			credentials, err := h.ensureUpdateServiceCredentials(context.Background(), "tenant", "app", "env", "contract_management", "prod", "operator", operation)
			if err != nil {
				t.Fatal(err)
			}
			if operation != "UPDATE" && len(credentials) != len(requirements) {
				t.Fatal("initial adoption/retry lost a required service credential")
			}
			for _, credential := range credentials {
				if credential.PlaintextSecret == "" {
					t.Fatal("service credential not delivered")
				}
				if operation == "UPDATE" && credential.Purpose == application.ServiceCredentialOwnerDirectoryRead {
					t.Fatal("ordinary update rotated the nonrotating owner directory credential")
				}
			}
		})
	}
}
