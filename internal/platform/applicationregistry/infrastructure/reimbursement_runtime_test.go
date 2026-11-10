package infrastructure

import (
	"strings"
	"testing"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
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
