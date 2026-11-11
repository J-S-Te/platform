package infrastructure

import (
	"github.com/J-S-Te/Basic-Platform/internal/platform/applicationregistry/application"
)

const settlementDashboardRelatedPrefix = "related_service.data_analysis.settlement_dashboard_read."

// settlementDashboardSource 核验财务接收端的独立数析来源身份。
// 参数：input为接收应用环境和平台登记的公开来源元数据。
// 返回值：完整来源；未安装返回nil，重复或任一身份漂移返回错误。
func settlementDashboardSource(input application.SubsystemProvisioningInput) (*application.RelatedSubsystemServiceClient, error) {
	if input.ApplicationCode != "settlement" {
		return nil, provisioningError("settlement dashboard receiver mismatch")
	}
	var found *application.RelatedSubsystemServiceClient
	for index := range input.RelatedServiceClients {
		source := &input.RelatedServiceClients[index]
		if source.ApplicationCode != "data_analysis" || source.Purpose != application.ServiceCredentialSettlementDashboardRead {
			continue
		}
		if found != nil {
			return nil, provisioningError("settlement dashboard source is ambiguous")
		}
		found = source
	}
	if found == nil {
		return nil, nil
	}
	if found.ApplicationID == "" || found.EnvironmentID == "" || found.Environment != input.Environment ||
		found.Client.ClientID != "data_analysis-"+input.Environment+"-settlement-dashboard" {
		return nil, provisioningError("settlement dashboard source environment mismatch")
	}
	sourceInput := input
	sourceInput.ApplicationCode = "data_analysis"
	sourceInput.ApplicationID, sourceInput.EnvironmentID = found.ApplicationID, found.EnvironmentID
	if err := validateP1ServiceCredential(sourceInput, application.ServiceCredentialSettlementDashboardRead, found.Client); err != nil {
		return nil, err
	}
	return found, nil
}

// resolveSettlementDashboardBinding 提供财务入口启用状态与公开信任标识。
// 参数：input为接收环境，source为严格白名单的绑定字段。
// 返回值：已验证公开值；来源未就绪返回禁用与原因，秘密或非法字段失败。
func resolveSettlementDashboardBinding(input application.SubsystemProvisioningInput, source string) (string, error) {
	found, err := settlementDashboardSource(input)
	if err != nil {
		return "", err
	}
	switch source {
	case "settlement_dashboard_enabled":
		return booleanEnvironmentValue(found != nil), nil
	case "settlement_dashboard_unavailable_reason":
		if found == nil {
			return "data_analysis settlement dashboard identity is not registered; controlled source onboarding/update is required", nil
		}
		return "", nil
	case settlementDashboardRelatedPrefix + "client_id":
		if found != nil {
			return found.Client.ClientID, nil
		}
		return "", nil
	case settlementDashboardRelatedPrefix + "oauth_client_id":
		if found != nil {
			return found.Client.ID, nil
		}
		return "", nil
	default:
		return "", provisioningError("unsupported settlement dashboard public identity field")
	}
}

// settlementDashboardRuntimeValues 为本地结算配置独立财务验签入口。
// 参数：input为接收环境，values为本次受控发布配置。
// 返回值：无错误表示可安全发布；身份漂移失败关闭。
func settlementDashboardRuntimeValues(input application.SubsystemProvisioningInput, values map[string]string) error {
	for key, source := range map[string]string{
		"ENABLED": "settlement_dashboard_enabled", "UNAVAILABLE_REASON": "settlement_dashboard_unavailable_reason",
		"CLIENT_ID": settlementDashboardRelatedPrefix + "client_id", "OAUTH_CLIENT_ID": settlementDashboardRelatedPrefix + "oauth_client_id",
	} {
		value, err := resolveSettlementDashboardBinding(input, source)
		if err != nil {
			return err
		}
		values["SETTLEMENT_DASHBOARD_MACHINE_"+key] = value
	}
	for key, value := range map[string]string{"AUDIENCE": "basic-platform-application", "ISSUER": "basic-platform", "PUBLIC_KEY_PATH": "/app/data/keys/jwt-ed25519-public.pem", "CALLER_APPLICATION_CODE": "data_analysis", "CALLER_ENVIRONMENT_CODE": input.Environment, "REQUIRED_SCOPE": "dashboard.settlement.read"} {
		values["SETTLEMENT_DASHBOARD_MACHINE_"+key] = value
	}
	return nil
}

