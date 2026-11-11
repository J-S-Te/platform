package infrastructure

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const reimbursementRelatedPrefix = "related_service.reimbursement."

// reimbursementSourceIdentities 校验报销来源两个用途均属于精确的同租户应用环境。
// 参数：input为项目接收端及已登记的公开来源元数据。
// 返回值：两用途公开ClientID；来源不存在返回空，部分缺失或漂移返回错误。
func reimbursementSourceIdentities(input application.SubsystemProvisioningInput) (map[string]string, error) {
	result := map[string]string{}
	for _, source := range input.RelatedServiceClients {
		if source.ApplicationCode != "reimbursement" {
			continue
		}
		purpose := source.Purpose
		if purpose != application.ServiceCredentialReimbursementProjectReferenceRead && purpose != application.ServiceCredentialProjectReimbursementCostWrite {
			continue
		}
		if input.ApplicationCode != "project_management" || result[purpose] != "" || source.ApplicationID == "" || source.EnvironmentID == "" || source.Environment != input.Environment {
			return nil, provisioningError("reimbursement source binding mismatch")
		}
		sourceInput := input
		sourceInput.ApplicationID = source.ApplicationID
		sourceInput.EnvironmentID = source.EnvironmentID
		if err := validateP1ServiceCredential(sourceInput, purpose, source.Client); err != nil {
			return nil, err
		}
		result[purpose] = source.Client.ClientID
	}
	if len(result) == 1 || (len(result) == 2 && result[application.ServiceCredentialReimbursementProjectReferenceRead] == result[application.ServiceCredentialProjectReimbursementCostWrite]) {
		return nil, provisioningError("reimbursement source identities must be distinct and complete")
	}
	return result, nil
}

// resolveReimbursementReceiverBinding 仅向项目接收端公开已核验ClientID及启用状态。
// 参数：input为接收环境，source为固定白名单来源。
// 返回值：公开身份或启用标记；不允许查询任何来源secret。
func resolveReimbursementReceiverBinding(input application.SubsystemProvisioningInput, source string) (string, error) {
	if input.ApplicationCode != "project_management" {
		return "", provisioningError("reimbursement binding receiver mismatch")
	}
	ids, err := reimbursementSourceIdentities(input)
	if err != nil {
		return "", err
	}
	if source == "reimbursement_integration_enabled" {
		return booleanEnvironmentValue(len(ids) == 2), nil
	}
	for _, purpose := range []string{application.ServiceCredentialReimbursementProjectReferenceRead, application.ServiceCredentialProjectReimbursementCostWrite} {
		if source == reimbursementRelatedPrefix+purpose+".client_id" {
			return ids[purpose], nil
		}
	}
	return "", provisioningError("reimbursement source secret delivery is forbidden")
}

// projectReimbursementRuntimeValues 填充项目接收端机器验签配置，来源未接入时显式关闭。
// 参数：input为已核验目标，values为本次受控配置。
// 返回值：完整且独立的公开身份可用时成功，部分或漂移身份拒绝发布。
func projectReimbursementRuntimeValues(input application.SubsystemProvisioningInput, values map[string]string) error {
	for key, source := range map[string]string{
		"REIMBURSEMENT_INTEGRATION_ENABLED": "reimbursement_integration_enabled",
		"REIMBURSEMENT_REFERENCE_CLIENT_ID": reimbursementRelatedPrefix + application.ServiceCredentialReimbursementProjectReferenceRead + ".client_id",
		"REIMBURSEMENT_SPEND_CLIENT_ID":     reimbursementRelatedPrefix + application.ServiceCredentialProjectReimbursementCostWrite + ".client_id",
	} {
		value, err := resolveReimbursementReceiverBinding(input, source)
		if err != nil {
			return err
		}
		values[key] = value
	}
	values["REIMBURSEMENT_MACHINE_AUDIENCE"] = "basic-platform-application"
	values["REIMBURSEMENT_MACHINE_ISSUER"] = "basic-platform"
	values["REIMBURSEMENT_MACHINE_PUBLIC_KEY_PATH"] = "/app/data/keys/jwt-ed25519-public.pem"
	values["REIMBURSEMENT_MACHINE_CALLER_ENVIRONMENT_CODE"] = input.Environment
	return nil
}

