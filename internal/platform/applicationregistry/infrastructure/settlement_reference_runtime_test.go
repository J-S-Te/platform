package infrastructure

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"os"
	"path/filepath"
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
	for purpose, scope := range map[string]string{"settlement_project_reference_read": "settlement.project_reference.read",
		"owner_directory_read":   "owner_directory.read",
		"notification_ingest":    "notification.ingest",
		"audit_ingest":           "audit.ingest",
		"file_gateway_write":     "platform:file:upload",
		"contract_approved_read": "contract.approved.internal.read"} {
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

// TestProjectSettlementReferenceProductionBinding 验证项目入站绑定同租户、应用及环境并拒绝漂移。
// 参数：t 为测试上下文。
// 返回值：无。
func TestProjectSettlementReferenceProductionBinding(t *testing.T) {
	for _, name := range []string{"valid", "tenant", "app", "env-id", "env-code", "scope", "type", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			target, input := settlementRoleRuntimeFixture(t, "project_management")
			client := &input.RelatedServiceClients[0].Client
			switch name {
			case "tenant":
				client.TenantID = "other"
			case "app":
				client.ApplicationID = "other"
			case "env-id":
				client.EnvironmentID = "other"
			case "env-code":
				input.RelatedServiceClients[0].Environment = "dev"
			case "scope":
				client.Scopes = append(client.Scopes, "dashboard.project.read")
			case "type":
				client.ClientType = "confidential"
			case "duplicate":
				input.RelatedServiceClients = append(input.RelatedServiceClients, input.RelatedServiceClients[0])
			}
			err := target.writeRuntimeConfiguration(input)
			if name != "valid" {
				if err == nil {
					t.Fatal("untrusted related client published")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			values, err := readEnvironmentValues(filepath.Join(target.config.DeployRoot, "runtime/project.env"))
			if err != nil || values["SETTLEMENT_REFERENCE_ENABLED"] != "true" || values["SETTLEMENT_REFERENCE_CLIENT_ID"] != "settlement-prod-project-reference" || values["SETTLEMENT_REFERENCE_CALLER_ENVIRONMENT_CODE"] != "prod" {
				t.Fatal("project reference route not bound")
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

// TestProjectSettlementReferenceLocalRuntime 验证项目本地接入写入受控来源且缺依赖时拒绝。
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
	input.RelatedServiceClients = nil
	if err := provisioner.p1LocalRuntimeValues(input, path, map[string]string{}, false); err == nil {
		t.Fatal("missing related source accepted")
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
