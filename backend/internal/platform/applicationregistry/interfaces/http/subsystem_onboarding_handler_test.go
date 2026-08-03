package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/backend/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/backend/internal/shared/authctx"
)

func TestOnboardSubsystemDoesNotReturnSecretOrDeploymentInstructions(t *testing.T) {
	t.Parallel()
	pathPrefix := "/contract_management"
	upstreamURL := "http://contract-api:8081"
	service := &stubSubsystemOnboardingService{result: application.SubsystemOnboardingResult{
		Application:                     application.Application{ID: "app-1", TenantID: "01K10A00000000000000000001", Code: "contract_management", Name: "合同管理系统", Status: "ACTIVE"},
		Environment:                     application.Environment{ID: "env-1", TenantID: "01K10A00000000000000000001", ApplicationID: "app-1", Environment: "dev", PathPrefix: &pathPrefix, UpstreamURL: &upstreamURL, Status: "ACTIVE"},
		OAuthClient:                     application.OAuthClientView{ID: "client-1", ClientID: "contract_management-dev-web", ClientName: "合同管理系统 Web", ClientType: "confidential", Status: "ACTIVE"},
		CatalogPublisherOAuthClient:     application.OAuthClientView{ID: "client-2", ClientID: "contract_management-dev-catalog-publisher", ClientName: "合同管理系统 Authorization Catalog Publisher", ClientType: "service", TokenAuthMethod: "client_secret_basic", Status: "ACTIVE"},
		PlaintextSecret:                 "must-never-reach-browser",
		CatalogPublisherPlaintextSecret: "catalog-publisher-secret-must-never-reach-browser",
		RedirectURI:                     "http://localhost:8081/contract_management/auth/callback",
		PublicURL:                       "http://localhost:8081/contract_management/",
		ServiceCredentials: []application.SubsystemServiceCredential{{
			Purpose:         application.ServiceCredentialAuditIngest,
			OAuthClient:     application.OAuthClientView{ClientID: "contract_management-dev-audit-publisher"},
			PlaintextSecret: "audit-secret-must-never-reach-browser",
		}},
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
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.OnboardSubsystem(response, request)
	if response.Code != stdhttp.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{"must-never-reach-browser", "catalog-publisher-secret-must-never-reach-browser", "audit-secret-must-never-reach-browser", `"integration"`, "environment_file", "gateway_command", "OIDC_CLIENT_SECRET", "PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"automation"`) || !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("response missing safe automation status: %s", body)
	}
	if !strings.Contains(body, `"authorization"`) || !strings.Contains(body, `"initial_admin_user_id":"01K10B00000000000000000001"`) || !strings.Contains(body, `"role_code":"admin"`) {
		t.Fatalf("response missing explicit initial administrator assignment: %s", body)
	}
	if access.userID != "01K10B00000000000000000001" || access.operatorID != "01K10B00000000000000000001" || access.applicationCode != "contract_management" {
		t.Fatalf("unexpected access assignment: %#v", access)
	}
	if provisioner.input.ApplicationID != "app-1" || provisioner.input.ClientSecret != "must-never-reach-browser" {
		t.Fatalf("deployment helper did not receive browser OIDC integration: %#v", provisioner.input)
	}
	if provisioner.input.CatalogPublisherClientID != "contract_management-dev-catalog-publisher" || provisioner.input.CatalogPublisherClientSecret != "catalog-publisher-secret-must-never-reach-browser" {
		t.Fatalf("deployment helper did not receive isolated catalog publisher integration: %#v", provisioner.input)
	}
	if credential, ok := provisioner.input.ServiceCredential(application.ServiceCredentialAuditIngest); !ok || credential.PlaintextSecret != "audit-secret-must-never-reach-browser" {
		t.Fatalf("deployment helper did not receive isolated audit integration: %#v", provisioner.input.ServiceCredentials)
	}
}

func TestOnboardSubsystemPersistsSelectedAdministratorForRetry(t *testing.T) {
	t.Parallel()
	pathPrefix := "/contract_management"
	upstreamURL := "http://contract-api:8081"
	service := &stubSubsystemOnboardingService{result: application.SubsystemOnboardingResult{
		Application:                 application.Application{ID: "app-1", Code: "contract_management"},
		Environment:                 application.Environment{Environment: "prod", PathPrefix: &pathPrefix, UpstreamURL: &upstreamURL},
		OAuthClient:                 application.OAuthClientView{ClientID: "contract_management-prod-web"},
		CatalogPublisherOAuthClient: application.OAuthClientView{ClientID: "contract_management-prod-catalog-publisher"},
		PlaintextSecret:             "browser-secret", CatalogPublisherPlaintextSecret: "publisher-secret",
		RedirectURI: "http://localhost:8081/contract_management/auth/callback",
		PublicURL:   "http://localhost:8081/contract_management/",
		ServiceCredentials: []application.SubsystemServiceCredential{{
			Purpose:         application.ServiceCredentialAuditIngest,
			OAuthClient:     application.OAuthClientView{ClientID: "contract_management-prod-audit-publisher"},
			PlaintextSecret: "audit-secret",
		}},
	}}
	stateStore := &recordingSubsystemDeploymentStateStore{}
	access := &recordingSubsystemAccessManager{roleCode: "admin"}
	handler, err := NewSubsystemOnboardingHandler(service, &recordingHTTPSubsystemProvisioner{}, access, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-onboarding", bytes.NewBufferString(`{"application_code":"contract_management","application_name":"合同管理系统","environment":"prod","public_base_url":"http://localhost:8081","upstream_url":"http://contract-api:8081","path_prefix":"/contract_management","client_type":"confidential","initial_admin_user_id":"01K10D00000000000000000001"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.OnboardSubsystem(response, request)

	if response.Code != stdhttp.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if service.input.InitialAdminUserID != "01K10D00000000000000000001" || access.userID != service.input.InitialAdminUserID {
		t.Fatalf("selected administrator was not carried through: input=%#v access=%#v", service.input, access)
	}
	if stateStore.initialAccessMarks != 1 {
		t.Fatalf("initial access completion marks = %d", stateStore.initialAccessMarks)
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
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
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

func TestOnboardSubsystemProvisioningFailureReturnsActionableSafeDetail(t *testing.T) {
	t.Parallel()
	provisioner := &recordingHTTPSubsystemProvisioner{preflightErr: fmt.Errorf("%w: subsystem Compose file is unavailable", application.ErrSubsystemProvisioningUnavailable)}
	handler, err := NewSubsystemOnboardingHandler(&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	requestBody := `{"application_code":"customer_and_opportunity","application_name":"客户与商机管理系统","environment":"dev","public_base_url":"http://localhost:8081","upstream_url":"http://customer-api:8090","path_prefix":"/customer-opportunity","client_type":"confidential"}`
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-onboarding", bytes.NewBufferString(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"}}))
	response := httptest.NewRecorder()
	handler.OnboardSubsystem(response, request)
	if response.Code != stdhttp.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "compose.yaml") || strings.Contains(response.Body.String(), "/Users/") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "尚未创建应用环境") || strings.Contains(response.Body.String(), "点击“重试”") {
		t.Fatalf("preflight guidance must ask for resubmission instead of retrying a non-existent environment: %s", response.Body.String())
	}
}

