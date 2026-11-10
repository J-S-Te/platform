package infrastructure

import (
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
