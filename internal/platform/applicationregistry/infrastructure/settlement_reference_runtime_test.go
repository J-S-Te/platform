package infrastructure

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// settlementRoleRuntimeFixture 从正式清单准备临时运行文件及独立机器元数据。
// 参数：t 为测试上下文，app 为目标应用编码。
// 返回值：生产目标及仅供隔离测试的配置输入。
func settlementRoleRuntimeFixture(t *testing.T,
	app string) (*productionComposeTarget,
	application.SubsystemProvisioningInput) {
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
		if candidate.Manifest.Application.Code == app {
			profile = candidate
		}
	}
	if profile.Manifest.Application.Code == "" {
		t.Fatal("missing profile")
	}
	input := p1FixtureProvisioningInput(application.SubsystemProvisioningInput{TenantID: "tenant-1",
		ApplicationID:          "target-app",
		EnvironmentID:          "target-env",
		ApplicationCode:        app,
		Environment:            "prod",
		AuthenticationProvider: "platform",
		Issuer:                 "https://identity.example",
		PublicURL:              "https://identity.example/" + app + "/",
		PathPrefix:             "/" + app,
		ClientID:               "browser",
		ClientSecret:           "browser-only",
		RedirectURI:            "https://identity.example/" + app + "/auth/callback"})
	input.ServiceCredentials = nil
	if app == "settlement" {
		input.ServiceCredentials = append(input.ServiceCredentials, costSenderFixtureCredential(input))
	}
	for purpose, scope := range map[string]string{"settlement_project_reference_read": "settlement.project_reference.read",
		"owner_directory_read":   "owner_directory.read",
		"notification_ingest":    "notification.ingest",
		"audit_ingest":           "audit.ingest",
		"file_gateway_write":     "platform:file:upload",
		"contract_approved_read": "contract.approved.internal.read",
		// 结算→CRM 信用同步：清单声明的 crm_credit_payment_ingest 绑定需要同用途凭据。
		"crm_credit_payment_ingest": "customer.credit.payment.ingest"} {
		input.ServiceCredentials = append(input.ServiceCredentials,
			application.SubsystemServiceCredential{Purpose: purpose,
				PlaintextSecret: "isolated-" + purpose,
				OAuthClient: application.OAuthClientView{ID: "uid-" + purpose,
					TenantID:        input.TenantID,
					ApplicationID:   input.ApplicationID,
					EnvironmentID:   input.EnvironmentID,
					ClientID:        app + "-prod-" + purpose,
					Status:          "ACTIVE",
					ClientType:      "service",
					TokenAuthMethod: "client_secret_basic",
					GrantTypes:      []string{"client_credentials"},
					Scopes:          []string{scope}}})
	}
	if app == "project_management" {
		input.RelatedServiceClients = []application.RelatedSubsystemServiceClient{{ApplicationCode: "settlement",
			ApplicationID: "source-app",
			Environment:   "prod",
			EnvironmentID: "source-env",
			Purpose:       "settlement_project_reference_read",
			Client: application.OAuthClientView{ID: "source-uid",
				TenantID:        input.TenantID,
				ApplicationID:   "source-app",
				EnvironmentID:   "source-env",
				ClientID:        "settlement-prod-project-reference",
				Status:          "ACTIVE",
				ClientType:      "service",
				TokenAuthMethod: "client_secret_basic",
				GrantTypes:      []string{"client_credentials"},
				Scopes:          []string{"settlement.project_reference.read"}}}}
	}
	for _, file := range profile.Manifest.Runtime.Files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, file.Path)), 0700); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(source, file.TemplatePath))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file.Path), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}}, input
}