type stubSubsystemOnboardingService struct {
	result      application.SubsystemOnboardingResult
	input       application.SubsystemOnboardingInput
	portalItems []application.PortalApplication
	err         error
}

func (service *stubSubsystemOnboardingService) OnboardSubsystem(_ context.Context, input application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error) {
	service.input = input
	return service.result, service.err
}

func (service *stubSubsystemOnboardingService) ListPortalApplications(context.Context, string, string, string) ([]application.PortalApplication, error) {
	if service.portalItems != nil {
		return service.portalItems, nil
	}
	applicationID := service.result.Application.ID
	if applicationID == "" {
		applicationID = "app-1"
	}
	environment := service.result.Environment.Environment
	if environment == "" {
		environment = "prod"
	}
	code := service.result.Application.Code
	if code == "" {
		code = "contract_management"
	}
	return []application.PortalApplication{{ApplicationID: applicationID, Code: code, Environment: environment}}, nil
}

func TestListPortalApplicationsDisablesUserSpecificResponseCaching(t *testing.T) {
	t.Parallel()
	service := &stubSubsystemOnboardingService{portalItems: []application.PortalApplication{}}
	handler, err := NewSubsystemOnboardingHandler(
		service, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/portal/applications", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.ListPortalApplications(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
	}
	if got := response.Header().Get("Vary"); got != "Cookie" {
		t.Fatalf("Vary = %q", got)
	}
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
	capabilities application.SubsystemProvisioningCapabilities
	preflightErr error
	provisionErr error
	updateErr    error
	teardownErr  error
}

