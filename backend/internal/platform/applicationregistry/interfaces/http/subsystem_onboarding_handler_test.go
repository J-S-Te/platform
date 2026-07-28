package http

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
)

func TestOnboardSubsystemDoesNotReturnSecretOrDeploymentInstructions(t *testing.T) {
	t.Parallel()
	pathPrefix := "/contract_management"
	upstreamURL := "http://contract-api:8081"
	service := &stubSubsystemOnboardingService{result: application.SubsystemOnboardingResult{
		Application:     application.Application{ID: "app-1", TenantID: "tenant-1", Code: "contract_management", Name: "合同管理系统", Status: "ACTIVE"},
		Environment:     application.Environment{ID: "env-1", TenantID: "tenant-1", ApplicationID: "app-1", Environment: "dev", PathPrefix: &pathPrefix, UpstreamURL: &upstreamURL, Status: "ACTIVE"},
		OAuthClient:     application.OAuthClientView{ID: "client-1", ClientID: "contract_management-dev-web", ClientName: "合同管理系统 Web", ClientType: "confidential", Status: "ACTIVE"},
		PlaintextSecret: "must-never-reach-browser",
		RedirectURI:     "http://localhost:8081/contract_management/auth/callback",
		PublicURL:       "http://localhost:8081/contract_management/",
	}}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(service, provisioner, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}

	requestBody := `{"application_code":"contract_management","application_name":"合同管理系统","environment":"dev","public_base_url":"http://localhost:8081","upstream_url":"http://contract-api:8081","path_prefix":"/contract_management","client_type":"confidential"}`
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-onboarding", bytes.NewBufferString(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.OnboardSubsystem(response, request)
	if response.Code != stdhttp.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{"must-never-reach-browser", `"integration"`, "environment_file", "gateway_command", "OIDC_CLIENT_SECRET"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"automation"`) || !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("response missing safe automation status: %s", body)
	}
	if provisioner.input.ClientSecret != "must-never-reach-browser" {
		t.Fatalf("deployment helper did not receive generated secret")
	}
}

type stubSubsystemOnboardingService struct {
	result application.SubsystemOnboardingResult
}

func (service *stubSubsystemOnboardingService) OnboardSubsystem(context.Context, application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error) {
	return service.result, nil
}

func (*stubSubsystemOnboardingService) ListPortalApplications(context.Context, string, string) ([]application.PortalApplication, error) {
	return nil, nil
}

type recordingHTTPSubsystemProvisioner struct {
	input application.SubsystemProvisioningInput
}

func (*recordingHTTPSubsystemProvisioner) Preflight(context.Context, string) error {
	return nil
}

func (provisioner *recordingHTTPSubsystemProvisioner) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.input = input
	return nil
}