// TestSettlementRoleProductionRuntimeDelivery 验证正式清单实际生成项目、目录、通知和审计配置。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementRoleProductionRuntimeDelivery(t *testing.T) {
	target, input := settlementRoleRuntimeFixture(t, "settlement")
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.config.DeployRoot, "runtime/settlement.env")
	values, err := readEnvironmentValues(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"SETTLEMENT_PROJECT_REFERENCE_CLIENT_SECRET": "isolated-settlement_project_reference_read",
		"SETTLEMENT_PROJECT_REFERENCE_URL":           "http://project-api:8082",
		"SETTLEMENT_PROJECT_REFERENCE_TOKEN_URL":     "http://platform-api:8080/oauth2/token",
		"PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET": "isolated-owner_directory_read",
		"PLATFORM_PERSONNEL_DIRECTORY_URL":           "http://platform-api:8080",
		"SETTLEMENT_NOTIFICATION_CLIENT_SECRET":      "isolated-notification_ingest",
		"SETTLEMENT_AUDIT_CLIENT_SECRET":             "isolated-audit_ingest"} {
		if values[key] != want {
			t.Fatalf("runtime missing %s", key)
		}
	}
	for index := range input.ServiceCredentials {
		if input.ServiceCredentials[index].Purpose == "settlement_project_reference_read" {
			input.ServiceCredentials[index].PlaintextSecret = ""
		}
	}
	if err := updateProductionSubsystemEnvironment(path,
		map[string]string{"SETTLEMENT_PROJECT_REFERENCE_CLIENT_SECRET": ""}); err != nil {
		t.Fatal(err)
	}
	if err := target.writeRuntimeFixedValues(input); err == nil {
		t.Fatal("missing one-time secret accepted")
	}
	for index := range input.ServiceCredentials {
		if input.ServiceCredentials[index].Purpose == "settlement_project_reference_read" {
			input.ServiceCredentials[index].PlaintextSecret = "redelivered"
		}
	}
	if err := target.writeRuntimeFixedValues(input); err != nil {
		t.Fatal(err)
	}
	values, err = readEnvironmentValues(path)
	if err != nil || values["SETTLEMENT_PROJECT_REFERENCE_CLIENT_SECRET"] != "redelivered" {
		t.Fatal("update failed to repair secret")
	}
}

// TestProjectSettlementReferenceProductionManifestDecoupled 验证生产清单不再声明结算 client 依赖：
// 结算来源未登记时项目照常接入，引用入口按平台 Application JWT + scope 授权（ALLOW_ANY_CLIENT=true）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectSettlementReferenceProductionManifestDecoupled(t *testing.T) {
	target, input := settlementRoleRuntimeFixture(t, "project_management")
	input.RelatedServiceClients = nil
	for _, runtimeFile := range target.config.Profile.Manifest.Runtime.Files {
		for key, source := range runtimeFile.Bindings {
			if strings.HasPrefix(source, "related_service.settlement.") {
				t.Fatalf("project manifest still binds settlement client: %s=%s", key, source)
			}
			if key == "SETTLEMENT_REFERENCE_CLIENT_ID" {
				t.Fatalf("project manifest still binds settlement client id: %s", source)
			}
		}
		if environment, exists := runtimeFile.Values["SETTLEMENT_REFERENCE_ALLOW_ANY_CLIENT"]; !exists || environment != "true" {
			t.Fatal("project manifest must accept any verified settlement client")
		}
		if enabled := runtimeFile.Values["SETTLEMENT_REFERENCE_ENABLED"]; enabled != "true" {
			t.Fatal("project manifest must enable the settlement reference route")
		}
		if source := runtimeFile.Bindings["SETTLEMENT_REFERENCE_CALLER_ENVIRONMENT_CODE"]; source != "environment" {
			t.Fatalf("project reference caller environment binding = %q, want environment", source)
		}
	}
	if err := target.writeRuntimeConfiguration(input); err != nil {
		t.Fatalf("project onboarding requires settlement dependency: %v", err)
	}
	values, err := readEnvironmentValues(filepath.Join(target.config.DeployRoot, "runtime/project.env"))
	if err != nil {
		t.Fatal(err)
	}
	if values["SETTLEMENT_REFERENCE_ENABLED"] != "true" || values["SETTLEMENT_REFERENCE_ALLOW_ANY_CLIENT"] != "true" {
		t.Fatalf("project reference route not enabled after decoupling: %q/%q",
			values["SETTLEMENT_REFERENCE_ENABLED"], values["SETTLEMENT_REFERENCE_ALLOW_ANY_CLIENT"])
	}
	if values["SETTLEMENT_REFERENCE_CALLER_ENVIRONMENT_CODE"] != "prod" {
		t.Fatalf("caller environment = %q, want prod", values["SETTLEMENT_REFERENCE_CALLER_ENVIRONMENT_CODE"])
	}
	// 清单不再写入可用的结算来源身份；模板占位也必须是空值。
	if usableProductionCredential(values["SETTLEMENT_REFERENCE_CLIENT_ID"]) {
		t.Fatalf("production published settlement reference identity: %q", values["SETTLEMENT_REFERENCE_CLIENT_ID"])
	}
}