// reimbursementLocalRuntimeValues 向报销本地运行文件投递独立用途凭据与公开接入配置。
// 参数：input为受控登记输入，path为来源文件，values接收本次原子更新，fresh标识首次接入。
// 返回值：全部机器归属和scope精确匹配时成功；缺secret仅允许同身份安全保留。
func reimbursementLocalRuntimeValues(input application.SubsystemProvisioningInput, path string, values map[string]string, fresh bool) error {
	current := map[string]string{}
	if _, err := os.Stat(path); err == nil {
		var e error
		current, e = readEnvironmentValues(path)
		if e != nil {
			return e
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := reimbursementLocalSecrets(current, values, fresh); err != nil {
		return err
	}
	for _, binding := range []struct{ purpose, prefix string }{
		{application.ServiceCredentialReimbursementProjectReferenceRead, "PROJECT_REFERENCE"},
		{application.ServiceCredentialProjectReimbursementCostWrite, "PROJECT_SPEND"},
		{application.ServiceCredentialOwnerDirectoryRead, "PLATFORM_OWNER_DIRECTORY"},
		{application.ServiceCredentialAuditIngest, "PLATFORM_AUDIT"},
		{application.ServiceCredentialNotificationIngest, "PLATFORM_NOTIFICATION"},
		{application.ServiceCredentialFileGatewayWrite, "FILE_GATEWAY"},
	} {
		if err := p1CredentialValues(input, binding.purpose, binding.prefix+"_CLIENT_ID", binding.prefix+"_CLIENT_SECRET", current, values); err != nil {
			return err
		}
	}
	if values["PROJECT_REFERENCE_CLIENT_ID"] == values["PROJECT_SPEND_CLIENT_ID"] {
		return provisioningError("reimbursement project clients must be distinct")
	}
	values["PLATFORM_BASE_URL"] = "http://platform-api:8080"
	values["PROJECT_API_BASE_URL"] = "http://project-api:8082"
	values["FILE_GATEWAY_BASE_URL"] = "http://file-gateway:8086"
	values["PLATFORM_APPLICATION_CODE"] = "reimbursement"
	values["PLATFORM_ENVIRONMENT_CODE"] = input.Environment
	values["OIDC_TENANT_ID"] = input.TenantID
	values["PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID"] = input.ApplicationID
	values["PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"] = input.CatalogPublisherClientID
	values["PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"] = input.CatalogPublisherClientSecret
	values["REIMBURSEMENT_AUDIT_REQUIRED"] = "true"

	values["PUBLIC_BASE_URL"] = strings.TrimRight(input.PublicURL, "/")
	values["FILE_GATEWAY_SCOPE"] = "platform:file:upload platform:file:bind platform:file:download"
	return reimbursementCostRuntimeValues(input, values)
}

// reimbursementLocalSecrets 保留已交付数据库与会话材料，初装时生成独立随机值。
// 参数：current为当前受控配置，values接收新值，fresh标识确认首次接入。
// 返回值：初装或已有材料合法时成功；既有材料损坏时拒绝静默轮换。
func reimbursementLocalSecrets(current, values map[string]string, fresh bool) error {
	for _, key := range []string{"REIMBURSEMENT_MYSQL_PASSWORD", "REIMBURSEMENT_MYSQL_ROOT_PASSWORD"} {
		raw := current[key]
		if strings.HasPrefix(raw, "PENDING_") || strings.HasPrefix(raw, "REPLACE_WITH_") {
			raw = ""
		}
		if current["MYSQL_DSN"] != "" && raw == "" {
			return provisioningError("reimbursement database credentials require controlled recovery")
		}
		value, err := existingOrRandomHex(raw)
		if err != nil {
			return err
		}
		values[key] = value
	}
	key := current["OIDC_SESSION_ENCRYPTION_KEY_BASE64"]
	if key == "" {
		if !fresh || current["MYSQL_DSN"] != "" {
			return provisioningError("reimbursement session key requires controlled recovery; do not rotate implicitly")
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		key = base64.StdEncoding.EncodeToString(raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(decoded) != 32 {
		return provisioningError("reimbursement session key is invalid; do not rotate implicitly")
	}
	values["OIDC_SESSION_ENCRYPTION_KEY_BASE64"] = key
	values["MYSQL_DSN"] = "reimbursement:" + values["REIMBURSEMENT_MYSQL_PASSWORD"] + "@tcp(reimbursement-mysql:3306)/reimbursement?parseTime=true&charset=utf8mb4&loc=UTC"
	return nil
}
