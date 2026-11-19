package infrastructure

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
	"gopkg.in/yaml.v3"
)

// TestCRMProjectHistoryProductionBindings 验证正式清单给 CRM 交付专用凭据和项目地址。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCRMProjectHistoryProductionBindings(t *testing.T) {
	deploy := filepath.Join(platformModuleRoot(t), "deploy", "production")
	profiles, _, err := loadProductionSubsystemProfiles(deploy, filepath.Join(deploy, "subsystems.d"))
	if err != nil {
		t.Fatal(err)
	}
	var customer, project bool
	for _, profile := range profiles {
		switch profile.Manifest.Application.Code {
		case "customer_and_opportunity":
			for _, file := range profile.Manifest.Runtime.Files {
				if file.Path != "runtime/customer-project-history.env" {
					if file.Values["CRM_PROJECT_HISTORY_ENABLED"] != "" ||
						file.Bindings["CRM_PROJECT_HISTORY_CLIENT_SECRET"] != "" {
						t.Fatal("CRM project history credential is in the shared runtime file")
					}
					continue
				}
				customer = file.Values["CRM_PROJECT_HISTORY_ENABLED"] == "true" &&
					file.Values["CRM_PROJECT_HISTORY_URL"] == "http://project-api:8082/internal/v1/crm/customers" &&
					file.Values["CRM_PROJECT_HISTORY_SCOPE"] == "crm.project_history.read" &&
					file.Bindings["CRM_PROJECT_HISTORY_CLIENT_ID"] == "service.crm_project_history_read.client_id" &&
					file.Bindings["CRM_PROJECT_HISTORY_CLIENT_SECRET"] == "service.crm_project_history_read.client_secret"
			}
		case "project_management":
			file := profile.Manifest.Runtime.Files[0]
			project = file.Values["CRM_PROJECT_HISTORY_MACHINE_ENABLED"] == "true" &&
				file.Values["CRM_PROJECT_HISTORY_MACHINE_PUBLIC_KEY_PATH"] == "/app/data/keys/jwt-ed25519-public.pem" &&
				file.Bindings["CRM_PROJECT_HISTORY_MACHINE_CALLER_ENVIRONMENT_CODE"] == "environment"
		}
	}
	if !customer || !project {
		t.Fatal("CRM project history machine bindings are missing or inconsistent")
	}
}

// TestCRMProjectHistoryComposeSecretIsolation 验证专用配置只交给 CRM API。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCRMProjectHistoryComposeSecretIsolation(t *testing.T) {
	path := filepath.Join(platformModuleRoot(t), "deploy", "production", "docker-compose.yml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			EnvFiles []yaml.Node `yaml:"env_file"`
		}
	}
	if err := yaml.Unmarshal(content, &compose); err != nil {
		t.Fatal(err)
	}
	var consumers []string
	for service, config := range compose.Services {
		for _, file := range config.EnvFiles {
			var entry struct{ Path string }
			if file.Kind == yaml.MappingNode {
				if err := file.Decode(&entry); err != nil {
					t.Fatal(err)
				}
			} else {
				entry.Path = file.Value
			}
			if entry.Path == "${CUSTOMER_PROJECT_HISTORY_RUNTIME_ENV_FILE:-runtime/customer-project-history.env}" ||
				entry.Path == productionCRMProjectHistoryRuntimePath {
				consumers = append(consumers, service)
			}
		}
	}
	if !reflect.DeepEqual(consumers, []string{"customer-api"}) {
		t.Fatalf("project history credentials mounted by %v", consumers)
	}
}

// TestCRMProjectHistoryRuntimeDoesNotRefreshPortal 使用正式清单验证 CRM 更新范围。
// 参数：t 为测试上下文。
// 返回值：无。
func TestCRMProjectHistoryRuntimeDoesNotRefreshPortal(t *testing.T) {
	deploy := filepath.Join(platformModuleRoot(t), "deploy", "production")
	profiles, _, err := loadProductionSubsystemProfiles(deploy, filepath.Join(deploy, "subsystems.d"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &runtimeConsumerRunner{running: true, service: "portal-invite-compensation-worker"}
	target := runtimeConsumerFixture(t, runner)
	target.config.RuntimeConsumers = productionRuntimeConsumers(profiles)
	for _, profile := range profiles {
		if profile.Manifest.Application.Code == "customer_and_opportunity" {
			target.config.Profile = profile
		}
	}
	target.enabledServices["portal-invite-compensation-worker"] = true
	if err := target.refreshRuntimeConsumers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.starts) != 0 {
		t.Fatal("CRM credential delivery restarted a Portal worker")
	}
	target.config.Profile.Manifest.Runtime.Files[0].Values["PORTAL_INVITE_COMPENSATION_MYSQL_DSN"] = "fixture"
	if err := target.refreshRuntimeConsumers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.starts) != 1 {
		t.Fatal("CRM configuration affecting the existing Portal worker did not refresh it")
	}
}

// TestCRMProjectHistoryRuntimeExtension 验证受控更新补建 CRM 专用文件并限制读写权限。
// 参数：t 为隔离文件测试上下文。
// 返回值：无。
func TestCRMProjectHistoryRuntimeExtension(t *testing.T) {
	target := runtimeConsumerFixture(t, &runtimeConsumerRunner{})
	root, err := filepath.EvalSymlinks(target.config.DeployRoot)
	if err != nil {
		t.Fatal(err)
	}
	target.config.DeployRoot = root
	profile := &target.config.Profile.Manifest
	profile.Application.Code = integratedCustomerApplicationCode
	profile.Application.Environment = "prod"
	const templateName = "history.env.example"
	template := filepath.Join(target.config.DeployRoot, templateName)
	if err := os.WriteFile(template, []byte("CRM_PROJECT_HISTORY_ENABLED=false\n"), 0644); err != nil {
		t.Fatal(err)
	}
	profile.Runtime.Files = append(profile.Runtime.Files, productionSubsystemRuntimeFileManifest{
		Path: productionCRMProjectHistoryRuntimePath, TemplatePath: templateName,
	})
	input := application.SubsystemProvisioningInput{
		ApplicationCode: integratedCustomerApplicationCode, Environment: "prod",
	}
	if err := target.initializeInheritedRuntimeExtensions(input); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target.config.DeployRoot, productionCRMProjectHistoryRuntimePath)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("project history runtime file must be private")
	}
}
