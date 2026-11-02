package infrastructure

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// TestP1StrictServicePurposesDeclareScopes 保证每个严格服务绑定用途都声明了唯一 scope。
// 历史缺陷：crm_contract_link_write 进入严格绑定列表但缺少 scope 映射，
// 导致 contract_management 接入在凭据校验阶段必然报 integration credential is incomplete。
func TestP1StrictServicePurposesDeclareScopes(t *testing.T) {
	if len(p1StrictServicePurposes) == 0 {
		t.Fatal("严格绑定用途列表为空")
	}
	for _, purpose := range p1StrictServicePurposes {
		scope, ok := p1ServiceCredentialScopes[purpose]
		if !ok || strings.TrimSpace(scope) == "" {
			t.Fatalf("严格绑定用途 %s 未声明允许的 scope", purpose)
		}
	}
}

// TestP1CRMLinkWriteCredentialScope 复现合同接入失败场景：crm_contract_link_write
// 必须按 opportunity.signed.write 通过校验，错误 scope 必须拒绝。
func TestP1CRMLinkWriteCredentialScope(t *testing.T) {
	input := application.SubsystemProvisioningInput{TenantID: "tenant-1", ApplicationID: "app-1", EnvironmentID: "env-1"}
	client := application.OAuthClientView{ID: "oauth-client-1", TenantID: "tenant-1", ApplicationID: "app-1",
		EnvironmentID: "env-1", ClientID: "contract-prod-crm-contract-link", Status: "ACTIVE",
		ClientType: "service", TokenAuthMethod: "client_secret_basic", GrantTypes: []string{"client_credentials"},
		Scopes: []string{"opportunity.signed.write"}}
	if err := validateP1ServiceCredential(input, application.ServiceCredentialCRMContractLinkWrite, client); err != nil {
		t.Fatalf("crm_contract_link_write 合法凭据被拒绝: %v", err)
	}
	client.Scopes = []string{"contract.summary.read"}
	if err := validateP1ServiceCredential(input, application.ServiceCredentialCRMContractLinkWrite, client); err == nil {
		t.Fatal("crm_contract_link_write 错误 scope 未被拒绝")
	}
}
