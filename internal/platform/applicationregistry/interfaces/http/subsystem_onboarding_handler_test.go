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
	"sync"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"github.com/J-S-Te/Basic-Platform/internal/shared/authctx"
	"github.com/J-S-Te/Basic-Platform/internal/shared/requestctx"
)

type failingKeycloakAuthorizationCatalog struct {
	err error
}

func (catalog failingKeycloakAuthorizationCatalog) ListKeycloakRoleCodes(context.Context, string, string) ([]string, error) {
	return nil, catalog.err
}

func TestLoadKeycloakRoleCodesForDriftFailsClosedWhenCatalogUnavailable(t *testing.T) {
	t.Parallel()
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"https://platform.example.com", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := handler.loadKeycloakRoleCodesForDrift(context.Background(), "tenant-1", "app-1"); err == nil {
		t.Fatal("expected an error when the authorization catalog is not configured")
	}

	catalogErr := errors.New("catalog backend unavailable")
	handler.ConfigureKeycloakAuthorizationCatalog(failingKeycloakAuthorizationCatalog{err: catalogErr})
	if _, err := handler.loadKeycloakRoleCodesForDrift(context.Background(), "tenant-1", "app-1"); !errors.Is(err, catalogErr) {
		t.Fatalf("catalog error = %v, want %v", err, catalogErr)
	}
}

