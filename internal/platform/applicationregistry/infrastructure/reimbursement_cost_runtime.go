package infrastructure

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const reimbursementCostRelatedPrefix = "related_service.settlement.settlement_reimbursement_cost_read."

// validateReimbursementCostIsolation 拒绝独立候选用途复用其他用途身份或待交付秘密。
// 参数：input为完整受控登记输入，client为当前候选用途身份。
// 返回值：用途独立时nil；重复登记或跨用途复用时错误。
func validateReimbursementCostIsolation(input application.SubsystemProvisioningInput, client application.OAuthClientView) error {
	secret, count := "", 0
	for _, credential := range input.ServiceCredentials {
		if credential.Purpose == application.ServiceCredentialSettlementReimbursementCostRead {
			secret, count = credential.PlaintextSecret, count+1
		}
	}
	if count > 1 {
		return provisioningError("reimbursement cost credential is ambiguous")
	}
	for _, credential := range input.ServiceCredentials {
		if credential.Purpose == application.ServiceCredentialSettlementReimbursementCostRead {
			continue
		}
		if credential.OAuthClient.ID == client.ID || credential.OAuthClient.ClientID == client.ClientID ||
			secret != "" && credential.PlaintextSecret == secret {
			return provisioningError("reimbursement cost credential must be distinct")
		}
	}
	return nil
}

// reimbursementCostSource 核验报销候选入口的唯一结算来源身份。
// 参数：input为接收端及平台查询的公开机器身份。
// 返回值：完整来源或未安装时的nil；任一归属漂移返回错误。
func reimbursementCostSource(input application.SubsystemProvisioningInput) (*application.RelatedSubsystemServiceClient, error) {
	if input.ApplicationCode != "reimbursement" {
		return nil, provisioningError("reimbursement cost receiver mismatch")
	}
	var found *application.RelatedSubsystemServiceClient
	for index := range input.RelatedServiceClients {
		source := &input.RelatedServiceClients[index]
		if source.ApplicationCode != "settlement" || source.Purpose != application.ServiceCredentialSettlementReimbursementCostRead {
			continue
		}
		if found != nil {
			return nil, provisioningError("reimbursement cost source is ambiguous")
		}
		found = source
	}
	if found == nil {
		return nil, nil
	}
	if found.Environment != input.Environment || found.ApplicationID == "" || found.EnvironmentID == "" {
		return nil, provisioningError("reimbursement cost source environment mismatch")
	}
	sourceInput := input
	sourceInput.ApplicationCode = "settlement"
	sourceInput.ApplicationID, sourceInput.EnvironmentID = found.ApplicationID, found.EnvironmentID
	if err := validateP1ServiceCredential(sourceInput, found.Purpose, found.Client); err != nil {
		return nil, err
	}
	return found, nil
}

// resolveReimbursementCostBinding 提供只读候选入口的开关及公开信任字段。
// 参数：input为报销受控接入，source为精确白名单绑定。
// 返回值：公开值；来源未安装时关闭并清空旧身份，非法字段返回错误。
func resolveReimbursementCostBinding(input application.SubsystemProvisioningInput, source string) (string, error) {
	found, err := reimbursementCostSource(input)
	if err != nil {
		return "", err
	}
	if source == "reimbursement_cost_enabled" {
		return booleanEnvironmentValue(found != nil), nil
	}
	fields := map[string]string{"client_id": "", "oauth_client_id": "", "application_id": "", "environment_id": ""}
	if found != nil {
		fields["client_id"], fields["oauth_client_id"] = found.Client.ClientID, found.Client.ID
		fields["application_id"], fields["environment_id"] = found.ApplicationID, found.EnvironmentID
	}
	for key, value := range fields {
		if source == reimbursementCostRelatedPrefix+key {
			return value, nil
		}
	}
	return "", provisioningError("unsupported reimbursement cost public identity field")
}

// reimbursementCostBindings 返回报销候选接收端固定绑定清单。
// 参数：无。
// 返回值：开关和四个公开身份字段，不含来源秘密。
func reimbursementCostBindings() map[string]string {
	const prefix = "REIMBURSEMENT_COST_MACHINE_"
	return map[string]string{
		prefix + "ENABLED":         "reimbursement_cost_enabled",
		prefix + "CLIENT_ID":       reimbursementCostRelatedPrefix + "client_id",
		prefix + "OAUTH_CLIENT_ID": reimbursementCostRelatedPrefix + "oauth_client_id",
		prefix + "APPLICATION_ID":  reimbursementCostRelatedPrefix + "application_id",
		prefix + "ENVIRONMENT_ID":  reimbursementCostRelatedPrefix + "environment_id",
	}
}

