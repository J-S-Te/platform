package application

import "strings"

// SubsystemUpdateP1Purposes 是需要严格校验（租户/类型/认证方式/授权类型）的用途集合。
// 与既有实现一致：这些用途由平台侧集成使用，凭据漂移会直接导致集成失败。
var SubsystemUpdateP1Purposes = []string{
	ServiceCredentialSettlementProjectReferenceRead,
	ServiceCredentialSettlementContractIngest,
	ServiceCredentialPortalReportFileRead,
	ServiceCredentialPortalReportDownloadIdentityRead,
}

// RequiresStrictSubsystemUpdateServiceClientValidation 判断机器用途是否要求严格归属校验。
// 参数：applicationCode 为接入应用，purpose 为凭据用途。
// 返回值：是否核验租户、应用环境、类型、认证方式及授权类型。
func RequiresStrictSubsystemUpdateServiceClientValidation(applicationCode, purpose string) bool {
	normalizedPurpose := strings.TrimSpace(purpose)
	for _, candidate := range SubsystemUpdateP1Purposes {
		if normalizedPurpose == candidate {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(applicationCode)) {
	case "settlement", "settlement_and_invoicing":
		return normalizedPurpose == ServiceCredentialOwnerDirectoryRead ||
			normalizedPurpose == ServiceCredentialNotificationIngest ||
			normalizedPurpose == ServiceCredentialAuditIngest
	case "contract_management":
		return normalizedPurpose == ServiceCredentialNotificationIngest
	default:
		return false
	}
}