// dataAnalysisSettlementRuntimeValues 交付数析独立财务读取凭据并保护只写密钥继承。
// 参数：input为数析登记身份，current为已有临时配置，values为本次受控更新。
// 返回值：独立身份完整时成功；身份漂移或缺少可安全继承密钥时要求重试。
func dataAnalysisSettlementRuntimeValues(input application.SubsystemProvisioningInput, current, values map[string]string) error {
	credential, ok := input.RegisteredServiceCredential(application.ServiceCredentialSettlementDashboardRead)
	if !ok || credential.OAuthClient.ClientID != "data_analysis-"+input.Environment+"-settlement-dashboard" {
		return provisioningError("data analysis settlement dashboard credential is unavailable or mismatched")
	}
	if err := p1CredentialValues(input, application.ServiceCredentialSettlementDashboardRead, "SETTLEMENT_MACHINE_CLIENT_ID", "SETTLEMENT_MACHINE_CLIENT_SECRET", current, values); err != nil {
		return err
	}
	for _, prefix := range []string{"CONTRACT", "PROJECT"} {
		if credential.OAuthClient.ClientID == values[prefix+"_MACHINE_CLIENT_ID"] ||
			(credential.PlaintextSecret != "" && credential.PlaintextSecret == values[prefix+"_MACHINE_CLIENT_SECRET"]) {
			return provisioningError("data analysis settlement credential must be distinct")
		}
	}
	values["SETTLEMENT_MACHINE_TOKEN_SCOPE"] = "dashboard.settlement.read"
	values["SETTLEMENT_FINANCE_URL"] = "http://settlement-api:8085/internal/v1/dashboard/finance"
	values["PROJECT_REPORT_URL"] = "http://project-api:8082/internal/v1/dashboard/reports"
	values["METRIC_FACTS_SYNC_ENABLED"] = "true"
	preserveDataAnalysisDashboardSources(input.ApplicationCode, current, values)
	return nil
}

// dataAnalysisDashboardRuntimeValues 同步本地首次接入与更新的独立看板机器凭据。
// 参数：input为登记身份，current为已有临时配置，values为本次发布配置。
// 返回值：用途完整时成功，缺少凭据或环境不匹配时拒绝。
func dataAnalysisDashboardRuntimeValues(input application.SubsystemProvisioningInput, current, values map[string]string) error {
	for _, binding := range []struct{ purpose, prefix, suffix, scope string }{
		{application.ServiceCredentialContractDashboardRead, "CONTRACT", "contract-dashboard", "dashboard.contract.read"},
		{application.ServiceCredentialProjectDashboardRead, "PROJECT", "project-dashboard", "dashboard.project.read"},
	} {
		credential, ok := input.ServiceCredential(binding.purpose)
		if !ok || credential.OAuthClient.ClientID != "data_analysis-"+input.Environment+"-"+binding.suffix {
			return provisioningError("data analysis dashboard credential is unavailable or mismatched")
		}
		values[binding.prefix+"_MACHINE_CLIENT_ID"] = credential.OAuthClient.ClientID
		values[binding.prefix+"_MACHINE_CLIENT_SECRET"] = credential.PlaintextSecret
		values[binding.prefix+"_MACHINE_TOKEN_SCOPE"] = binding.scope
	}
	values["MACHINE_TOKEN_URL"] = "http://platform-api:8080/oauth2/token"
	values["MACHINE_TOKEN_ISSUER"] = "basic-platform"
	values["MACHINE_TOKEN_AUDIENCE"] = "basic-platform-application"
	return dataAnalysisSettlementRuntimeValues(input, current, values)
}

// preserveDataAnalysisDashboardSources 保留运维明确选择的来源URL和新指标启停。
// 参数：code为接收应用，current为受控现值，values为本次默认配置。
// 返回值：无；显式空URL也表示未接入，不能被更新静默启用。
func preserveDataAnalysisDashboardSources(code string, current, values map[string]string) {
	if code != "data_analysis" {
		return
	}
	for _, key := range []string{"PROJECT_REPORT_URL", "SETTLEMENT_FINANCE_URL", "METRIC_FACTS_SYNC_ENABLED"} {
		if value, exists := current[key]; exists {
			values[key] = value
		}
	}
}

// settlementDashboardRuntimeEmptyBindings 限定结算财务入口在明确启停状态下允许的空绑定。
// 参数：code为接收应用，bindings为审核清单映射，values为待校验的运行配置。
// 返回值：可为空的固定键集合；用途映射、启停状态或禁用原因非法时返回错误。
func settlementDashboardRuntimeEmptyBindings(code string, bindings, values map[string]string) (map[string]bool, error) {
	allowed := map[string]bool{}
	if code != "settlement" {
		return allowed, nil
	}
	const prefix = "SETTLEMENT_DASHBOARD_MACHINE_"
	expected := map[string]string{
		prefix + "ENABLED":            "settlement_dashboard_enabled",
		prefix + "UNAVAILABLE_REASON": "settlement_dashboard_unavailable_reason",
		prefix + "CLIENT_ID":          settlementDashboardRelatedPrefix + "client_id",
		prefix + "OAUTH_CLIENT_ID":    settlementDashboardRelatedPrefix + "oauth_client_id",
	}
	declared := false
	for key := range expected {
		if _, exists := bindings[key]; exists {
			declared = true
		}
	}
	if !declared {
		return allowed, nil
	}
	for key, source := range expected {
		if bindings[key] != source {
			return nil, provisioningError("settlement dashboard runtime binding mismatch")
		}
	}
	switch values[prefix+"ENABLED"] {
	case "true":
		allowed[prefix+"UNAVAILABLE_REASON"] = true
	case "false":
		reason := values[prefix+"UNAVAILABLE_REASON"]
		if productionEnvironmentValueMissing(reason) || !validEnvironmentValue(reason) {
			return nil, provisioningError("disabled settlement dashboard requires a valid unavailable reason")
		}
		allowed[prefix+"CLIENT_ID"], allowed[prefix+"OAUTH_CLIENT_ID"] = true, true
	default:
		return nil, provisioningError("settlement dashboard enabled state is invalid")
	}
	return allowed, nil
}
