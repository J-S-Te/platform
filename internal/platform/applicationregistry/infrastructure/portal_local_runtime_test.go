package infrastructure

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// portalLocalRuntimeInput 按权威凭据规则构造门户自有身份，不携带CRM邀请/下载身份用途。
// 参数：无。
// 返回值：仅供本地配置测试的同租户应用环境输入及正式展开scope。
func portalLocalRuntimeInput() application.SubsystemProvisioningInput {
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{TenantID: "tenant-1", ApplicationID: "portal-app-1",
		ApplicationCode: integratedPortalApplicationCode, Environment: "dev", ClientID: "customer_portal-dev-web",
		ClientSecret: "fixture-browser", Issuer: "http://localhost:8081", PublicURL: "http://localhost:8081/customer-portal/",
		RedirectURI: "http://localhost:8081/customer-portal/auth/callback", PathPrefix: "/customer-portal",
		UpstreamURL: "http://portal-api:8091", CatalogPublisherClientID: "portal-publisher", CatalogPublisherClientSecret: "fixture-publisher"})
	for _, requirement := range application.SubsystemUpdateServiceCredentialRequirements(integratedPortalApplicationCode) {
		if requirement.Purpose == application.ServiceCredentialPortalInviteVerify || requirement.Purpose == application.ServiceCredentialPortalReportDownloadIdentityRead {
			continue
		}
		input.ServiceCredentials = append(input.ServiceCredentials, application.SubsystemServiceCredential{Purpose: requirement.Purpose,
			PlaintextSecret: "fixture-" + requirement.Purpose,
			OAuthClient: application.OAuthClientView{ID: "uid-" + requirement.Purpose, TenantID: input.TenantID, ApplicationID: input.ApplicationID,
				EnvironmentID: input.EnvironmentID, ClientID: "customer_portal-dev-" + requirement.Suffix, Status: "ACTIVE", ClientType: "service",
				TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: application.SubsystemUpdateServiceCredentialScopes(requirement)}})
	}
	return input
}

// TestPortalLocalRuntimeDeliversOwnedCredentialsAndHMAC 验证正式本地生成链能交付独立门户所需材料。
// 参数：t为测试上下文，只使用临时文件和内存元数据。
// 返回值：无；五用途、真实AppID、独立HMAC或worker来源丢失时失败。
func TestPortalLocalRuntimeDeliversOwnedCredentialsAndHMAC(t *testing.T) {
	path := localModeRuntimeFile(t, "PORTAL_CUSTOMER_DIRECTORY_MODE=local\n")
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	input := portalLocalRuntimeInput()
	p := &LocalDockerSubsystemProvisioner{}
	if err := p.p1LocalRuntimeValues(input, path, values, false); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"PROVISION", "ROLE_ASSIGN", "ROLE_REVOKE", "BINDING_WRITE", "BINDING_DISABLE"} {
		if values["PORTAL_LOCAL_"+prefix+"_CLIENT_ID"] == "" || values["PORTAL_LOCAL_"+prefix+"_CLIENT_SECRET"] == "" {
			t.Fatalf("missing local %s credential", prefix)
		}
	}
	if values["PORTAL_FILE_GATEWAY_APPLICATION_ID"] != input.ApplicationID || values["PORTAL_REPORT_SOURCE_APPLICATION_ID"] != input.ApplicationID {
		t.Fatal("local file/source application identities differ")
	}
	key, err := base64.StdEncoding.DecodeString(values["PORTAL_LOCAL_INVITE_HMAC_KEY_BASE64"])
	if err != nil || len(key) < 32 {
		t.Fatal("local invitation HMAC is missing")
	}
	for _, other := range []string{"PORTAL_ENCRYPTION_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_REPORT_CONTENT_KEY_BASE64", "PORTAL_HMAC_KEY_BASE64"} {
		if values[other] == values["PORTAL_LOCAL_INVITE_HMAC_KEY_BASE64"] {
			t.Fatal("invitation HMAC reused another purpose key")
		}
	}
	worker, err := readEnvironmentValues(filepath.Join(filepath.Dir(path), ".env.portal-report-worker.local"))
	if err != nil {
		t.Fatal(err)
	}
	if worker["PORTAL_CUSTOMER_DIRECTORY_MODE"] != "local" || worker["PORTAL_REPORT_WORKER_INGEST_ONLY"] != "true" {
		t.Fatal("local worker can fall back to project delivery")
	}
	if err := updateSubsystemEnvironment(path, path, values); err != nil {
		t.Fatal(err)
	}
	for index := range input.ServiceCredentials {
		input.ServiceCredentials[index].PlaintextSecret = ""
	}
	next := map[string]string{}
	if err := p.p1LocalRuntimeValues(input, path, next, false); err != nil {
		t.Fatal(err)
	}
	if next["PORTAL_LOCAL_INVITE_HMAC_KEY_BASE64"] != values["PORTAL_LOCAL_INVITE_HMAC_KEY_BASE64"] {
		t.Fatal("controlled update rotated live invitation HMAC")
	}
}

