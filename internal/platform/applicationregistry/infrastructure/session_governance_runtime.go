package infrastructure

import "github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"

// managedSessionEnvironment 为已接入的业务入口生成平台统一开关和公开桥接地址。
// 参数：policy为签名部署输入中的平台策略，code为受控应用代码。
// 返回值：需要下发的非秘密配置；旧请求保留原值，非法策略拒绝写入。
func managedSessionEnvironment(policy *application.CentralSessionRuntimePolicy, code string) (map[string]string, error) {
	values := map[string]string{}
	if policy == nil {
		return values, nil
	}
	switch code {
	case integratedContractApplicationCode, integratedProjectApplicationCode, integratedSettlementApplicationCode,
		integratedCustomerApplicationCode, integratedPortalApplicationCode, "data_analysis":
	default:
		return values, nil
	}
	validated, err := application.NewCentralSessionRuntimePolicy(policy.Enabled, policy.PublicBaseURL)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if code == integratedPortalApplicationCode {
		prefix = "PORTAL_"
	}
	values[prefix+"CENTRAL_SESSION_ENABLED"] = booleanEnvironmentValue(validated.Enabled)
	values[prefix+"CENTRAL_SESSION_PUBLIC_BASE_URL"] = validated.PublicBaseURL
	return values, nil
}
