package infrastructure

import (
	"path/filepath"
	"testing"
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
		file := profile.Manifest.Runtime.Files[0]
		switch profile.Manifest.Application.Code {
		case "customer_and_opportunity":
			customer = file.Values["CRM_PROJECT_HISTORY_ENABLED"] == "true" &&
				file.Values["CRM_PROJECT_HISTORY_URL"] == "http://project-api:8082/internal/v1/crm/customers" &&
				file.Values["CRM_PROJECT_HISTORY_SCOPE"] == "crm.project_history.read" &&
				file.Bindings["CRM_PROJECT_HISTORY_CLIENT_ID"] == "service.crm_project_history_read.client_id" &&
				file.Bindings["CRM_PROJECT_HISTORY_CLIENT_SECRET"] == "service.crm_project_history_read.client_secret"
		case "project_management":
			project = file.Values["CRM_PROJECT_HISTORY_MACHINE_ENABLED"] == "true" &&
				file.Values["CRM_PROJECT_HISTORY_MACHINE_PUBLIC_KEY_PATH"] == "/app/data/keys/jwt-ed25519-public.pem" &&
				file.Bindings["CRM_PROJECT_HISTORY_MACHINE_CALLER_ENVIRONMENT_CODE"] == "environment"
		}
	}
	if !customer || !project {
		t.Fatal("CRM project history machine bindings are missing or inconsistent")
	}
}