func (provisioner *recordingHTTPSubsystemProvisioner) Capabilities() application.SubsystemProvisioningCapabilities {
	return provisioner.capabilities
}

func (provisioner *recordingHTTPSubsystemProvisioner) Preflight(context.Context, application.SubsystemPreflightInput) error {
	return provisioner.preflightErr
}

func (provisioner *recordingHTTPSubsystemProvisioner) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.input = input
	return provisioner.provisionErr
}

func (provisioner *recordingHTTPSubsystemProvisioner) Update(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.input = input
	return provisioner.updateErr
}

func (provisioner *recordingHTTPSubsystemProvisioner) Teardown(_ context.Context, _ string, applicationCode, _ string) error {
	provisioner.teardownCode = applicationCode
	return provisioner.teardownErr
}

func TestGetSubsystemCapabilitiesReturnsSafeProductionPolicy(t *testing.T) {
	t.Parallel()
	provisioner := &recordingHTTPSubsystemProvisioner{capabilities: application.SubsystemProvisioningCapabilities{
		Enabled:                   true,
		Mode:                      "production",
		SupportedApplicationCodes: []string{"contract_management"},
		SupportedEnvironments:     []string{"prod"},
		Targets: []application.SubsystemProvisioningTarget{{
			ApplicationCode: "contract_management", ApplicationName: "合同管理系统",
			Description: "合同创建与审批", Environment: "prod", UpstreamURL: "http://contract-api:8081",
			PathPrefix: "/contract_management", ClientType: "confidential",
		}},
		DefaultApplicationCode: "contract_management",
		DefaultApplicationName: "合同管理系统",
		DefaultDescription:     "合同创建与审批",
		DefaultEnvironment:     "prod",
		DefaultUpstreamURL:     "http://contract-api:8081",
		DefaultPathPrefix:      "/contract_management",
		DefaultClientType:      "confidential",
	}}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{},
		"https://portal.example.com/", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/subsystem-capabilities", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.GetSubsystemCapabilities(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var envelope struct {
		Data struct {
			AutomationEnabled         bool     `json:"automation_enabled"`
			DeploymentMode            string   `json:"deployment_mode"`
			SupportedApplicationCodes []string `json:"supported_application_codes"`
			SupportedEnvironments     []string `json:"supported_environments"`
			Targets                   []struct {
				ApplicationCode string `json:"application_code"`
				Environment     string `json:"environment"`
				PathPrefix      string `json:"path_prefix"`
			} `json:"targets"`
			Defaults struct {
				ApplicationCode string `json:"application_code"`
				Environment     string `json:"environment"`
				PublicBaseURL   string `json:"public_base_url"`
				UpstreamURL     string `json:"upstream_url"`
				PathPrefix      string `json:"path_prefix"`
				ClientType      string `json:"client_type"`
			} `json:"defaults"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data := envelope.Data
	if !data.AutomationEnabled || data.DeploymentMode != "production" ||
		len(data.SupportedApplicationCodes) != 1 || data.SupportedApplicationCodes[0] != "contract_management" ||
		len(data.SupportedEnvironments) != 1 || data.SupportedEnvironments[0] != "prod" {
		t.Fatalf("unexpected production policy: %#v", data)
	}
	if data.Defaults.ApplicationCode != "contract_management" || data.Defaults.Environment != "prod" ||
		data.Defaults.PublicBaseURL != "https://portal.example.com" || data.Defaults.UpstreamURL != "http://contract-api:8081" ||
		data.Defaults.PathPrefix != "/contract_management" || data.Defaults.ClientType != "confidential" {
		t.Fatalf("unexpected production defaults: %#v", data.Defaults)
	}
	if len(data.Targets) != 1 || data.Targets[0].ApplicationCode != "contract_management" ||
		data.Targets[0].Environment != "prod" || data.Targets[0].PathPrefix != "/contract_management" {
		t.Fatalf("unexpected reviewed targets: %#v", data.Targets)
	}
	body := response.Body.String()
	for _, forbidden := range []string{"client_secret", "password", "tenant_id", "socket", "docker.sock", "image_digest", "compose_file", "host_path", "command"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("capability response leaked %q: %s", forbidden, body)
		}
	}
}

func TestSubsystemProvisioningNextActionCoversProductionManifestFailures(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		errMessage string
		want       string
	}{
		"Agent unavailable": {
			errMessage: "subsystem provisioning unavailable: deployment helper is unavailable",
			want:       "subsystem-provisioner",
		},
		"stale Agent target registry": {
			errMessage: "subsystem provisioning unavailable: production subsystem target is not allowed",
			want:       "subsystems.d",
		},
		"CRM runtime secrets": {
			errMessage: "subsystem provisioning unavailable: production subsystem runtime secrets are incomplete",
			want:       "runtime/*.env",
		},
		"CRM image not published": {
			errMessage: "subsystem provisioning unavailable: production subsystem image must use an immutable digest",
			want:       ".release.env",
		},
		"database dependency failed": {
			errMessage: "subsystem provisioning unavailable: start production subsystem dependencies",
			want:       "数据库或依赖服务",
		},
		"migration failed": {
			errMessage: "subsystem provisioning unavailable: migrate production subsystem database",
			want:       "migrate",
		},
		"API health failed": {
			errMessage: "subsystem provisioning unavailable: start production subsystem services",
			want:       "目标 API",
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			nextAction := subsystemProvisioningNextAction(errors.New(test.errMessage))
			if !strings.Contains(nextAction, test.want) {
				t.Fatalf("next action = %q, want substring %q", nextAction, test.want)
			}
			if strings.Contains(nextAction, "请查看平台部署 Agent 与目标 API 的运行日志") {
				t.Fatalf("production failure fell back to the generic next action: %q", nextAction)
			}
		})
	}
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
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
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
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
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
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"},
		User:   authctx.ReferenceName{ID: "01K10B00000000000000000001"},
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

type recordedDeploymentTransition struct {
	tenantID        string
	applicationCode string
	environment     string
	status          string
	operation       string
	errorCode       string
	errorMessage    string
}

type recordingSubsystemDeploymentStateStore struct {
	transitions        []recordedDeploymentTransition
	state              application.SubsystemDeploymentState
	initialAccessMarks int
	transitionErr      error
	getErr             error
	contextErr         error
}

func (store *recordingSubsystemDeploymentStateStore) TransitionSubsystemDeployment(_ context.Context, tenantID, applicationCode, environment, status, operation, errorCode, errorMessage string, _ time.Time) error {
	store.transitions = append(store.transitions, recordedDeploymentTransition{
		tenantID: tenantID, applicationCode: applicationCode, environment: environment,
		status: status, operation: operation, errorCode: errorCode, errorMessage: errorMessage,
	})
	return store.transitionErr
}

func (store *recordingSubsystemDeploymentStateStore) GetSubsystemDeploymentState(context.Context, string, string, string) (application.SubsystemDeploymentState, error) {
	return store.state, store.getErr
}

func (store *recordingSubsystemDeploymentStateStore) GetSubsystemDeploymentContext(context.Context, string, string, string) (application.SubsystemDeploymentState, error) {
	return store.state, store.contextErr
}

func (store *recordingSubsystemDeploymentStateStore) MarkSubsystemInitialAccessAssigned(context.Context, string, string, string, time.Time) error {
	store.initialAccessMarks++
	return nil
}

func TestRetrySubsystemPersistsLifecycleWithoutRepeatingOnboarding(t *testing.T) {
	t.Parallel()
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{ApplicationID: "app-1", InitialAdminUserID: "01K10B00000000000000000001"}}
	access := &recordingSubsystemAccessManager{roleCode: "admin"}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, access,
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-retry", bytes.NewBufferString(`{"application_code":"customer_management","environment":"dev"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10E00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(stateStore.transitions) != 2 {
		t.Fatalf("transitions = %#v, want UPDATING then READY", stateStore.transitions)
	}
	if stateStore.transitions[0].status != application.SubsystemDeploymentStatusUpdating || stateStore.transitions[0].operation != "RETRY" {
		t.Fatalf("start transition = %#v", stateStore.transitions[0])
	}
	if stateStore.transitions[1].status != application.SubsystemDeploymentStatusReady || stateStore.transitions[1].operation != "RETRY" {
		t.Fatalf("terminal transition = %#v", stateStore.transitions[1])
	}
	if access.userID != "01K10B00000000000000000001" || stateStore.initialAccessMarks != 1 {
		t.Fatalf("retry did not complete pending initial access: access=%#v marks=%d", access, stateStore.initialAccessMarks)
	}
	if access.operatorID != "01K10E00000000000000000001" {
		t.Fatalf("retry operator = %q, want current operator", access.operatorID)
	}
	if provisioner.input.ApplicationID != "app-1" || provisioner.input.ApplicationCode != "customer_management" || provisioner.input.Environment != "dev" {
		t.Fatalf("retry did not reuse persisted application context: %#v", provisioner.input)
	}
}

func TestRetrySubsystemDoesNotRestoreAlreadyCompletedInitialAccess(t *testing.T) {
	t.Parallel()
	assignedAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{
		ApplicationID: "app-1", InitialAdminUserID: "01K10D00000000000000000001", InitialAccessAssignedAt: &assignedAt,
	}}
	access := &recordingSubsystemAccessManager{roleCode: "admin"}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, access,
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-retry", bytes.NewBufferString(`{"application_code":"contract_management","environment":"prod"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10E00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if access.userID != "" || stateStore.initialAccessMarks != 0 {
		t.Fatalf("retry unexpectedly restored initial access: access=%#v marks=%d", access, stateStore.initialAccessMarks)
	}
}

