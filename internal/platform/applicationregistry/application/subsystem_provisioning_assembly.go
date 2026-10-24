package application

import (
	"fmt"
	"strings"
)

// SubsystemOnboardingProvisioningAssembly 是"首次接入"装配部署输入所需的参数。
// 其中 ClientSecret / CatalogPublisherClientSecret 由服务层解析后经内存传递：
// 本模块只做装配，**不写库、不打日志、不返回给浏览器**。
type SubsystemOnboardingProvisioningAssembly struct {
	TenantID        string
	ApplicationID   string
	EnvironmentID   string
	ApplicationCode string
	Environment     string
	// Issuer 与 AuthenticationProvider 来自服务端配置与环境绑定，不从浏览器提示推断。
	Issuer                       string
	AuthenticationProvider       string
	ManifestChecksum             string
	ClientID                     string
	ClientSecret                 string
	CatalogPublisherClientID     string
	CatalogPublisherClientSecret string
	ServiceCredentials           []SubsystemServiceCredential
	RelatedServiceClients        []RelatedSubsystemServiceClient
	RedirectURI                  string
	PublicURL                    string
	PathPrefix                   string
	UpstreamURL                  string
}

// BuildSubsystemOnboardingProvisioningInput 装配首次接入的部署输入。
// 校验只针对必需字段：缺失即失败（不猜测、不用空值继续部署）。
func BuildSubsystemOnboardingProvisioningInput(assembly SubsystemOnboardingProvisioningAssembly) (SubsystemProvisioningInput, error) {
	// 只校验部署协议真正依赖的定位字段（租户 + 应用编码 + 环境）：
	// 应用/环境 ID 与凭据在部分既有流程中允许为空（由服务层或 Agent 侧补齐），
	// 这里要求非空会改变既有行为。
	required := map[string]string{
		"tenant_id":        assembly.TenantID,
		"application_code": assembly.ApplicationCode,
		"environment":      assembly.Environment,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return SubsystemProvisioningInput{}, fmt.Errorf("subsystem onboarding provisioning requires %s: %w", field, ErrValidation)
		}
	}
	return SubsystemProvisioningInput{
		TenantID:                     strings.TrimSpace(assembly.TenantID),
		ApplicationID:                strings.TrimSpace(assembly.ApplicationID),
		EnvironmentID:                strings.TrimSpace(assembly.EnvironmentID),
		ApplicationCode:              strings.TrimSpace(assembly.ApplicationCode),
		Environment:                  strings.TrimSpace(assembly.Environment),
		Issuer:                       strings.TrimSpace(assembly.Issuer),
		AuthenticationProvider:       strings.TrimSpace(assembly.AuthenticationProvider),
		ManifestChecksum:             strings.TrimSpace(assembly.ManifestChecksum),
		ClientID:                     assembly.ClientID,
		ClientSecret:                 assembly.ClientSecret,
		CatalogPublisherClientID:     strings.TrimSpace(assembly.CatalogPublisherClientID),
		CatalogPublisherClientSecret: assembly.CatalogPublisherClientSecret,
		ServiceCredentials:           assembly.ServiceCredentials,
		RelatedServiceClients:        assembly.RelatedServiceClients,
		RedirectURI:                  strings.TrimSpace(assembly.RedirectURI),
		PublicURL:                    strings.TrimSpace(assembly.PublicURL),
		PathPrefix:                   strings.TrimSpace(assembly.PathPrefix),
		UpstreamURL:                  strings.TrimSpace(assembly.UpstreamURL),
	}, nil
}
