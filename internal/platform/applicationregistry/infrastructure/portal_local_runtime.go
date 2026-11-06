package infrastructure

import (
	"bytes"
	"encoding/base64"
	"os"
	"slices"
	"strings"

	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

var portalLocalCredentialFields = []struct{ purpose, prefix string }{
	{application.ServiceCredentialExternalUserProvision, "PORTAL_LOCAL_PROVISION"},
	{application.ServiceCredentialApplicationRoleAssign, "PORTAL_LOCAL_ROLE_ASSIGN"},
	{application.ServiceCredentialApplicationRoleRevoke, "PORTAL_LOCAL_ROLE_REVOKE"},
	{application.ServiceCredentialPortalMappingProvision, "PORTAL_LOCAL_BINDING_WRITE"},
	{application.ServiceCredentialPortalMappingDisable, "PORTAL_LOCAL_BINDING_DISABLE"},
}

// localPortalMode 检查来源枚举且不允许自动值覆盖既有来源选择。
// 参数：current为已有文件，values为本次受控生成值。
// 返回值：精确local标记；非法或冲突模式返回错误，缺省保持CRM。
func localPortalMode(current, values map[string]string) (bool, error) {
	mode, proposed := current["PORTAL_CUSTOMER_DIRECTORY_MODE"], values["PORTAL_CUSTOMER_DIRECTORY_MODE"]
	if mode != "" && proposed != "" && mode != proposed {
		return false, provisioningError("Portal directory runtime modes conflict")
	}
	if mode == "" {
		mode = proposed
	}
	switch mode {
	case "", "crm":
		return false, nil
	case "local":
		return true, nil
	default:
		return false, provisioningError("Portal customer directory mode is invalid")
	}
}

// localPortalModeFromFile 只从受控来源文件读取本地模式并保护本地目录运行材料。
// 参数：path为Agent推导的模板或当前运行文件。
// 返回值：本地标记；明确local时非普通、链接或group/world权限均拒绝。
func localPortalModeFromFile(path string) (bool, error) {
	current, err := readEnvironmentValues(path)
	if err != nil {
		return false, provisioningError("Portal source runtime is unavailable")
	}
	local, err := localPortalMode(current, nil)
	if err != nil || !local {
		return local, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return false, provisioningError("local Portal source runtime must be private")
	}
	return true, nil
}

// portalRuntimeCredentials 在本地模式不强制未消费的CRM邀请凭据，旧模式保持原契约。
// 参数：input为已登记目标及本次交付，local来自受控文件的精确模式。
// 返回值：Audit及五项本地用途，缺失时失败；CRM分支沿用原必需列表。
func portalRuntimeCredentials(input application.SubsystemProvisioningInput, local bool) (map[string]application.SubsystemServiceCredential, error) {
	if !local {
		return requiredPortalServiceCredentials(input)
	}
	result := map[string]application.SubsystemServiceCredential{}
	purposes := []string{application.ServiceCredentialAuditIngest}
	for _, field := range portalLocalCredentialFields {
		purposes = append(purposes, field.purpose)
	}
	for _, purpose := range purposes {
		credential, ok := input.ServiceCredential(purpose)
		if !ok {
			return nil, provisioningError("local Portal service credentials are incomplete")
		}
		result[purpose] = credential
	}
	return result, nil
}

// portalLocalCredentialRequirement 读取既有权威需求，避免将用途名误当scope。
// 参数：purpose为门户固定用途。
// 返回值：正式需求或错误；文件网关使用权威展开后的三scope集合。
func portalLocalCredentialRequirement(purpose string) (application.SubsystemUpdateServiceCredentialRequirement, error) {
	for _, requirement := range application.SubsystemUpdateServiceCredentialRequirements(integratedPortalApplicationCode) {
		if requirement.Purpose == purpose {
			return requirement, nil
		}
	}
	return application.SubsystemUpdateServiceCredentialRequirement{}, provisioningError("unknown Portal runtime credential purpose")
}

// portalLocalCredentialValues 严格核验本门户机器身份后下发或沿用同身份已交付密钥。
// 参数：input为目标，purpose/prefix为固定用途，current为现值，values接收本次值。
// 返回值：跨租户/应用/环境、scope漂移、重复或缺少Secret时返回受控重试错误。
func portalLocalCredentialValues(input application.SubsystemProvisioningInput, purpose, prefix string, current, values map[string]string) error {
	requirement, err := portalLocalCredentialRequirement(purpose)
	if err != nil {
		return err
	}
	credential, ok := input.RegisteredServiceCredential(purpose)
	client := credential.OAuthClient
	if !ok || !portalLocalClientMatches(input, client, requirement) {
		return provisioningError("local Portal runtime credential identity or scope mismatch")
	}
	idKey, secretKey := prefix+"_CLIENT_ID", prefix+"_CLIENT_SECRET"
	secret := credential.PlaintextSecret
	if secret == "" {
		if current[idKey] != client.ClientID {
			return serviceCredentialRetryError()
		}
		secret = current[secretKey]
	}
	if !usableProductionCredential(secret) || secret != strings.TrimSpace(secret) || !validEnvironmentValue(secret) {
		return serviceCredentialRetryError()
	}
	values[idKey], values[secretKey] = client.ClientID, secret
	return nil
}

// portalLocalClientMatches 按目标归属和权威展开scope验证唯一机器身份。
// 参数：input为门户目标，client为已登记元数据，requirement为平台既有用途规则。
// 返回值：身份完整、同租户应用环境且权限集合精确时返回true。
func portalLocalClientMatches(input application.SubsystemProvisioningInput, client application.OAuthClientView, requirement application.SubsystemUpdateServiceCredentialRequirement) bool {
	wantScopes := application.SubsystemUpdateServiceCredentialScopes(requirement)
	actualScopes := append([]string(nil), client.Scopes...)
	slices.Sort(actualScopes)
	slices.Sort(wantScopes)
	targetMatches := input.ApplicationCode == integratedPortalApplicationCode && input.TenantID != "" &&
		input.ApplicationID != "" && input.EnvironmentID != "" && validLocalRuntimeEnvironment(input.Environment)
	identityMatches := client.ID != "" && client.TenantID == input.TenantID &&
		client.ApplicationID == input.ApplicationID && client.EnvironmentID == input.EnvironmentID &&
		client.ClientID == input.ApplicationCode+"-"+input.Environment+"-"+requirement.Suffix &&
		client.ClientID != input.ClientID && client.Status == "ACTIVE"
	protocolMatches := client.ClientType == "service" && client.TokenAuthMethod == "client_secret_basic" &&
		len(client.GrantTypes) == 1 && client.GrantTypes[0] == "client_credentials"
	return targetMatches && identityMatches && protocolMatches && slices.Equal(actualScopes, wantScopes)
}

// portalLocalRuntimeValues 装配门户自有目录及v2写源，不借用CRM身份或来源密钥。
// 参数：input为同租户应用环境，current为现值，values为待落盘值及独立密钥。
// 返回值：五用途、网关身份或HMAC不完整时失败，不生成占位业务身份。
func portalLocalRuntimeValues(input application.SubsystemProvisioningInput, current, values map[string]string) error {
	for _, field := range portalLocalCredentialFields {
		if err := portalLocalCredentialValues(input, field.purpose, field.prefix, current, values); err != nil {
			return err
		}
	}
	if err := portalLocalCredentialValues(input, application.ServiceCredentialFileGatewayWrite, "FILE_GATEWAY", current, values); err != nil {
		return err
	}
	if err := validatePortalLocalInvitationHMAC(values); err != nil {
		return err
	}
	requirement, err := portalLocalCredentialRequirement(application.ServiceCredentialFileGatewayWrite)
	if err != nil {
		return err
	}
	values["FILE_GATEWAY_SCOPE"] = strings.Join(application.SubsystemUpdateServiceCredentialScopes(requirement), " ")
	values["FILE_GATEWAY_BASE_URL"] = "http://file-gateway:8086"
	values["FILE_GATEWAY_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values["PORTAL_LOCAL_PLATFORM_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values["PORTAL_FILE_GATEWAY_APPLICATION_ID"] = input.ApplicationID
	values["PORTAL_REPORT_SOURCE_APPLICATION_ID"] = input.ApplicationID
	values["PORTAL_CUSTOMER_DIRECTORY_MODE"] = "local"
	return nil
}

// validatePortalLocalInvitationHMAC 验证本地邀请密钥有效且不复用其他门户用途。
// 参数：values为本次已生成或保留的完整密钥材料，不记录或返回密钥。
// 返回值：错误表示缺失、编码无效或复用，现有有效密钥不轮换。
func validatePortalLocalInvitationHMAC(values map[string]string) error {
	key, err := base64.StdEncoding.DecodeString(values["PORTAL_LOCAL_INVITE_HMAC_KEY_BASE64"])
	if err != nil || len(key) < 32 {
		return provisioningError("local Portal invitation HMAC is unavailable")
	}
	for _, name := range []string{"PORTAL_ENCRYPTION_KEY_BASE64", "PORTAL_REPORT_CONTENT_KEY_BASE64", "PORTAL_REPORT_INGEST_DESCRIPTOR_KEY_BASE64", "PORTAL_HMAC_KEY_BASE64"} {
		other, err := base64.StdEncoding.DecodeString(values[name])
		if err != nil || len(other) < 32 || bytes.Equal(key, other) {
			return provisioningError("local Portal invitation HMAC must be independent")
		}
	}
	return nil
}
