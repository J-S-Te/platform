package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		Application:                     application.Application{ID: "app-1", TenantID: "tenant-1", Code: "contract_management", Name: "合同管理系统", Status: "ACTIVE"},
		Environment:                     application.Environment{ID: "env-1", TenantID: "tenant-1", ApplicationID: "app-1", Environment: "dev", PathPrefix: &pathPrefix, UpstreamURL: &upstreamURL, Status: "ACTIVE"},
		OAuthClient:                     application.OAuthClientView{ID: "client-1", ClientID: "contract_management-dev-web", ClientName: "合同管理系统 Web", ClientType: "confidential", Status: "ACTIVE"},
		CatalogPublisherOAuthClient:     application.OAuthClientView{ID: "client-2", ClientID: "contract_management-dev-catalog-publisher", ClientName: "合同管理系统 Authorization Catalog Publisher", ClientType: "service", TokenAuthMethod: "client_secret_basic", Status: "ACTIVE"},
		PlaintextSecret:                 "must-never-reach-browser",
		CatalogPublisherPlaintextSecret: "catalog-publisher-secret-must-never-reach-browser",
		RedirectURI:                     "http://localhost:8081/contract_management/auth/callback",
		PublicURL:                       "http://localhost:8081/contract_management/",
	}}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	access := &recordingSubsystemAccessManager{roleCode: "admin"}
	handler, err := NewSubsystemOnboardingHandler(service, provisioner, access, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	for _, forbidden := range []string{"must-never-reach-browser", "catalog-publisher-secret-must-never-reach-browser", `"integration"`, "environment_file", "gateway_command", "OIDC_CLIENT_SECRET", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"automation"`) || !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("response missing safe automation status: %s", body)
	}
	if !strings.Contains(body, `"authorization"`) || !strings.Contains(body, `"initial_admin_user_id":"user-1"`) || !strings.Contains(body, `"role_code":"admin"`) {
		t.Fatalf("response missing explicit initial administrator assignment: %s", body)
	}
	if access.userID != "user-1" || access.operatorID != "user-1" || access.applicationCode != "contract_management" {
		t.Fatalf("unexpected access assignment: %#v", access)
	}
	if provisioner.input.ApplicationID != "app-1" || provisioner.input.ClientSecret != "must-never-reach-browser" {
		t.Fatalf("deployment helper did not receive browser OIDC integration: %#v", provisioner.input)
	}
	if provisioner.input.CatalogPublisherClientID != "contract_management-dev-catalog-publisher" || provisioner.input.CatalogPublisherClientSecret != "catalog-publisher-secret-must-never-reach-browser" {
		t.Fatalf("deployment helper did not receive isolated catalog publisher integration: %#v", provisioner.input)
	}
}

func TestOnboardSubsystemExistingEnvironmentReturnsActionableConflict(t *testing.T) {
	t.Parallel()
	service := &stubSubsystemOnboardingService{err: &application.SubsystemOnboardingConflict{
		ApplicationCode: "contract_management",
		Environment:     "prod",
		Status:          "ACTIVE",
	}}
	handler, err := NewSubsystemOnboardingHandler(service, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	requestBody := `{"application_code":"contract_management","application_name":"合同管理系统","environment":"prod","public_base_url":"http://localhost:8081","upstream_url":"http://contract-api:8081","path_prefix":"/contract_management","client_type":"confidential"}`
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-onboarding", bytes.NewBufferString(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.OnboardSubsystem(response, request)
	if response.Code != stdhttp.StatusConflict {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		Code    string            `json:"code"`
		Details map[string]string `json:"details"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Code != "IAM_SUBSYSTEM_ALREADY_ONBOARDED" {
		t.Fatalf("code = %q", body.Code)
	}
	if body.Details["application_code"] != "contract_management" || body.Details["environment"] != "prod" || body.Details["status"] != "ACTIVE" {
		t.Fatalf("unexpected details: %#v", body.Details)
	}
	if !strings.Contains(body.Details["next_action"], "日常代码") || !strings.Contains(body.Details["next_action"], "无需执行接入或撤销脚本") {
		t.Fatalf("conflict guidance must keep normal subsystem releases separate from onboarding: %#v", body.Details)
	}
	if !errors.Is(service.err, application.ErrSubsystemOnboardingAlreadyExists) {
		t.Fatal("typed conflict must support errors.Is")
	}
}

type stubSubsystemOnboardingService struct {
	result application.SubsystemOnboardingResult
	err    error
}

func (service *stubSubsystemOnboardingService) OnboardSubsystem(context.Context, application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error) {
	return service.result, service.err
}

func (*stubSubsystemOnboardingService) ListPortalApplications(context.Context, string, string, string) ([]application.PortalApplication, error) {
	return nil, nil
}

type recordingSubsystemAccessManager struct {
	tenantID        string
	applicationCode string
	userID          string
	operatorID      string
	roleCode        string
	err             error
}

func (manager *recordingSubsystemAccessManager) AssignInitialAdministrator(_ context.Context, tenantID, applicationCode, userID, operatorID string) (string, error) {
	manager.tenantID = tenantID
	manager.applicationCode = applicationCode
	manager.userID = userID
	manager.operatorID = operatorID
	return manager.roleCode, manager.err
}

type recordingHTTPSubsystemProvisioner struct {
	input        application.SubsystemProvisioningInput
	teardownCode string
}

func (*recordingHTTPSubsystemProvisioner) Preflight(context.Context, string) error {
	return nil
}

func (provisioner *recordingHTTPSubsystemProvisioner) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.input = input
	return nil
}

func (provisioner *recordingHTTPSubsystemProvisioner) Update(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.input = input
	return nil
}

func (provisioner *recordingHTTPSubsystemProvisioner) Teardown(_ context.Context, applicationCode, _ string) error {
	provisioner.teardownCode = applicationCode
	return nil
}

func TestUpdateSubsystemCallsProvisionerWithMinimalInput(t *testing.T) {
	t.Parallel()
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	requestBody := `{"application_code":"contract_management","environment":"prod"}`
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-update", bytes.NewBufferString(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)
	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if provisioner.input.ApplicationCode != "contract_management" || provisioner.input.Environment != "prod" {
		t.Fatalf("update input = %#v, want minimal {contract_management, prod}", provisioner.input)
	}
	// Update MUST NOT carry the OAuth secret forward: a re-run of .env.local would need the
	// bcrypt-hashed plaintext, which the service has not retained.
	if provisioner.input.ClientSecret != "" || provisioner.input.CatalogPublisherClientSecret != "" {
		t.Fatalf("update must not carry client secrets: %#v", provisioner.input)
	}
}

func TestUpdateSubsystemRejectsMissingFields(t *testing.T) {
	t.Parallel()
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	requestBody := `{"application_code":"","environment":"prod"}`
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-update", bytes.NewBufferString(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)
	if response.Code != stdhttp.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if provisioner.input.ApplicationCode != "" {
		t.Fatalf("update must not be invoked on validation failure: %#v", provisioner.input)
	}
}

func TestTeardownSubsystemCallsProvisionerAndAcknowledgesDeepCleanup(t *testing.T) {
	t.Parallel()
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	requestBody := `{"application_code":"contract_management","environment":"prod"}`
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-teardown", bytes.NewBufferString(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"},
		User:   authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.TeardownSubsystem(response, request)
	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if provisioner.teardownCode != "contract_management" {
		t.Fatalf("teardown code = %q, want contract_management", provisioner.teardownCode)
	}
	if !strings.Contains(response.Body.String(), `"status":"torn_down"`) {
		t.Fatalf("response missing torn_down status: %s", response.Body.String())
	}
}