// TestProjectSettlementReferenceStrictResolverRejectsDrift 验证保留的严格解析器仍然拒绝漂移：
// 清单不再引用它，但它一旦被重新引用不得放宽同租户、应用、环境、scope 与客户端类型校验。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectSettlementReferenceStrictResolverRejectsDrift(t *testing.T) {
	for _, name := range []string{"valid", "tenant", "app", "env-id", "env-code", "scope", "type", "duplicate", "missing"} {
		t.Run(name, func(t *testing.T) {
			_, input := settlementRoleRuntimeFixture(t, "project_management")
			switch name {
			case "missing":
				input.RelatedServiceClients = nil
			case "tenant":
				input.RelatedServiceClients[0].Client.TenantID = "other"
			case "app":
				input.RelatedServiceClients[0].Client.ApplicationID = "other"
			case "env-id":
				input.RelatedServiceClients[0].Client.EnvironmentID = "other"
			case "env-code":
				input.RelatedServiceClients[0].Environment = "dev"
			case "scope":
				input.RelatedServiceClients[0].Client.Scopes = append(input.RelatedServiceClients[0].Client.Scopes, "dashboard.project.read")
			case "type":
				input.RelatedServiceClients[0].Client.ClientType = "confidential"
			case "duplicate":
				input.RelatedServiceClients = append(input.RelatedServiceClients, input.RelatedServiceClients[0])
			}
			clientID, err := resolveSettlementReferenceIdentity(input, settlementReferenceBinding)
			if name == "valid" {
				if err != nil || clientID != "settlement-prod-project-reference" {
					t.Fatalf("trusted settlement source rejected: clientID=%q err=%v", clientID, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("untrusted settlement source accepted: %q", clientID)
			}
		})
	}
}

// TestSettlementRoleLocalRuntimeDelivery 验证本地接入和更新实际写入独立身份并拒绝缺失密钥。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementRoleLocalRuntimeDelivery(t *testing.T) {
	_, input := settlementRoleRuntimeFixture(t, "settlement")
	path := filepath.Join(t.TempDir(), "settlement.env")
	if err := os.WriteFile(path, []byte("SETTLEMENT_DEVELOPMENT_AUTH=true\nKEEP=original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	provisioner := &LocalDockerSubsystemProvisioner{}
	values := map[string]string{}
	if err := provisioner.p1LocalRuntimeValues(input, path, values, true); err != nil {
		t.Fatal(err)
	}
	if err := updateSubsystemEnvironment(path, path, values); err != nil {
		t.Fatal(err)
	}
	actual, err := readEnvironmentValues(path)
	if err != nil || actual["SETTLEMENT_PROJECT_REFERENCE_CLIENT_SECRET"] != "isolated-settlement_project_reference_read" ||
		actual["SETTLEMENT_NOTIFICATION_CLIENT_SECRET"] != "isolated-notification_ingest" ||
		actual["SETTLEMENT_AUDIT_CLIENT_SECRET"] != "isolated-audit_ingest" || actual["KEEP"] != "original" {
		t.Fatal("local delivery missing or unrelated setting overwritten")
	}
	for index := range input.ServiceCredentials {
		if input.ServiceCredentials[index].Purpose == "settlement_project_reference_read" {
			input.ServiceCredentials[index].PlaintextSecret = ""
		}
	}
	if err := provisioner.p1LocalRuntimeValues(input, path, map[string]string{}, false); err != nil {
		t.Fatal("valid existing secret was not preserved")
	}
	if err := updateSubsystemEnvironment(path,
		path,
		map[string]string{"SETTLEMENT_PROJECT_REFERENCE_CLIENT_ID": "different-source"}); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.p1LocalRuntimeValues(input, path, map[string]string{}, false); err == nil {
		t.Fatal("mismatched existing client inherited another secret")
	}
}

// TestProjectSettlementReferenceLocalRuntime 验证项目本地接入写入受控来源。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectSettlementReferenceLocalRuntime(t *testing.T) {
	_, input := settlementRoleRuntimeFixture(t, "project_management")
	path := filepath.Join(t.TempDir(), "project.env")
	if err := os.WriteFile(path, []byte("KEEP=original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	provisioner := &LocalDockerSubsystemProvisioner{}
	values := map[string]string{}
	if err := provisioner.p1LocalRuntimeValues(input, path, values, true); err != nil {
		t.Fatal(err)
	}
	if err := updateSubsystemEnvironment(path, path, values); err != nil {
		t.Fatal(err)
	}
	actual, err := readEnvironmentValues(path)
	if err != nil || actual["SETTLEMENT_REFERENCE_ENABLED"] != "true" ||
		actual["SETTLEMENT_REFERENCE_CLIENT_ID"] != "settlement-prod-project-reference" || actual["KEEP"] != "original" {
		t.Fatal("local project reference identity not delivered")
	}
}

// TestProjectSettlementReferenceLocalRuntimeAllowsSettlementLater 验证本地"项目先接入、结算后接入"：
// 结算来源未登记时关闭引用集成并清空残留身份，来源存在但漂移时仍拒绝。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectSettlementReferenceLocalRuntimeAllowsSettlementLater(t *testing.T) {
	provisioner := &LocalDockerSubsystemProvisioner{}
	path := filepath.Join(t.TempDir(), "project.env")
	if err := os.WriteFile(path, []byte("KEEP=original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, ready := settlementRoleRuntimeFixture(t, "project_management")

	// 先用已登记来源完成一次接入，制造可被继承的旧客户端配置。
	values := map[string]string{}
	if err := provisioner.p1LocalRuntimeValues(ready, path, values, true); err != nil {
		t.Fatal(err)
	}
	if err := updateSubsystemEnvironment(path, path, values); err != nil {
		t.Fatal(err)
	}
	before, err := readEnvironmentValues(path)
	if err != nil || before["SETTLEMENT_REFERENCE_ENABLED"] != "true" {
		t.Fatal("local project reference identity not delivered")
	}

	// 结算侧尚未接入（无相关来源）：本地更新必须安全关闭引用集成而不是 422。
	_, pending := settlementRoleRuntimeFixture(t, "project_management")
	pending.RelatedServiceClients = nil
	values = map[string]string{}
	if err := provisioner.p1LocalRuntimeValues(pending, path, values, false); err != nil {
		t.Fatalf("missing settlement source must not block local project update: %v", err)
	}
	if values["SETTLEMENT_REFERENCE_ENABLED"] != "false" || values["SETTLEMENT_REFERENCE_CLIENT_ID"] != "" {
		t.Fatalf("pending settlement source wrote enabled integration: %v", values)
	}
	if err := updateSubsystemEnvironment(path, path, values); err != nil {
		t.Fatal(err)
	}
	actual, err := readEnvironmentValues(path)
	if err != nil || actual["KEEP"] != "original" || actual["SETTLEMENT_REFERENCE_ENABLED"] != "false" ||
		actual["SETTLEMENT_REFERENCE_CLIENT_ID"] != "" || actual["SETTLEMENT_REFERENCE_AUDIENCE"] != "" ||
		actual["SETTLEMENT_REFERENCE_ISSUER"] != "" || actual["SETTLEMENT_REFERENCE_PUBLIC_KEY_PATH"] != "" ||
		actual["SETTLEMENT_REFERENCE_CALLER_ENVIRONMENT_CODE"] != "" {
		t.Fatalf("stale settlement reference identity retained: %v", actual)
	}

	// 首次接入（无既有运行文件）同样允许先接入。
	freshPath := filepath.Join(t.TempDir(), "fresh-project.env")
	if err := os.WriteFile(freshPath, []byte("KEEP=original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := map[string]string{}
	if err := provisioner.p1LocalRuntimeValues(pending, freshPath, fresh, true); err != nil {
		t.Fatalf("first local project onboarding blocked by missing settlement source: %v", err)
	}
	if fresh["SETTLEMENT_REFERENCE_ENABLED"] != "false" || fresh["SETTLEMENT_REFERENCE_CLIENT_ID"] != "" {
		t.Fatalf("first onboarding enabled unsettled integration: %v", fresh)
	}

	// 结算来源已登记但字段漂移：仍必须拒绝，不能静默关闭。
	for name, mutate := range map[string]func(*application.RelatedSubsystemServiceClient){
		"environment": func(client *application.RelatedSubsystemServiceClient) { client.Environment = "dev" },
		"scope": func(client *application.RelatedSubsystemServiceClient) {
			client.Client.Scopes = []string{"settlement.contract.read"}
		},
		"status":      func(client *application.RelatedSubsystemServiceClient) { client.Client.Status = "DISABLED" },
		"application": func(client *application.RelatedSubsystemServiceClient) { client.ApplicationID = "" },
	} {
		_, drifted := settlementRoleRuntimeFixture(t, "project_management")
		mutate(&drifted.RelatedServiceClients[0])
		if err := provisioner.p1LocalRuntimeValues(drifted, path, map[string]string{}, false); err == nil {
			t.Fatalf("%s drift accepted by local project update", name)
		}
	}

	// 同名来源重复：拒绝（不能任选其一）。
	_, duplicated := settlementRoleRuntimeFixture(t, "project_management")
	duplicated.RelatedServiceClients = append(duplicated.RelatedServiceClients, duplicated.RelatedServiceClients[0])
	if err := provisioner.p1LocalRuntimeValues(duplicated, path, map[string]string{}, false); err == nil {
		t.Fatal("duplicate settlement source accepted by local project update")
	}
}

// TestSettlementRoleProductionRejectsDriftedPlatformCredentials 验证三条结算平台链路首次及更新均拒绝漂移。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSettlementRoleProductionRejectsDriftedPlatformCredentials(t *testing.T) {
	for purpose, prefix := range map[string]string{
		"owner_directory_read": "PLATFORM_PERSONNEL_DIRECTORY",
		"notification_ingest":  "SETTLEMENT_NOTIFICATION",
		"audit_ingest":         "SETTLEMENT_AUDIT",
	} {
		for _, mode := range []string{"FIRST", "UPDATE"} {
			for _, fault := range []string{
				"tenant", "app", "environment", "type", "auth", "grant", "scope", "duplicate", "missing-secret",
			} {
				t.Run(purpose+"/"+mode+"/"+fault, func(t *testing.T) {
					target, input := settlementRoleRuntimeFixture(t, "settlement")
					path := filepath.Join(target.config.DeployRoot, "runtime/settlement.env")
					if mode == "UPDATE" {
						if err := target.writeRuntimeConfiguration(input); err != nil {
							t.Fatal(err)
						}
					}
					for index := range input.ServiceCredentials {
						credential := &input.ServiceCredentials[index]
						if credential.Purpose != purpose {
							continue
						}
						switch fault {
						case "tenant":
							credential.OAuthClient.TenantID = "other"
						case "app":
							credential.OAuthClient.ApplicationID = "other"
						case "environment":
							credential.OAuthClient.EnvironmentID = "other"
						case "type":
							credential.OAuthClient.ClientType = "confidential"
						case "auth":
							credential.OAuthClient.TokenAuthMethod = "client_secret_post"
						case "grant":
							credential.OAuthClient.GrantTypes = []string{"authorization_code"}
						case "scope":
							credential.OAuthClient.Scopes = []string{"admin"}
						case "duplicate":
							input.ServiceCredentials = append(input.ServiceCredentials, *credential)
						case "missing-secret":
							credential.PlaintextSecret = ""
						}
						break
					}
					if fault == "missing-secret" && mode == "UPDATE" {
						if err := updateProductionSubsystemEnvironment(path, map[string]string{
							prefix + "_CLIENT_ID": "old-wrong-identity", prefix + "_CLIENT_SECRET": "",
						}); err != nil {
							t.Fatal(err)
						}
					}
					before, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "UPDATE" {
						err = target.writeRuntimeFixedValues(input)
					} else {
						err = target.writeRuntimeConfiguration(input)
					}
					if err == nil {
						t.Fatal("untrusted platform credential published or silently inherited")
					}
					after, err := os.ReadFile(path)
					if err != nil || string(after) != string(before) {
						t.Fatal("rejected delivery partially rewrote runtime")
					}
				})
			}
		}
	}
}
