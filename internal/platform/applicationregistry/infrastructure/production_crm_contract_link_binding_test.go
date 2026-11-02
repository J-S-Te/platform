package infrastructure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestProductionContractCRMLinkIsBoundInReviewedProfileAndTemplate 验证合同→CRM
// 关联回写在生产清单中启用，并使用受控接入注入的专用用途凭据（BIZ-XEVT2-02）：
// 清单声明 allowed_service_bindings、values 提供非敏感的 base/token URL 与固定 scope，
// 模板保留 PENDING_ONBOARDING 占位等待接入注入。缺任一项都会让回写在生产上静默失效。
func TestProductionContractCRMLinkIsBoundInReviewedProfileAndTemplate(t *testing.T) {
	t.Parallel()
	root := platformModuleRoot(t)
	profiles, _, err := loadProductionSubsystemProfiles(filepath.Join(root, "deploy", "production"), filepath.Join(root, "deploy", "production", "subsystems.d"))
	if err != nil {
		t.Fatal(err)
	}
	var profile productionSubsystemProfile
	for _, candidate := range profiles {
		if candidate.Manifest.Application.Code == "contract_management" {
			profile = candidate
		}
	}
	if len(profile.Manifest.Runtime.Files) == 0 {
		t.Fatal("contract profile missing")
	}
	if profile.Manifest.Application.AllowedServiceBindings == nil ||
		!stringSliceContains(*profile.Manifest.Application.AllowedServiceBindings, application.ServiceCredentialCRMContractLinkWrite) {
		t.Fatal("contract CRM link purpose not allowed")
	}
	file := profile.Manifest.Runtime.Files[0]
	for key, source := range map[string]string{
		"CRM_LINK_CLIENT_ID":     "service.crm_contract_link_write.client_id",
		"CRM_LINK_CLIENT_SECRET": "service.crm_contract_link_write.client_secret",
	} {
		if file.Bindings[key] != source {
			t.Fatalf("CRM link binding %s is not controlled", key)
		}
	}
	for key, want := range map[string]string{
		"CRM_LINK_BASE_URL":  "http://customer-api:8090/customer-opportunity",
		"CRM_LINK_TOKEN_URL": "http://platform-api:8080/oauth2/token",
		"CRM_LINK_SCOPE":     "opportunity.signed.write",
	} {
		if file.Values[key] != want {
			t.Fatalf("CRM link value %s = %q, want %q", key, file.Values[key], want)
		}
	}
	template, err := os.ReadFile(filepath.Join(root, "deploy", "production", "subsystem-templates", "contract.env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CRM_LINK_CLIENT_ID", "CRM_LINK_CLIENT_SECRET"} {
		if !strings.Contains(string(template), key+"=PENDING_ONBOARDING") {
			t.Fatalf("CRM link template key %s missing or not pending onboarding", key)
		}
	}
	if !strings.Contains(string(template), "CRM_LINK_BASE_URL=\n") {
		t.Fatal("CRM link template base URL should stay empty: the reviewed manifest enables the edge after credential delivery")
	}
}
