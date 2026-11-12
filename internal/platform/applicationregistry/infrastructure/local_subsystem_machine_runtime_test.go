package infrastructure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// contractMachineTestEnvironment 写入本地合同运行文件，并返回路径。
func contractMachineTestEnvironment(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env.local")
	if err := os.WriteFile(path, []byte("CUSTOM_SETTING=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertRuntimeValues 断言运行文件包含全部期望键值，且保留未托管的自定义键。
func assertRuntimeValues(t *testing.T, path string, expected map[string]string) {
	t.Helper()
	values, err := readEnvironmentValues(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range expected {
		if got := values[key]; got != want {
			t.Fatalf("runtime value %s = %q, want %q", key, got, want)
		}
	}
	if values["CUSTOM_SETTING"] != "keep" {
		t.Fatal("managed rewrite dropped unmanaged runtime key")
	}
}

// TestLocalContractUpdateWritesCRMOpportunityIntakeAndSummaryMachineEntrypoints 覆盖本地合同
// 侧机器入口：CRM 商机签约事件与合同摘要的调用方固定为 customer_and_opportunity，client id
// 必须随目标环境推导，而不是写死某个环境。
func TestLocalContractUpdateWritesCRMOpportunityIntakeAndSummaryMachineEntrypoints(t *testing.T) {
	t.Parallel()
	path := contractMachineTestEnvironment(t)
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{
		ApplicationCode: integratedContractApplicationCode,
		Environment:     "staging",
	})
	if err := (&LocalDockerSubsystemProvisioner{}).updateServiceCredentialRuntimeConfiguration(input, path); err != nil {
		t.Fatalf("update contract machine runtime: %v", err)
	}
	assertRuntimeValues(t, path, map[string]string{
		"CRM_OPPORTUNITY_INTAKE_MACHINE_ENABLED":                 "true",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRE_BEARER":          "true",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_ISSUER":                  "basic-platform",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_AUDIENCE":                "basic-platform-application",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_PUBLIC_KEY_PATH":         "/app/data/keys/jwt-ed25519-public.pem",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_CLIENT_ID":               "customer_and_opportunity-staging-opportunity-intake",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_APPLICATION_CODE": "customer_and_opportunity",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_CALLER_ENVIRONMENT_CODE": "staging",
		"CRM_OPPORTUNITY_INTAKE_MACHINE_REQUIRED_SCOPE":          "opportunity.signed.write",
		"CONTRACT_SUMMARY_MACHINE_ENABLED":                       "true",
		"CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER":                "true",
		"CONTRACT_SUMMARY_MACHINE_ISSUER":                        "basic-platform",
		"CONTRACT_SUMMARY_MACHINE_AUDIENCE":                      "basic-platform-application",
		"CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH":               "/app/data/keys/jwt-ed25519-public.pem",
		"CONTRACT_SUMMARY_MACHINE_CLIENT_ID":                     "customer_and_opportunity-staging-contract-summary",
		"CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE":       "customer_and_opportunity",
		"CONTRACT_SUMMARY_MACHINE_CALLER_ENVIRONMENT_CODE":       "staging",
		"CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE":                "contract.summary.read",
		"CRM_LINK_BASE_URL":                                      "",
	})
}

// TestLocalContractUpdateEnablesCRMLinkOnlyWithControlledCredential 覆盖合同→CRM 回写开关：
// 没有受控凭据时地址留空（该边停用，避免启动校验失败关闭）；凭据齐全时才打开回写。
func TestLocalContractUpdateEnablesCRMLinkOnlyWithControlledCredential(t *testing.T) {
	t.Parallel()
	path := contractMachineTestEnvironment(t)
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{
		ApplicationCode: integratedContractApplicationCode,
		Environment:     "staging",
		ServiceCredentials: []application.SubsystemServiceCredential{{
			Purpose:         application.ServiceCredentialCRMContractLinkWrite,
			OAuthClient:     application.OAuthClientView{ClientID: "contract_management-staging-crm-contract-link"},
			PlaintextSecret: "crm-link-test-secret",
		}},
	})
	if err := (&LocalDockerSubsystemProvisioner{}).updateServiceCredentialRuntimeConfiguration(input, path); err != nil {
		t.Fatalf("update contract machine runtime: %v", err)
	}
	assertRuntimeValues(t, path, map[string]string{
		"CRM_LINK_BASE_URL":      "http://customer-api:8090/customer-opportunity",
		"CRM_LINK_TOKEN_URL":     "http://platform-api:8080/oauth2/token",
		"CRM_LINK_CLIENT_ID":     "contract_management-staging-crm-contract-link",
		"CRM_LINK_CLIENT_SECRET": "crm-link-test-secret",
		"CRM_LINK_SCOPE":         "opportunity.signed.write",
	})
}

// projectFileGatewayTestInput 构造项目子系统的公开运行时输入，mode 决定是否携带文件网关凭据。
func projectFileGatewayTestInput(withCredential bool) application.SubsystemProvisioningInput {
	credentials := []application.SubsystemServiceCredential{{
		Purpose:         application.ServiceCredentialNotificationIngest,
		OAuthClient:     application.OAuthClientView{ClientID: "project_management-staging-notification-ingest"},
		PlaintextSecret: "notification-test-secret",
	}}
	if withCredential {
		credentials = append(credentials, application.SubsystemServiceCredential{
			Purpose:         application.ServiceCredentialFileGatewayWrite,
			OAuthClient:     application.OAuthClientView{ClientID: "project_management-staging-file-gateway-writer"},
			PlaintextSecret: "file-gateway-test-secret",
		})
	}
	return p1FixtureProvisioningInput(application.SubsystemProvisioningInput{
		ApplicationID:      "project-app-1",
		ApplicationCode:    integratedProjectApplicationCode,
		Environment:        "staging",
		Issuer:             "http://localhost:8081/realms/basic-platform",
		PublicURL:          "http://localhost:8081/project_management/",
		PathPrefix:         "/project_management",
		RedirectURI:        "http://localhost:8081/project_management/auth/callback",
		ServiceCredentials: credentials,
	})
}

// TestLocalProjectUpdateEnablesFileGatewayForEvidenceVerification 覆盖项目证据核验依赖的
// 文件网关运行时键：启用开关必须与三 scope 一起下发，否则 verifyFileEvidence 返回 503。
func TestLocalProjectUpdateEnablesFileGatewayForEvidenceVerification(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env.local")
	if err := os.WriteFile(path, []byte("CUSTOM_SETTING=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&LocalDockerSubsystemProvisioner{}).updatePublicRuntimeConfiguration(projectFileGatewayTestInput(true), path); err != nil {
		t.Fatalf("update project public runtime: %v", err)
	}
	assertRuntimeValues(t, path, map[string]string{
		"FILE_GATEWAY_ENABLED":        "true",
		"FILE_GATEWAY_BASE_URL":       "http://file-gateway:8086",
		"FILE_GATEWAY_TOKEN_URL":      "http://platform-api:8080/oauth2/token",
		"FILE_GATEWAY_APPLICATION_ID": "project-app-1",
		"FILE_GATEWAY_CLIENT_ID":      "project_management-staging-file-gateway-writer",
		"FILE_GATEWAY_CLIENT_SECRET":  "file-gateway-test-secret",
		"FILE_GATEWAY_SCOPE":          "platform:file:upload platform:file:bind platform:file:download",
	})
}

// TestLocalProjectUpdateSkipsFileGatewayWithoutCredential 断言缺少受控凭据时不伪造文件网关身份：
// 运行文件保持不写 FILE_GATEWAY_*，项目侧继续以 503 失败关闭。
func TestLocalProjectUpdateSkipsFileGatewayWithoutCredential(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".env.local")
	if err := os.WriteFile(path, []byte("CUSTOM_SETTING=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&LocalDockerSubsystemProvisioner{}).updatePublicRuntimeConfiguration(projectFileGatewayTestInput(false), path); err != nil {
		t.Fatalf("update project public runtime: %v", err)
	}
	values, err := readEnvironmentValues(path)
	if err != nil {
		t.Fatal(err)
	}
	for key := range values {
		if strings.HasPrefix(key, "FILE_GATEWAY_") {
			t.Fatalf("file gateway runtime key written without controlled credential: %s", key)
		}
	}
	if values["CUSTOM_SETTING"] != "keep" {
		t.Fatal("managed rewrite dropped unmanaged runtime key")
	}
}

// TestLocalProjectUpdateWritesAuditPublisherCredential 覆盖项目受控更新：平台会轮换审计
// 发布者密钥，更新路径必须把新 client/secret 写回运行文件；否则项目继续用旧 secret
// 调用平台审计入口并持续 401（2026-11-12 本地实测）。
func TestLocalProjectUpdateWritesAuditPublisherCredential(t *testing.T) {
	t.Parallel()
	path := contractMachineTestEnvironment(t)
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{
		ApplicationCode: integratedProjectApplicationCode,
		Environment:     "staging",
		ServiceCredentials: []application.SubsystemServiceCredential{{
			Purpose:         application.ServiceCredentialAuditIngest,
			OAuthClient:     application.OAuthClientView{ClientID: "project_management-staging-audit-publisher"},
			PlaintextSecret: "project-audit-test-secret",
		}},
	})
	if err := (&LocalDockerSubsystemProvisioner{}).updateServiceCredentialRuntimeConfiguration(input, path); err != nil {
		t.Fatalf("update project machine runtime: %v", err)
	}
	assertRuntimeValues(t, path, map[string]string{
		"PLATFORM_AUDIT_CLIENT_ID":     "project_management-staging-audit-publisher",
		"PLATFORM_AUDIT_CLIENT_SECRET": "project-audit-test-secret",
	})
}

// TestLocalProjectUpdateRequiresAuditPublisherCredential 断言缺少受控审计凭据时失败关闭，
// 不允许把空值写进运行文件或静默沿用旧 secret。
func TestLocalProjectUpdateRequiresAuditPublisherCredential(t *testing.T) {
	t.Parallel()
	path := contractMachineTestEnvironment(t)
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{
		ApplicationCode: integratedProjectApplicationCode,
		Environment:     "staging",
	})
	if err := (&LocalDockerSubsystemProvisioner{}).updateServiceCredentialRuntimeConfiguration(input, path); err == nil {
		t.Fatal("expected missing project audit credential to fail closed")
	}
}
