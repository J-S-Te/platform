package infrastructure

import (
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

// settlementReferenceBinding 是项目清单声明结算只读身份的关联绑定来源。
const settlementReferenceBinding = "related_service.settlement.settlement_project_reference_read.client_id"

// findSettlementReferenceSource 查找项目接收端可用的结算只读来源（不做字段校验）。
// 参数：input 为项目环境及已登记来源。
// 返回值：未登记时返回 nil；同名来源重复时返回歧义错误。
func findSettlementReferenceSource(input application.SubsystemProvisioningInput) (*application.RelatedSubsystemServiceClient, error) {
	var found *application.RelatedSubsystemServiceClient
	for index := range input.RelatedServiceClients {
		candidate := &input.RelatedServiceClients[index]
		if candidate.ApplicationCode != "settlement" ||
			candidate.Purpose != application.ServiceCredentialSettlementProjectReferenceRead {
			continue
		}
		if found != nil {
			return nil, provisioningError("settlement reference source is ambiguous")
		}
		found = candidate
	}
	return found, nil
}

// resolveSettlementReferenceIdentity 校验项目接收端所绑定的结算只读身份。
// 参数：input 为项目环境及已登记来源，source 为允许的关联身份字段。
// 返回值：已核验的公开客户端标识；来源缺失、歧义或漂移时拒绝。
func resolveSettlementReferenceIdentity(input application.SubsystemProvisioningInput, source string) (string, error) {
	if input.ApplicationCode != "project_management" || source != settlementReferenceBinding {
		return "", provisioningError("settlement reference binding does not match receiver")
	}
	found, err := findSettlementReferenceSource(input)
	if err != nil {
		return "", err
	}
	if found == nil || found.ApplicationID == "" || found.EnvironmentID == "" ||
		found.Environment != input.Environment {
		return "", provisioningError("settlement reference requires source onboarding/update in the same tenant and environment")
	}
	sourceInput := input
	sourceInput.ApplicationID, sourceInput.EnvironmentID = found.ApplicationID, found.EnvironmentID
	if err := validateP1ServiceCredential(sourceInput,
		application.ServiceCredentialSettlementProjectReferenceRead, found.Client); err != nil {
		return "", err
	}
	return found.Client.ClientID, nil
}

// projectSettlementReferenceRuntimeValues 配置项目独立的结算引用验签入口（本地/开发路径）。
// 参数：input 为项目环境及已核验来源元数据，values 为待写入配置。
// 返回值：配置完整时为空；来源未登记（结算后接入）时关闭集成并清空残留来源身份；
// 来源已登记但漂移/歧义时拒绝发布。
func projectSettlementReferenceRuntimeValues(input application.SubsystemProvisioningInput, values map[string]string) error {
	if input.ApplicationCode != "project_management" {
		return provisioningError("settlement reference binding does not match receiver")
	}
	found, err := findSettlementReferenceSource(input)
	if err != nil {
		return err
	}
	if found == nil {
		// 结算侧尚未接入：项目按安全失败关闭先接入，结算接入后再次受控更新才启用引用接口。
		disableSettlementReferenceRuntimeValues(values)
		return nil
	}
	clientID, err := resolveSettlementReferenceIdentity(input, settlementReferenceBinding)
	if err != nil {
		return err
	}
	values["SETTLEMENT_REFERENCE_ENABLED"] = "true"
	values["SETTLEMENT_REFERENCE_CLIENT_ID"] = clientID
	values["SETTLEMENT_REFERENCE_AUDIENCE"] = "basic-platform-application"
	values["SETTLEMENT_REFERENCE_ISSUER"] = "basic-platform"
	values["SETTLEMENT_REFERENCE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
	values["SETTLEMENT_REFERENCE_CALLER_ENVIRONMENT_CODE"] = input.Environment
	return nil
}

// disableSettlementReferenceRuntimeValues 关闭项目结算引用集成并清空上一次登记的来源身份。
// 参数：values 为待写入配置。来源字段写空值而非保留旧值，避免沿用已不存在客户端的残留配置。
func disableSettlementReferenceRuntimeValues(values map[string]string) {
	values["SETTLEMENT_REFERENCE_ENABLED"] = "false"
	for _, key := range []string{
		"SETTLEMENT_REFERENCE_CLIENT_ID",
		"SETTLEMENT_REFERENCE_AUDIENCE",
		"SETTLEMENT_REFERENCE_ISSUER",
		"SETTLEMENT_REFERENCE_PUBLIC_KEY_PATH",
		"SETTLEMENT_REFERENCE_CALLER_ENVIRONMENT_CODE",
	} {
		values[key] = ""
	}
}

// settlementRoleRuntimeValues 配置结算项目查询、人员目录及通知审计出站身份。
// 参数：input 为结算登记输入，current 为现有临时运行值，values 为本次更新。
// 返回值：身份完整时为空；密钥缺失时要求受控重试补发。
func settlementRoleRuntimeValues(input application.SubsystemProvisioningInput, current, values map[string]string) error {
	for _, binding := range []struct{ purpose, prefix string }{
		{application.ServiceCredentialSettlementProjectReferenceRead, "SETTLEMENT_PROJECT_REFERENCE"},
		{application.ServiceCredentialOwnerDirectoryRead, "PLATFORM_PERSONNEL_DIRECTORY"},
		{application.ServiceCredentialNotificationIngest, "SETTLEMENT_NOTIFICATION"},
		{application.ServiceCredentialAuditIngest, "SETTLEMENT_AUDIT"},
	} {
		if err := p1CredentialValues(input, binding.purpose, binding.prefix+"_CLIENT_ID",
			binding.prefix+"_CLIENT_SECRET", current, values); err != nil {
			return err
		}
	}
	values["SETTLEMENT_PROJECT_REFERENCE_URL"] = "http://project-api:8082"
	values["SETTLEMENT_PROJECT_REFERENCE_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values["PLATFORM_PERSONNEL_DIRECTORY_URL"] = "http://platform-api:8080"
	values["OIDC_TENANT_ID"] = input.TenantID
	values["PLATFORM_ENVIRONMENT_CODE"] = strings.TrimSpace(input.Environment)
	return nil
}

// strictProductionServiceBinding 限定结算平台出站凭据使用严格校验及只写密钥保护。
// 参数：input 为接收应用，source 为清单中的服务绑定来源。
// 返回值：是否必须校验完整登记身份并拒绝静默继承错误配置。
func strictProductionServiceBinding(input application.SubsystemProvisioningInput, source string) bool {
	if isP1ProductionServiceBinding(source) {
		return true
	}
	if input.ApplicationCode != "reimbursement" && input.ApplicationCode != "settlement" && input.ApplicationCode != "settlement_and_invoicing" {
		return false
	}
	parts := strings.Split(source, ".")
	return len(parts) == 3 && parts[0] == "service" &&
		(parts[2] == "client_id" || parts[2] == "client_secret") &&
		application.RequiresStrictSubsystemUpdateServiceClientValidation(input.ApplicationCode, parts[1])
}
