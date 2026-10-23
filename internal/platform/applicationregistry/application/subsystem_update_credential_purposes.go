package application

import "strings"

// SubsystemUpdateP1Purposes 是需要严格校验（租户/类型/认证方式/授权类型）的用途集合。
// 与既有实现一致：这些用途由平台侧集成使用，凭据漂移会直接导致集成失败。
var SubsystemUpdateP1Purposes = []string{
	ServiceCredentialSettlementContractIngest,
	ServiceCredentialPortalReportFileRead,
	ServiceCredentialPortalReportDownloadIdentityRead,
}

// RequiresStrictSubsystemUpdateServiceClientValidation 判断该用途是否需要严格校验既有服务客户端。
// 既有实现的条件：属于 p1 用途，或（合同管理 + 通知摄取特例）。
func RequiresStrictSubsystemUpdateServiceClientValidation(applicationCode, purpose string) bool {
	normalizedPurpose := strings.TrimSpace(purpose)
	for _, candidate := range SubsystemUpdateP1Purposes {
		if normalizedPurpose == candidate {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(applicationCode), "contract_management") &&
		normalizedPurpose == ServiceCredentialNotificationIngest
}