// TestPortalLocalProvisionDoesNotTouchCRM 验证真实Provision配置链不借助CRM运行文件或进程。
// 参数：t为测试上下文，所有编排只交给既有记录runner。
// 返回值：无；独售缺少CRM仍被隐式依赖时失败。
func TestPortalLocalProvisionDoesNotTouchCRM(t *testing.T) {
	root, platformRoot, _, gateway := createIntegratedProvisionerFixture(t, integratedPortalApplicationCode)
	source := filepath.Join(platformRoot, "docker/.env.portal.local.example")
	file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("\nPORTAL_CUSTOMER_DIRECTORY_MODE=local\n")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("write synthetic mode failed")
	}
	crm := filepath.Join(platformRoot, "docker/.env.customer.local")
	if err := os.Remove(crm); err != nil {
		t.Fatal(err)
	}
	runner := &recordingSubsystemRunner{}
	p, err := newLocalDockerSubsystemProvisioner(LocalDockerSubsystemProvisionerConfig{Enabled: true, ProjectsRoot: root,
		GatewayScriptPath: gateway, PlatformComposeProject: "basic-platform-local", Timeout: time.Second}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Provision(context.Background(), portalLocalRuntimeInput()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crm); !os.IsNotExist(err) {
		t.Fatal("local portal wrote CRM environment")
	}
	for _, call := range runner.calls {
		if containsString(call.arguments, "customer-api") {
			t.Fatal("local portal restarted CRM")
		}
	}
}

// TestPortalLocalRuntimeRejectsDriftedCredentialsAndKeys 验证新模式不能通过错误身份或复用密钥启动。
// 参数：t为测试上下文，先有合法正式生成基线，再逐项改变真实输入。
// 返回值：无；错误范围、归属、重复、来源或密钥应在写入worker前失败。
func TestPortalLocalRuntimeRejectsDriftedCredentialsAndKeys(t *testing.T) {
	for _, mode := range []string{"tenant", "application", "environment", "scope", "mapping_purpose_as_scope",
		"duplicate", "missing_secret", "browser_identity", "file_scope_subset", "wrong_source_subject",
		"hmac_reuse", "hmac_invalid", "invalid_mode", "unsafe_file"} {
		t.Run(mode, func(t *testing.T) {
			content := "PORTAL_CUSTOMER_DIRECTORY_MODE=local\n"
			if mode == "hmac_reuse" {
				key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
				content += "PORTAL_HMAC_KEY_BASE64=" + key + "\nPORTAL_LOCAL_INVITE_HMAC_KEY_BASE64=" + key + "\n"
			}
			if mode == "hmac_invalid" {
				content += "PORTAL_LOCAL_INVITE_HMAC_KEY_BASE64=not-base64\n"
			}
			if mode == "invalid_mode" {
				content = "PORTAL_CUSTOMER_DIRECTORY_MODE=true\n"
			}
			path := localModeRuntimeFile(t, content)
			path, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unsafe_file" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			input := portalLocalRuntimeInput()
			for index := range input.ServiceCredentials {
				credential := &input.ServiceCredentials[index]
				if credential.Purpose == application.ServiceCredentialExternalUserProvision {
					switch mode {
					case "tenant":
						credential.OAuthClient.TenantID = "foreign"
					case "application":
						credential.OAuthClient.ApplicationID = "foreign"
					case "environment":
						credential.OAuthClient.EnvironmentID = "foreign"
					case "scope":
						credential.OAuthClient.Scopes = []string{"*"}
					case "duplicate":
						input.ServiceCredentials = append(input.ServiceCredentials, *credential)
					case "missing_secret":
						credential.PlaintextSecret = ""
					case "browser_identity":
						input.ClientID = credential.OAuthClient.ClientID
					}
				}
				if credential.Purpose == application.ServiceCredentialPortalMappingProvision && mode == "mapping_purpose_as_scope" {
					credential.OAuthClient.Scopes = []string{"portal_mapping_provision"}
				}
				if credential.Purpose == application.ServiceCredentialFileGatewayWrite && mode == "file_scope_subset" {
					credential.OAuthClient.Scopes = []string{"platform:file:upload"}
				}
				if credential.Purpose == application.ServiceCredentialPortalReportFileRead && mode == "wrong_source_subject" {
					credential.OAuthClient.ClientID = "customer_portal-dev-file-gateway"
				}
			}
			if err := (&LocalDockerSubsystemProvisioner{}).p1LocalRuntimeValues(input, path, map[string]string{}, false); err == nil {
				t.Fatalf("local unsafe input accepted: %s", mode)
			}
		})
	}
}