func TestUpdateSubsystemFailurePersistsSafeFailureSummary(t *testing.T) {
	t.Parallel()
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{ApplicationID: "app-1", InitialAdminUserID: "01K10B00000000000000000001"}}
	provisioner := &recordingHTTPSubsystemProvisioner{updateErr: application.ErrSubsystemProvisioningUnavailable}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-update", bytes.NewBufferString(`{"application_code":"customer_management","environment":"dev"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)

	if response.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(stateStore.transitions) != 2 || stateStore.transitions[1].status != application.SubsystemDeploymentStatusFailed {
		t.Fatalf("transitions = %#v", stateStore.transitions)
	}
	failed := stateStore.transitions[1]
	if failed.errorCode != "DEPLOYMENT_AGENT_FAILED" || failed.errorMessage != "部署 Agent 执行失败" {
		t.Fatalf("unsafe or unexpected failure summary = %#v", failed)
	}
}

func TestGetSubsystemStatusReturnsDurableSafeState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{
		ApplicationCode: "customer_management", Environment: "dev",
		Status: application.SubsystemDeploymentStatusFailed, Operation: "RETRY",
		Generation: 2, AttemptCount: 2, LastErrorCode: "DEPLOYMENT_AGENT_FAILED",
		LastError: "部署 Agent 执行失败", UpdatedAt: now,
	}}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/subsystem-status?application_code=customer_management&environment=dev", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.GetSubsystemStatus(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{`"status":"PROVISION_FAILED"`, `"operation":"RETRY"`, `"attempt_count":2`, `"last_error_code":"DEPLOYMENT_AGENT_FAILED"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response missing %s: %s", expected, body)
		}
	}
	if !strings.Contains(body, `"next_action":`) || !strings.Contains(body, "不要重复新增接入") {
		t.Fatalf("status response missing actionable recovery guidance: %s", body)
	}
	for _, forbidden := range []string{"client_secret", "command", "filesystem", "container_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("status response leaked %q: %s", forbidden, body)
		}
	}
}
