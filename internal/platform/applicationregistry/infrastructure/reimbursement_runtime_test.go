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
	"gopkg.in/yaml.v3"
)

// reimbursementInput 为临时运行配置构建用途分离且scope精确的合成凭据。
// 参数：无。
// 返回值：仅供本地测试的报销登记输入，不读取真实运行材料。
func reimbursementInput() application.SubsystemProvisioningInput {
	in := application.SubsystemProvisioningInput{TenantID: "tenant", ApplicationID: "app", EnvironmentID: "env", ApplicationCode: "reimbursement", Environment: "prod", AuthenticationProvider: "platform", Issuer: "https://identity.test", ClientID: "reimbursement-prod-web", ClientSecret: "browser-secret", PublicURL: "https://identity.test/reimbursement/", PathPrefix: "/reimbursement", RedirectURI: "https://identity.test/reimbursement/auth/callback", CatalogPublisherClientID: "publisher", CatalogPublisherClientSecret: "publisher-secret"}
	for _, r := range application.SubsystemUpdateServiceCredentialRequirements("reimbursement") {
		in.ServiceCredentials = append(in.ServiceCredentials, application.SubsystemServiceCredential{Purpose: r.Purpose, PlaintextSecret: "isolated-" + r.Purpose, OAuthClient: application.OAuthClientView{ID: "uuid-" + r.Purpose, TenantID: in.TenantID, ApplicationID: in.ApplicationID, EnvironmentID: in.EnvironmentID, ClientID: "reimbursement-prod-" + r.Suffix, Status: "ACTIVE", ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"}, Scopes: application.SubsystemUpdateServiceCredentialScopes(r)}})
	}
	return in
}

// TestReimbursementProductionAndLocalBindings 验证正式清单和本地投递的凭据及缺失拒绝一致。
// 参数：t为测试上下文。
// 返回值：无；用途中缺失、scope扩大或秘密串用时失败。
func TestReimbursementProductionAndLocalBindings(t *testing.T) {
	source := filepath.Join(platformModuleRoot(t), "deploy", "production")
	profiles, _, err := loadProductionSubsystemProfiles(source, filepath.Join(source, "subsystems.d"))
	if err != nil {
		t.Fatal(err)
	}
	var profile productionSubsystemProfile
	for _, p := range profiles {
		if p.Manifest.Application.Code == "reimbursement" {
			profile = p
		}
	}
	if profile.Manifest.Application.Code == "" {
		t.Fatal("descriptor missing")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, f := range profile.Manifest.Runtime.Files {
		data, err := os.ReadFile(filepath.Join(source, f.TemplatePath))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, f.Path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	target := productionComposeTarget{config: productionComposeTargetConfig{DeployRoot: root, Profile: profile}}
	in := reimbursementInput()
	if err = target.writeRuntimeConfiguration(in); err != nil {
		t.Fatal(err)
	}
	current, err := readEnvironmentValues(filepath.Join(root, "runtime", "reimbursement.env"))
	if err != nil {
		t.Fatal(err)
	}
	for key, purpose := range map[string]string{"PROJECT_REFERENCE_CLIENT_SECRET": "reimbursement_project_reference_read", "PROJECT_SPEND_CLIENT_SECRET": "project_reimbursement_cost_write", "FILE_GATEWAY_CLIENT_SECRET": "file_gateway_write"} {
		if current[key] != "isolated-"+purpose {
			t.Fatal(key)
		}
	}
	if current["COMMERCIAL_LICENSE_ENABLED"] != "" || current["COMMERCIAL_LICENSE_COMPATIBILITY"] != "" || current["REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED"] != "" {
		t.Fatal("unsafe runtime flags")
	}
	local := map[string]string{}
	if err = reimbursementLocalRuntimeValues(in, filepath.Join(root, "new.env"), local, true); err != nil {
		t.Fatal(err)
	}
	if local["PROJECT_REFERENCE_CLIENT_SECRET"] != current["PROJECT_REFERENCE_CLIENT_SECRET"] || local["PROJECT_SPEND_CLIENT_SECRET"] != current["PROJECT_SPEND_CLIENT_SECRET"] || local["PROJECT_REFERENCE_CLIENT_ID"] == local["PROJECT_SPEND_CLIENT_ID"] {
		t.Fatal("local credential drift")
	}
	for i := range in.ServiceCredentials {
		if in.ServiceCredentials[i].Purpose == "file_gateway_write" {
			in.ServiceCredentials[i].OAuthClient.Scopes = []string{"platform:file:upload"}
		}
	}
	if err = target.writeRuntimeConfiguration(in); err == nil {
		t.Fatal("file scope loss accepted")
	}
	if err = reimbursementLocalRuntimeValues(in, filepath.Join(root, "new.env"), map[string]string{}, true); err == nil {
		t.Fatal("local file scope loss accepted")
	}
}

// TestReimbursementReceiverNeverGetsSecrets 验证项目仅获得完整、同环境的两个公开来源身份。
// 参数：t为测试上下文。
// 返回值：无；单个身份、重复、环境漂移或secret绑定被接受时失败。
func TestReimbursementReceiverNeverGetsSecrets(t *testing.T) {
	in := reimbursementInput()
	receiver := application.SubsystemProvisioningInput{TenantID: in.TenantID, ApplicationID: "project", EnvironmentID: "project-env", ApplicationCode: "project_management", Environment: "prod"}
	values := map[string]string{}
	if err := projectReimbursementRuntimeValues(receiver, values); err != nil || values["REIMBURSEMENT_INTEGRATION_ENABLED"] != "false" {
		t.Fatal(values, err)
	}
	for _, c := range in.ServiceCredentials {
		if c.Purpose == application.ServiceCredentialReimbursementProjectReferenceRead || c.Purpose == application.ServiceCredentialProjectReimbursementCostWrite {
			receiver.RelatedServiceClients = append(receiver.RelatedServiceClients, application.RelatedSubsystemServiceClient{ApplicationCode: "reimbursement", ApplicationID: in.ApplicationID, EnvironmentID: in.EnvironmentID, Environment: "prod", Purpose: c.Purpose, Client: c.OAuthClient})
		}
	}
	if err := projectReimbursementRuntimeValues(receiver, values); err != nil || values["REIMBURSEMENT_INTEGRATION_ENABLED"] != "true" {
		t.Fatal(values, err)
	}
	for key := range values {
		if strings.Contains(key, "SECRET") {
			t.Fatal("source secret leaked")
		}
	}
	if _, err := resolveReimbursementReceiverBinding(receiver, reimbursementRelatedPrefix+application.ServiceCredentialReimbursementProjectReferenceRead+".client_secret"); err == nil {
		t.Fatal("source secret accepted")
	}
	receiver.RelatedServiceClients = receiver.RelatedServiceClients[:1]
	if err := projectReimbursementRuntimeValues(receiver, values); err == nil {
		t.Fatal("partial identities accepted")
	}
}

// TestReimbursementMigrationGateAndCompose 验证备份失败阻断迁移且三份编排使用独立许可与健康检查。
// 参数：t为测试上下文。
// 返回值：无；模板伪造备份批准或许可消费者接线缺失时失败。
func TestReimbursementMigrationGateAndCompose(t *testing.T) {
	if _, err := reimbursementMigrationArguments("", "reimbursement-migrate", false); err == nil {
		t.Fatal("unverified backup accepted")
	}
	args, err := reimbursementMigrationArguments("qa", "reimbursement-migrate", true)
	if err != nil || !strings.Contains(strings.Join(args, " "), "-e REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED=true") {
		t.Fatal(args, err)
	}
	root := platformModuleRoot(t)
	for _, name := range []string{"compose.local.yaml", "deploy/production/docker-compose.yml", "deploy/production/compose.yaml"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err = yaml.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		for _, service := range []string{"reimbursement-api", "reimbursement-worker"} {
			raw, _ := yaml.Marshal(c.Services[service])
			text := string(raw)
			if !strings.Contains(text, "license-"+service+".env") || !strings.Contains(text, "license-state-"+service) {
				t.Fatal(name, service)
			}
			if strings.Contains(text, "COMMERCIAL_LICENSE_COMPATIBILITY") {
				t.Fatal("compatibility bypass")
			}
			if service == "reimbursement-worker" && !strings.Contains(text, "9097/healthz") {
				t.Fatal("worker health not independent")
			}
		}
		raw, _ := yaml.Marshal(c.Services["reimbursement-migrate"])
		if strings.Contains(string(raw), "REIMBURSEMENT_MIGRATION_BACKUP_VERIFIED") {
			t.Fatal("permanent backup approval")
		}
	}
}

// TestReimbursementTeardownStopsOnlyOwnConsumers 验证未生成许可批准文件时也不会误停合同组件。
// 参数：t为测试上下文，所有运行文件和命令记录均为临时合成数据。
// 返回值：无；错误组件被选择或删除运行材料时失败。
func TestReimbursementTeardownStopsOnlyOwnConsumers(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"platform/scripts/gateway.sh", "platform/compose.local.yaml", "platform/docker/.env.local", "platform/docker/.env.reimbursement.local", "reimbursement/Dockerfile"} {
		path := filepath.Join(root, relative)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, []byte("OIDC_CLIENT_ID=reimbursement-prod-web\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &recordingSubsystemRunner{}
	p := &LocalDockerSubsystemProvisioner{runner: runner, config: LocalDockerSubsystemProvisionerConfig{Enabled: true, ProjectsRoot: root, GatewayScriptPath: filepath.Join(root, "platform/scripts/gateway.sh"), GatewayIncludePath: filepath.Join(root, "platform/docker/gateway.conf"), Timeout: time.Minute, DockerBinary: "docker"}}
	if err = p.Teardown(context.Background(), "tenant", "reimbursement", "prod"); err != nil {
		t.Fatal(err)
	}
	stopped := false
	for _, call := range runner.calls {
		if containsString(call.arguments, "stop") {
			stopped = true
			if !containsString(call.arguments, "reimbursement-api") || !containsString(call.arguments, "reimbursement-worker") || containsString(call.arguments, "contract-api") {
				t.Fatal(call.arguments)
			}
		}
	}
	if !stopped {
		t.Fatal("missing scoped stop")
	}
	if _, err = os.Stat(filepath.Join(root, "platform/docker/.env.reimbursement.local")); err != nil {
		t.Fatal("runtime materials removed")
	}
}

// TestReimbursementLocalSessionKeyLifecycle 验证仅首次接入生成会话密钥且存量缺损被阻断。
// 参数：t为本地测试上下文，运行材料仅写入测试临时目录。
// 返回值：无；保留、生成、损坏拒绝及fresh调用链任一违反约定时失败。
func TestReimbursementLocalSessionKeyLifecycle(t *testing.T) {
	valid := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	existing := "MYSQL_DSN=fixture\nREIMBURSEMENT_MYSQL_PASSWORD=fixture\n" +
		"REIMBURSEMENT_MYSQL_ROOT_PASSWORD=fixture\n"
	// 首次接入失败会留下"键存在但值为空"的运行文件；这类材料没有任何可轮换的加密会话，
	// 必须允许 retry 重新生成，否则部署会永久卡在 controlled recovery。
	emptyPlaceholders := "MYSQL_DSN=\nREIMBURSEMENT_MYSQL_PASSWORD=\n" +
		"REIMBURSEMENT_MYSQL_ROOT_PASSWORD=\nOIDC_SESSION_ENCRYPTION_KEY_BASE64=\n"
	for _, tc := range []struct {
		name      string
		fresh     bool
		current   string
		wantError bool
	}{
		{"fresh", true, "", false},
		{"existing_missing_key", false, existing, true},
		{"fresh_flag_existing_missing_key", true, existing, true},
		{"existing_preserved", false, existing + "OIDC_SESSION_ENCRYPTION_KEY_BASE64=" + valid + "\n", false},
		{"existing_invalid", false, existing + "OIDC_SESSION_ENCRYPTION_KEY_BASE64=invalid\n", true},
		{"first_attempt_missing_file", false, "", false},
		{"first_attempt_empty_placeholders", false, emptyPlaceholders, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "synthetic.env")
			if tc.current != "" {
				if err := os.WriteFile(path, []byte(tc.current), 0600); err != nil {
					t.Fatal(err)
				}
			}
			values := map[string]string{}
			err := (&LocalDockerSubsystemProvisioner{}).p1LocalRuntimeValues(reimbursementInput(), path, values, tc.fresh)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v; wantError=%v", err, tc.wantError)
			}
			if !tc.wantError {
				decoded, err := base64.StdEncoding.DecodeString(values["OIDC_SESSION_ENCRYPTION_KEY_BASE64"])
				if err != nil || len(decoded) != 32 {
					t.Fatal("session key is not 32 bytes")
				}
				if strings.Contains(tc.current, "OIDC_SESSION_ENCRYPTION_KEY_BASE64="+valid) &&
					values["OIDC_SESSION_ENCRYPTION_KEY_BASE64"] != valid {
					t.Fatal("existing session key changed")
				}
				if !strings.HasPrefix(values["MYSQL_DSN"], "reimbursement:") {
					t.Fatal("mysql dsn was not derived from generated credentials")
				}
			}
		})
	}
}
