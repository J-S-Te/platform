package infrastructure

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gopkg.in/yaml.v3"
)

func reviewedP1PortalFixture(t *testing.T) (*productionComposeTarget, application.SubsystemProvisioningInput) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(platformModuleRoot(t), "deploy", "production")
	profiles, _, err := loadProductionSubsystemProfiles(source, filepath.Join(source, "subsystems.d"))
	if err != nil {
		t.Fatal(err)
	}
	var profile productionSubsystemProfile
	for _, candidate := range profiles {
		if candidate.Manifest.Application.Code == "customer_portal" {
			profile = candidate
		}
	}
	// CRM 是单独管理的共享运行环境，报告服务对不会继承它。
	files := profile.Manifest.Runtime.Files[:2]
	profile.Manifest.Runtime.Files = files
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(source, file.TemplatePath))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file.Path), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	input := application.SubsystemProvisioningInput{TenantID: "tenant-1", ApplicationID: "portal-app-1", EnvironmentID: "env-1", ApplicationCode: "customer_portal", Environment: "prod", AuthenticationProvider: "platform", Issuer: "https://identity.example", PublicURL: "https://identity.example/customer-portal/", PathPrefix: "/customer-portal", ClientID: "portal-web", ClientSecret: "browser-secret", RedirectURI: "https://identity.example/customer-portal/auth/callback"}
	for _, purpose := range []string{application.ServiceCredentialPortalReportFileRead, application.ServiceCredentialPortalReportDownloadIdentityRead, application.ServiceCredentialAuditIngest, application.ServiceCredentialFileGatewayWrite, application.ServiceCredentialPortalMappingProvision, application.ServiceCredentialPortalMappingDisable, application.ServiceCredentialPortalInviteVerify} {
		scope := p1FixtureScope(purpose)
		input.ServiceCredentials = append(input.ServiceCredentials, application.SubsystemServiceCredential{Purpose: purpose, PlaintextSecret: "distinct-" + purpose, OAuthClient: application.OAuthClientView{ID: "uid-" + purpose, TenantID: input.TenantID, ApplicationID: input.ApplicationID, EnvironmentID: input.EnvironmentID, ClientID: "portal-prod-" + purpose, Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: []string{scope}}})
	}
	return &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}}, input
}
func TestP1ReviewedPortalRuntimeSeparatesSecretsSharesVaultAndPreservesUpdate(t *testing.T) {
	target, input := reviewedP1PortalFixture(t)
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	apiPath := filepath.Join(target.config.DeployRoot, "runtime/portal.env")
	workerPath := filepath.Join(target.config.DeployRoot, "runtime/portal-report-worker.env")
	api, _ := readEnvironmentValues(apiPath)
	worker, _ := readEnvironmentValues(workerPath)
	for _, key := range []string{"PORTAL_REPORT_CONTENT_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_OIDC_TENANT_ID", "PORTAL_REPORT_STORE_ROOT"} {
		if api[key] == "" || api[key] != worker[key] {
			t.Fatalf("shared report binding mismatch %s", key)
		}
	}
	if api["PORTAL_REPORT_CONTENT_KEY_BASE64"] == api["PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"] {
		t.Fatal("content and descriptor key reused")
	}
	for _, key := range []string{"PORTAL_REPORT_CONTENT_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"} {
		decoded, err := base64.StdEncoding.DecodeString(api[key])
		if err != nil || len(decoded) != 32 {
			t.Fatal("invalid report key")
		}
	}
	if _, exists := worker["PORTAL_REPORT_IDENTITY_CLIENT_SECRET"]; exists {
		t.Fatal("worker received API identity secret")
	}
	if _, exists := api["PORTAL_REPORT_SOURCE_CLIENT_SECRET"]; exists {
		t.Fatal("API received worker source secret")
	}
	if worker["PORTAL_REPORT_WORKER_INGEST_ONLY"] != "true" {
		t.Fatal("unconfigured external report delivery enabled")
	}
	if err := updateProductionSubsystemEnvironment(workerPath, map[string]string{"PORTAL_REPORT_WORKER_INGEST_ONLY": "false", "PORTAL_REPORT_PROJECT_REQUEST_URL": "https://report.example/requests", "PORTAL_REPORT_PROJECT_CLIENT_ID": "external-client", "PORTAL_REPORT_PROJECT_CLIENT_SECRET": "external-service-secret"}); err != nil {
		t.Fatal(err)
	}
	for index := range input.ServiceCredentials {
		if isP1ProductionServiceBinding("service." + input.ServiceCredentials[index].Purpose + ".client_secret") {
			input.ServiceCredentials[index].PlaintextSecret = ""
		}
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	after, _ := readEnvironmentValues(apiPath)
	afterWorker, _ := readEnvironmentValues(workerPath)
	if afterWorker["PORTAL_REPORT_WORKER_INGEST_ONLY"] != "false" || afterWorker["PORTAL_REPORT_PROJECT_CLIENT_SECRET"] != "external-service-secret" {
		t.Fatal("UPDATE silently disabled configured external report delivery")
	}
	if api["PORTAL_REPORT_CONTENT_KEY_BASE64"] != after["PORTAL_REPORT_CONTENT_KEY_BASE64"] || worker["PORTAL_REPORT_SOURCE_CLIENT_SECRET"] != afterWorker["PORTAL_REPORT_SOURCE_CLIENT_SECRET"] || api["PORTAL_REPORT_IDENTITY_CLIENT_SECRET"] != after["PORTAL_REPORT_IDENTITY_CLIENT_SECRET"] {
		t.Fatal("normal UPDATE rotated report secrets")
	}
	if err := updateProductionSubsystemEnvironment(workerPath, map[string]string{"PORTAL_REPORT_SOURCE_CLIENT_SECRET": ""}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(apiPath)
	if err := target.writeRuntimeFixedValues(input); err == nil || !strings.Contains(err.Error(), "RETRY") {
		t.Fatal("missing one-time secret did not require explicit RETRY")
	}
	unchanged, _ := os.ReadFile(apiPath)
	if string(before) != string(unchanged) {
		t.Fatal("failed preflight partially published API secrets")
	}
	for index := range input.ServiceCredentials {
		if input.ServiceCredentials[index].Purpose == application.ServiceCredentialPortalReportFileRead {
			input.ServiceCredentials[index].PlaintextSecret = "controlled-new-source-secret"
		}
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	recovered, _ := readEnvironmentValues(workerPath)
	if recovered["PORTAL_REPORT_WORKER_INGEST_ONLY"] != "false" || recovered["PORTAL_REPORT_PROJECT_REQUEST_URL"] != "https://report.example/requests" {
		t.Fatal("controlled RETRY downgraded configured external delivery")
	}
	if recovered["PORTAL_REPORT_SOURCE_CLIENT_SECRET"] != "controlled-new-source-secret" {
		t.Fatal("controlled redelivery missing")
	}
	after, _ = readEnvironmentValues(apiPath)
	if after["PORTAL_REPORT_IDENTITY_CLIENT_SECRET"] != api["PORTAL_REPORT_IDENTITY_CLIENT_SECRET"] {
		t.Fatal("source repair overwrote API secret")
	}
}
func TestP1InheritanceRejectsUnreviewedPathSecretAndSymlink(t *testing.T) {
	target, _ := reviewedP1PortalFixture(t)
	for _, test := range []struct {
		name   string
		mutate func(*productionSubsystemManifest)
	}{
		{"forward", func(m *productionSubsystemManifest) { m.Runtime.Files[1].InheritFrom = "runtime/customer.env" }},
		{"self", func(m *productionSubsystemManifest) { m.Runtime.Files[1].InheritFrom = m.Runtime.Files[1].Path }},
		{"outside", func(m *productionSubsystemManifest) { m.Runtime.Files[1].InheritFrom = "../portal.env" }},
		{"secret", func(m *productionSubsystemManifest) {
			m.Runtime.Files[1].InheritKeys = []string{"PORTAL_REPORT_IDENTITY_CLIENT_SECRET"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			content, _ := yaml.Marshal(target.config.Profile.Manifest)
			var clone productionSubsystemManifest
			if err := yaml.Unmarshal(content, &clone); err != nil {
				t.Fatal(err)
			}
			test.mutate(&clone)
			if err := normalizeAndValidateProductionSubsystemManifest(&clone); err == nil {
				t.Fatal("unsafe inheritance accepted")
			}
		})
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/portal.env")
	other := filepath.Join(target.config.DeployRoot, "outside.env")
	if err := os.Rename(path, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	file := target.config.Profile.Manifest.Runtime.Files[1]
	staged := map[string]map[string]string{file.InheritFrom: {"PORTAL_REPORT_CONTENT_KEY_BASE64": "value"}}
	if err := inheritProductionRuntimeValues(target.config.DeployRoot, file, staged, map[string]string{}); err == nil {
		t.Fatal("symlink source accepted")
	}
	if err := inheritProductionRuntimeValues(target.config.DeployRoot, file, nil, map[string]string{}); err == nil {
		t.Fatal("unprepared source accepted")
	}
}
func TestP1RuntimeGroupValidatesAllBeforePublication(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	a := filepath.Join(root, "a.env")
	b := filepath.Join(root, "b.env")
	if err := os.WriteFile(a, []byte("KEY=original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("KEY=one\nKEY=two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateProductionRuntimeGroup([]runtimeEnvironmentUpdate{{a, map[string]string{"KEY": "new"}}, {b, map[string]string{"KEY": "new"}}}); err == nil {
		t.Fatal("duplicate managed key accepted")
	}
	unchanged, _ := os.ReadFile(a)
	if string(unchanged) != "KEY=original\n" {
		t.Fatal("group failure partially wrote first file")
	}
}
func TestP1RelatedContractBindingRejectsWrongTenantEnvironmentScopeAndAmbiguity(t *testing.T) {
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{TenantID: "tenant-1", ApplicationID: "receiver", ApplicationCode: "settlement", Environment: "prod"})
	for _, test := range []struct {
		name   string
		mutate func(*application.SubsystemProvisioningInput)
	}{
		{"tenant", func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Client.TenantID = "other" }},
		{"environment", func(i *application.SubsystemProvisioningInput) { i.RelatedServiceClients[0].Environment = "dev" }},
		{"uid", func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients[0].Client.EnvironmentID = "other-uid"
		}},
		{"scope", func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients[0].Client.Scopes = []string{"settlement.contract.ingest", "admin"}
		}},
		{"duplicate", func(i *application.SubsystemProvisioningInput) {
			i.RelatedServiceClients = append(i.RelatedServiceClients, i.RelatedServiceClients[0])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clone := input
			clone.RelatedServiceClients = append([]application.RelatedSubsystemServiceClient{}, input.RelatedServiceClients...)
			test.mutate(&clone)
			if _, err := resolveProductionBinding(clone, "related_service.contract_management.settlement_contract_ingest.client_id"); err == nil {
				t.Fatal("wrong source binding accepted")
			}
		})
	}
	expected := map[string]string{"client_id": "contract_management-prod-settlement-integration", "oauth_client_id": "contract-source-client", "application_id": "contract-app-1", "environment_id": "contract-env-1"}
	for field, want := range expected {
		got, err := resolveProductionBinding(input, "related_service.contract_management.settlement_contract_ingest."+field)
		if err != nil || got != want {
			t.Fatalf("field %s failed", field)
		}
	}
}

func TestP1ReportVaultRejectsAliasedLongTermKeysBeforePublish(t *testing.T) {
	target, input := reviewedP1PortalFixture(t)
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	apiPath := filepath.Join(target.config.DeployRoot, "runtime/portal.env")
	workerPath := filepath.Join(target.config.DeployRoot, "runtime/portal-report-worker.env")
	values, err := readEnvironmentValues(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateProductionSubsystemEnvironment(apiPath, map[string]string{"PORTAL_REPORT_CONTENT_KEY_BASE64": values["PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64"]}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeFixedValues(input); err == nil {
		t.Fatal("content key alias accepted")
	}
	after, err := os.ReadFile(workerPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("invalid vault key published to worker")
	}
}