// reimbursementCostRuntimeValues 下发本地报销候选入口的精确信任边界。
// 参数：input为报销登记环境，values接收本次运行配置。
// 返回值：来源公开身份完整或明确关闭时成功，漂移失败。
func reimbursementCostRuntimeValues(input application.SubsystemProvisioningInput, values map[string]string) error {
	for key, source := range reimbursementCostBindings() {
		value, err := resolveReimbursementCostBinding(input, source)
		if err != nil {
			return err
		}
		values[key] = value
	}
	for key, value := range map[string]string{
		"AUDIENCE": "basic-platform-application", "ISSUER": "basic-platform",
		"PUBLIC_KEY_PATH":         "/app/data/keys/jwt-ed25519-public.pem",
		"CALLER_APPLICATION_CODE": "settlement", "CALLER_ENVIRONMENT_CODE": input.Environment,
		"TENANT_ID": input.TenantID, "REQUIRED_SCOPE": "reimbursement.cost_candidate.read",
	} {
		values["REIMBURSEMENT_COST_MACHINE_"+key] = value
	}
	return nil
}

// reimbursementCostRuntimeEmptyBindings 仅在明确禁用时允许固定来源身份为空。
// 参数：code为接收应用，bindings为审核清单，values为准备校验的配置。
// 返回值：可空键集合；缺项、映射漂移或非法开关返回错误。
func reimbursementCostRuntimeEmptyBindings(code string, bindings, values map[string]string) (map[string]bool, error) {
	allowed := map[string]bool{}
	if code != "reimbursement" {
		return allowed, nil
	}
	expected := reimbursementCostBindings()
	declared := false
	for key := range expected {
		_, exists := bindings[key]
		declared = declared || exists
	}
	if !declared {
		return allowed, nil
	}
	for key, source := range expected {
		if bindings[key] != source {
			return nil, provisioningError("reimbursement cost runtime binding mismatch")
		}
	}
	switch values["REIMBURSEMENT_COST_MACHINE_ENABLED"] {
	case "false":
		for key := range expected {
			if key != "REIMBURSEMENT_COST_MACHINE_ENABLED" {
				allowed[key] = true
			}
		}
	case "true":
	default:
		return nil, provisioningError("reimbursement cost enabled state is invalid")
	}
	return allowed, nil
}

// settlementReimbursementCostRuntimeValues 交付结算的独立候选读取凭据。
// 参数：input为结算登记环境，current为受控现值，values接收本次原子更新。
// 返回值：完整身份及独占secret可交付时成功，否则要求受控重试。
func settlementReimbursementCostRuntimeValues(input application.SubsystemProvisioningInput, current, values map[string]string) error {
	const prefix = "SETTLEMENT_REIMBURSEMENT_COST_"
	if err := p1CredentialValues(input, application.ServiceCredentialSettlementReimbursementCostRead,
		prefix+"CLIENT_ID", prefix+"CLIENT_SECRET", current, values); err != nil {
		return err
	}
	values[prefix+"ENABLED"] = "true"
	values[prefix+"URL"] = "http://reimbursement-api:8087/internal/v1/settlement/cost-candidates"
	values[prefix+"TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values[prefix+"SCOPE"] = "reimbursement.cost_candidate.read"
	values[prefix+"TENANT_ID"] = input.TenantID
	values[prefix+"ENVIRONMENT"] = input.Environment
	if selected, exists := current[prefix+"ENABLED"]; exists {
		values[prefix+"ENABLED"] = selected
	}
	return nil
}

// preserveReimbursementCostSelection 保留结算运维明确设置的候选读取开关。
// 参数：code为目标应用，current为受控现值，values为本次更新集合。
// 返回值：无；不因普通刷新静默重新开启已关闭的数据来源。
func preserveReimbursementCostSelection(code string, current, values map[string]string) {
	if code != "settlement" {
		return
	}
	const key = "SETTLEMENT_REIMBURSEMENT_COST_ENABLED"
	if selected, exists := current[key]; exists {
		values[key] = selected
	}
}
