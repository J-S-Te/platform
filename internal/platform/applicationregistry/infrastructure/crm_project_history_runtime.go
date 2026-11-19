package infrastructure

import (
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const productionCRMProjectHistoryRuntimePath = "runtime/customer-project-history.env"

// customerCRMProjectHistoryRuntimeValues 交付本地 CRM 使用的项目历史专用凭据。
// 参数：input 为受控接入或更新数据，values 为待写入的运行配置。
// 返回值：已提供的凭据不完整或串环境时返回错误。
func customerCRMProjectHistoryRuntimeValues(
	input application.SubsystemProvisioningInput, values map[string]string,
) error {
	credential, ok := input.ServiceCredential(application.ServiceCredentialCRMProjectHistoryRead)
	if !ok {
		return nil
	}
	expectedClient := input.ApplicationCode + "-" + input.Environment + "-crm-project-history"
	if credential.OAuthClient.ClientID != expectedClient || strings.TrimSpace(credential.PlaintextSecret) == "" {
		return provisioningError("CRM project history credential does not match the target environment")
	}
	values["CRM_PROJECT_HISTORY_ENABLED"] = "true"
	values["CRM_PROJECT_HISTORY_URL"] = "http://project-api:8082/internal/v1/crm/customers"
	values["CRM_PROJECT_HISTORY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values["CRM_PROJECT_HISTORY_CLIENT_ID"] = expectedClient
	values["CRM_PROJECT_HISTORY_CLIENT_SECRET"] = credential.PlaintextSecret
	values["CRM_PROJECT_HISTORY_SCOPE"] = "crm.project_history.read"
	return nil
}

// projectCRMProjectHistoryRuntimeValues 交付本地项目 API 的 CRM 机器身份验签设置。
// 参数：environment 为同环境的 CRM 调用方环境，values 为待写入运行配置。
// 返回值：无。
func projectCRMProjectHistoryRuntimeValues(environment string, values map[string]string) {
	values["CRM_PROJECT_HISTORY_MACHINE_ENABLED"] = "true"
	values["CRM_PROJECT_HISTORY_MACHINE_AUDIENCE"] = "basic-platform-application"
	values["CRM_PROJECT_HISTORY_MACHINE_ISSUER"] = "basic-platform"
	values["CRM_PROJECT_HISTORY_MACHINE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
	values["CRM_PROJECT_HISTORY_MACHINE_CALLER_ENVIRONMENT_CODE"] = environment
}