func TestWriteKeycloakSyncFailureKeepsResponseSafeAndLogsCorrelatedCause(t *testing.T) {
	t.Parallel()
	const requestID = "01KZRME97X5XBWB3H1E74KZTSX"
	const sensitiveCause = "Keycloak admin secret must-not-reach-browser"

	var logs bytes.Buffer
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"https://platform.example.com", slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, stage := range []string{"broker", "client", "roles", "mapping", "backfill", "readiness"} {
		stage := stage
		t.Run(stage, func(t *testing.T) {
			request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-keycloak/sync", nil)
			request = request.WithContext(requestctx.WithRequestID(request.Context(), requestID))
			response := httptest.NewRecorder()

			handler.writeKeycloakSyncFailure(response, request, subsystemLifecycleRequest{ApplicationCode: "contract_management", Environment: "prod"}, stage, errors.New(sensitiveCause))

			if response.Code != stdhttp.StatusServiceUnavailable {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var body struct {
				Code      string            `json:"code"`
				RequestID string            `json:"request_id"`
				Details   map[string]string `json:"details"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != "PLATFORM_DEPENDENCY_UNAVAILABLE" || body.RequestID != requestID || body.Details["stage"] != stage {
				t.Fatalf("unexpected safe response: %#v", body)
			}
			if body.Details["detail"] == "" || body.Details["next_action"] == "" || strings.Contains(response.Body.String(), sensitiveCause) {
				t.Fatalf("response must give safe guidance without original cause: %s", response.Body.String())
			}
		})
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, "request_id="+requestID) || !strings.Contains(logOutput, sensitiveCause) || !strings.Contains(logOutput, "stage=roles") {
		t.Fatalf("logs must retain correlated original error and stage: %s", logOutput)
	}
}

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

func TestRegisterSubsystemDirectoryDoesNotProvisionOrCreateOAuthClient(t *testing.T) {
	t.Parallel()
	pathPrefix := "/inventory"
	baseURL := "https://portal.example.com"
	upstreamURL := "http://inventory-api:8080"
	issuerAlias := application.IssuerAliasKeycloak
	service := &stubSubsystemOnboardingService{directoryResult: application.SubsystemDirectoryRegistrationResult{
		Application: application.Application{ID: "app-1", TenantID: "01K10A00000000000000000001", Code: "inventory", Name: "库存管理系统", ApplicationType: "web", Status: "ACTIVE"},
		Environment: application.Environment{ID: "env-1", TenantID: "01K10A00000000000000000001", ApplicationID: "app-1", Environment: "prod", BaseURL: &baseURL, UpstreamURL: &upstreamURL, PathPrefix: &pathPrefix, IssuerAlias: &issuerAlias, Status: "ACTIVE"},
		LoginTarget: application.LoginTargetManagementItem{ID: "target-1", TenantID: "01K10A00000000000000000001", ApplicationID: "app-1", EnvironmentID: "env-1", TargetCode: "home", Name: "库存管理系统首页", TargetURI: "/inventory/", Status: "ACTIVE"},
		PublicURL:   "https://portal.example.com/inventory/",
	}}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(service, provisioner, &recordingSubsystemAccessManager{}, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler.ConfigureKeycloak(true, "https://sso.example.com/realms/basic-platform", "basic-platform")
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-directory", bytes.NewBufferString(`{"application_code":"inventory","application_name":"库存管理系统","environment":"prod","public_base_url":"https://portal.example.com","upstream_url":"http://inventory-api:8080","path_prefix":"/inventory","issuer_alias":"keycloak"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"}}))
	response := httptest.NewRecorder()

	handler.RegisterSubsystemDirectory(response, request)

	if response.Code != stdhttp.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if provisioner.preflightCalls != 0 || provisioner.provisionCalls != 0 {
		t.Fatalf("directory registration invoked deployment: %#v", provisioner)
	}
	if service.directoryInput.IssuerAlias == nil || *service.directoryInput.IssuerAlias != application.IssuerAliasKeycloak {
		t.Fatalf("issuer alias = %#v", service.directoryInput.IssuerAlias)
	}
	body := response.Body.String()
	if strings.Contains(body, "oauth_client") || strings.Contains(body, "client_secret") || !strings.Contains(body, "Keycloak Client 同步") {
		t.Fatalf("directory response must be free of OAuth client data and guide the next step: %s", body)
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
	result          application.SubsystemOnboardingResult
	directoryResult application.SubsystemDirectoryRegistrationResult
	input           application.SubsystemOnboardingInput
	directoryInput  application.SubsystemDirectoryRegistrationInput
	portalItems     []application.PortalApplication
	err             error
	directoryErr    error
}

func (service *stubSubsystemOnboardingService) OnboardSubsystem(_ context.Context, input application.SubsystemOnboardingInput) (application.SubsystemOnboardingResult, error) {
	service.input = input
	return service.result, service.err
}

func (service *stubSubsystemOnboardingService) RegisterSubsystemDirectory(_ context.Context, input application.SubsystemDirectoryRegistrationInput) (application.SubsystemDirectoryRegistrationResult, error) {
	service.directoryInput = input
	return service.directoryResult, service.directoryErr
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

func (service *stubSubsystemOnboardingService) ResolveApplicationEnvironment(context.Context, string, string, string) (string, string, error) {
	applicationID := service.result.Application.ID
	if applicationID == "" {
		applicationID = "app-1"
	}
	environmentID := service.result.Environment.ID
	if environmentID == "" {
		environmentID = "env-1"
	}
	return applicationID, environmentID, nil
}

func (service *stubSubsystemOnboardingService) PreflightValidate(context.Context, application.SubsystemOnboardingInput) error {
	return nil
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

func TestListPortalApplicationsReturnsUserProjectionGate(t *testing.T) {
	t.Parallel()
	service := &stubSubsystemOnboardingService{portalItems: []application.PortalApplication{{
		ApplicationID: "app-1", EnvironmentID: "env-1", Code: "customer_and_opportunity", Name: "客户与商机管理系统",
		Environment: "prod", TargetCode: "home", TargetURI: "/customer-opportunity/", PublicURL: "https://portal.example.com/customer-opportunity/",
		Allowed: false, Projection: application.PortalProjectionReadiness{Status: "RUNNING", Ready: false, NextAction: "账号权限正在同步，请稍后重试。"},
	}}}
	handler, err := NewSubsystemOnboardingHandler(
		service, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/portal/applications", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.ListPortalApplications(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{`"allowed":false`, `"projection_status":"RUNNING"`, `"projection_ready":false`, `"projection_next_action":"账号权限正在同步，请稍后重试。"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response missing %s: %s", expected, body)
		}
	}
}

func TestSubsystemHealthDashboardDoesNotReportUnverifiedDependenciesAsHealthy(t *testing.T) {
	t.Parallel()
	service := &stubSubsystemOnboardingService{portalItems: []application.PortalApplication{{
		ApplicationID: "app-1", Code: "data_analysis", Name: "数据看板与统计分析系统", Environment: "prod",
		Projection: application.PortalProjectionReadiness{Status: "SUCCEEDED", Ready: true},
	}}}
	handler, err := NewSubsystemOnboardingHandler(
		service, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/keycloak-integration/health-dashboard", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "tenant-1"}, User: authctx.ReferenceName{ID: "user-1"},
	}))
	response := httptest.NewRecorder()

	handler.GetSubsystemHealthDashboard(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{
		`"directory_ok":true`, `"credentials_ok":false`, `"runtime_ok":true`,
		`"keycloak_ok":false`, `"credentials_status":"UNKNOWN"`,
		`"keycloak_status":"NOT_APPLICABLE"`, `"projection_status":"SUCCEEDED"`,
		`"status":"VERIFICATION_REQUIRED"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response missing %s: %s", expected, body)
		}
	}
	if strings.Contains(body, `"status":"HEALTHY"`) {
		t.Fatalf("unverified dependencies must not be reported healthy: %s", body)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := response.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q", got)
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
	input          application.SubsystemProvisioningInput
	teardownCode   string
	capabilities   application.SubsystemProvisioningCapabilities
	preflightCalls int
	provisionCalls int
	preflightErr   error
	provisionErr   error
	updateErr      error
	teardownErr    error
}

func (provisioner *recordingHTTPSubsystemProvisioner) Capabilities() application.SubsystemProvisioningCapabilities {
	return provisioner.capabilities
}

func (provisioner *recordingHTTPSubsystemProvisioner) Preflight(context.Context, application.SubsystemPreflightInput) error {
	provisioner.preflightCalls++
	return provisioner.preflightErr
}

func (provisioner *recordingHTTPSubsystemProvisioner) Provision(_ context.Context, input application.SubsystemProvisioningInput) error {
	provisioner.provisionCalls++
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
		"dependent runtime credentials": {
			errMessage: "subsystem provisioning unavailable: production subsystem runtime secrets are incomplete",
			want:       "前置子系统",
		},
		"stale Agent infrastructure validation": {
			errMessage: "subsystem provisioning unavailable: production infrastructure secrets are incomplete",
			want:       "旧版 Agent",
		},
		"target database credentials": {
			errMessage: "subsystem provisioning unavailable: production subsystem database credentials are incomplete",
			want:       "实际使用的数据库凭据",
		},
		"runtime template unavailable": {
			errMessage: "subsystem provisioning unavailable: production subsystem runtime template is unavailable",
			want:       "自动创建文件",
		},
		"existing generated key is malformed": {
			errMessage: "subsystem provisioning unavailable: production subsystem generated runtime secret is invalid",
			want:       "避免误轮换",
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

type recordingNotificationSink struct {
	mu    sync.Mutex
	calls []SubsystemLifecycleNotification
}

func (sink *recordingNotificationSink) SendSubsystemLifecycle(_ context.Context, input SubsystemLifecycleNotification) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.calls = append(sink.calls, input)
	return nil
}

func TestNotifySubsystemLifecycleUsesSink(t *testing.T) {
	sink := &recordingNotificationSink{}
	handler, err := NewSubsystemOnboardingHandlerWithNotifications(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), sink,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.notifySubsystemLifecycle(context.Background(), "tenant-1", "user-1", "合同管理系统", "contract_management", "prod", true, "")
	handler.notifySubsystemLifecycle(context.Background(), "tenant-1", "user-1", "客户自助门户", "customer_portal", "prod", false, "boom")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.calls) != 2 {
		t.Fatalf("calls = %d, want 2: %#v", len(sink.calls), sink.calls)
	}
	if !sink.calls[0].Succeeded || sink.calls[0].ApplicationCode != "contract_management" {
		t.Fatalf("first call = %#v", sink.calls[0])
	}
	if sink.calls[1].Succeeded || sink.calls[1].Detail != "boom" {
		t.Fatalf("second call = %#v", sink.calls[1])
	}
}

func TestNotifySubsystemLifecycleNilSinkIsNoop(t *testing.T) {
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler.notifySubsystemLifecycle(context.Background(), "tenant-1", "user-1", "合同管理系统", "contract_management", "prod", true, "")
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

func TestUpdateSubsystemDoesNotConvertUnmanagedEnvironmentIntoProvisionFailure(t *testing.T) {
	t.Parallel()
	// Directory registration deliberately has no deployment-state row.  Until the
	// dedicated adoption endpoint exists, the legacy update endpoint must refuse
	// that state before invoking the Agent or persisting PROVISION_FAILED.
	stateStore := &recordingSubsystemDeploymentStateStore{contextErr: application.ErrNotFound}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, provisioner, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-update", bytes.NewBufferString(`{"application_code":"settlement","environment":"dev"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)

	if response.Code != stdhttp.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(stateStore.transitions) != 0 {
		t.Fatalf("unmanaged update must not write lifecycle transitions: %#v", stateStore.transitions)
	}
	if provisioner.input.ApplicationCode != "" {
		t.Fatalf("unmanaged update must not invoke the Agent: %#v", provisioner.input)
	}
}

func TestAdoptSubsystemCreatesManagedLifecycleForUnmanagedEnvironment(t *testing.T) {
	t.Parallel()
	stateStore := &recordingSubsystemDeploymentStateStore{contextErr: application.ErrNotFound}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{result: application.SubsystemOnboardingResult{
			Application: application.Application{ID: "settlement-app"},
			Environment: application.Environment{ID: "settlement-dev"},
		}}, provisioner, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	handler.serviceCredentials = &serviceCredentialManagerStub{clients: []application.OAuthClientView{{
		ID: "settlement-catalog-client", ApplicationID: "settlement-app", EnvironmentID: "settlement-dev",
		ClientID: "settlement-dev-catalog-publisher", Status: "ACTIVE",
	}}}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-adopt", bytes.NewBufferString(`{"application_code":"settlement","environment":"dev"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.AdoptSubsystem(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(stateStore.transitions) != 2 ||
		stateStore.transitions[0].status != application.SubsystemDeploymentStatusUpdating || stateStore.transitions[0].operation != "ADOPT" ||
		stateStore.transitions[1].status != application.SubsystemDeploymentStatusReady || stateStore.transitions[1].operation != "ADOPT" {
		t.Fatalf("adoption transitions = %#v, want ADOPT UPDATING then READY", stateStore.transitions)
	}
	if provisioner.input.ApplicationID != "settlement-app" || provisioner.input.ApplicationCode != "settlement" || provisioner.input.Environment != "dev" {
		t.Fatalf("adoption did not pass the resolved deployment boundary: %#v", provisioner.input)
	}
	if provisioner.input.CatalogPublisherClientID != "settlement-dev-catalog-publisher" || provisioner.input.CatalogPublisherClientSecret != "retry-secret" {
		t.Fatalf("adoption did not mint the Settlement catalog publisher credential: %#v", provisioner.input)
	}
}

func TestRetryDataAnalysisFailsClosedWithoutRuntimeCredentialManager(t *testing.T) {
	t.Parallel()
	stateStore := &recordingSubsystemDeploymentStateStore{contextErr: application.ErrNotFound}
	provisioner := &recordingHTTPSubsystemProvisioner{}
	service := &stubSubsystemOnboardingService{result: application.SubsystemOnboardingResult{
		Application: application.Application{ID: "data-analysis-app"},
		Environment: application.Environment{ID: "data-analysis-prod"},
	}}
	access := &recordingSubsystemAccessManager{roleCode: "dashboard_admin"}
	handler, err := NewSubsystemOnboardingHandler(
		service, provisioner, access, "http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodPost, "/api/v1/subsystem-retry", bytes.NewBufferString(`{"application_code":"data_analysis","environment":"prod"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.UpdateSubsystem(response, request)

	if response.Code != stdhttp.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if len(stateStore.transitions) != 0 {
		t.Fatalf("retry transitioned deployment without required credentials: %#v", stateStore.transitions)
	}
	if provisioner.input.ApplicationID != "" {
		t.Fatalf("retry invoked provisioner without required credentials: %#v", provisioner.input)
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

func TestGetSubsystemStatusReturnsUnmanagedStateForRegisteredEnvironmentWithoutDeployment(t *testing.T) {
	t.Parallel()
	stateStore := &recordingSubsystemDeploymentStateStore{getErr: application.ErrNotFound}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/subsystem-status?application_code=settlement&environment=dev", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.GetSubsystemStatus(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, expected := range []string{`"application_code":"settlement"`, `"environment":"dev"`, `"status":"UNMANAGED"`, `"operation":"NONE"`, `"next_action":`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("response missing %s: %s", expected, response.Body.String())
		}
	}
}

func TestGetSubsystemStatusRecoversStaleProvisioningStateForRetry(t *testing.T) {
	t.Parallel()
	startedAt := time.Now().UTC().Add(-subsystemDeploymentStaleAfter - time.Minute)
	stateStore := &recordingSubsystemDeploymentStateStore{state: application.SubsystemDeploymentState{
		TenantID: "01K10A00000000000000000001", ApplicationCode: "customer_portal", Environment: "prod",
		Status: application.SubsystemDeploymentStatusProvisioning, Operation: "ONBOARD", StartedAt: &startedAt,
	}}
	handler, err := NewSubsystemOnboardingHandler(
		&stubSubsystemOnboardingService{}, &recordingHTTPSubsystemProvisioner{}, &recordingSubsystemAccessManager{},
		"http://localhost:8081", slog.New(slog.NewTextHandler(io.Discard, nil)), stateStore,
	)
	if err != nil {
		t.Fatalf("construct handler: %v", err)
	}
	request := httptest.NewRequest(stdhttp.MethodGet, "/api/v1/subsystem-status?application_code=customer_portal&environment=prod", nil)
	request = request.WithContext(authctx.WithPrincipal(request.Context(), authctx.Principal{
		Tenant: authctx.ReferenceName{ID: "01K10A00000000000000000001"}, User: authctx.ReferenceName{ID: "01K10B00000000000000000001"},
	}))
	response := httptest.NewRecorder()

	handler.GetSubsystemStatus(response, request)

	if response.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":"PROVISION_FAILED"`) || !strings.Contains(response.Body.String(), `"last_error_code":"DEPLOYMENT_INTERRUPTED"`) {
		t.Fatalf("response did not expose recoverable failure: %s", response.Body.String())
	}
	if len(stateStore.transitions) != 1 || stateStore.transitions[0].status != application.SubsystemDeploymentStatusFailed {
		t.Fatalf("transitions = %#v", stateStore.transitions)
	}
}
