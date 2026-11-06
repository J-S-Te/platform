package application

import "slices"

// SubsystemRelatedServicePolicy 仅允许在精确结算环境中放宽缺失合同依赖。
type SubsystemRelatedServicePolicy struct {
	SettlementContractOptionalEnvironment string
}

// ValidateRelatedServiceCapabilityProjection 校验Agent模式投影不能跨应用、跨环境或重复声明。
// 参数：caps为特权Agent的只读可用性响应，不接受浏览器提供的运行配置。
// 返回值：投影有效时返回nil；无效模式、重复或非结算目标返回失败关闭错误。
func ValidateRelatedServiceCapabilityProjection(caps SubsystemProvisioningCapabilities) error {
	environments := caps.SettlementContractOptionalEnvironments
	if len(environments) == 0 {
		return nil
	}
	if !caps.Enabled || (caps.Mode != "local" && caps.Mode != "production") ||
		!slices.Contains(caps.SupportedApplicationCodes, "settlement") || len(environments) > 4 {
		return ErrRelatedServiceClientUnavailable
	}
	seen := map[string]bool{}
	for _, environment := range environments {
		if seen[environment] || !slices.Contains([]string{"dev", "test", "staging", "prod"}, environment) ||
			!slices.Contains(caps.SupportedEnvironments, environment) {
			return ErrRelatedServiceClientUnavailable
		}
		seen[environment] = true
		if caps.Mode == "production" {
			found := 0
			for _, target := range caps.Targets {
				if target.ApplicationCode == "settlement" && target.Environment == environment {
					found++
				}
			}
			if found != 1 {
				return ErrRelatedServiceClientUnavailable
			}
		}
	}
	return nil
}

// RelatedServicePolicyFromCapabilities 从Agent的精确目标投影构造依赖策略。
// 参数：caps为真实Agent响应，app/environment为控制面已校验的接收应用环境。
// 返回值：只对精确结算环境返回可选策略；其他目标保持严格，坏投影返回错误。
func RelatedServicePolicyFromCapabilities(caps SubsystemProvisioningCapabilities, app, environment string) (SubsystemRelatedServicePolicy, error) {
	if err := ValidateRelatedServiceCapabilityProjection(caps); err != nil {
		return SubsystemRelatedServicePolicy{}, err
	}
	policy := SubsystemRelatedServicePolicy{}
	if app == "settlement" && slices.Contains(caps.SettlementContractOptionalEnvironments, environment) {
		policy.SettlementContractOptionalEnvironment = environment
	}
	return policy, nil
}
