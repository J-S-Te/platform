package application

import (
	"fmt"
	"strings"
)

// SubsystemUpdateProvisioningAssembly 是"受控更新"装配部署输入所需的参数（非敏感部分 + 由调用方解析的凭据）。
// 与首次接入的装配分开：更新路径允许"仅重建"（网关字段全空），也允许携带切换/回滚语义。
type SubsystemUpdateProvisioningAssembly struct {
	TenantID        string
	ApplicationID   string
	EnvironmentID   string
	ApplicationCode string
	Environment     string
	// ManifestChecksum 来自服务端清单目标；生产模式下为空必须由调用方先行拒绝。
	ManifestChecksum       string
	Issuer                 string
	AuthenticationProvider string
	// AuthenticationRuntimeUpdate 表示本次更新同时更新认证运行时（切换/回滚或凭据刷新）。
	AuthenticationRuntimeUpdate bool
	// 网关字段：三者要么都为空（仅重建），要么都提供（派生浏览器 URL）。
	PublicBaseURL string
	UpstreamURL   string
	PathPrefix    string
	// 凭据与关联信息由调用方在受控步骤中解析后传入（明文不进日志）。
	ClientID                     string
	ClientSecret                 string
	CatalogPublisherClientID     string
	CatalogPublisherClientSecret string
	ServiceCredentials           []SubsystemServiceCredential
	RelatedServiceClients        []RelatedSubsystemServiceClient
	InitialRuntimeProvisioning   bool
}

// BuildSubsystemUpdateProvisioningInput 用途：保留登记身份并装配受控更新的部署输入。
// 参数：assembly 为服务端解析的身份、凭据及完整或全空的网关字段。
// 返回值：返回派生 URL 和完整身份；部分网关字段无效时返回校验错误。
func BuildSubsystemUpdateProvisioningInput(assembly SubsystemUpdateProvisioningAssembly) (SubsystemProvisioningInput, error) {
	publicBaseURL := strings.TrimRight(strings.TrimSpace(assembly.PublicBaseURL), "/")
	upstreamURL := strings.TrimRight(strings.TrimSpace(assembly.UpstreamURL), "/")
	pathPrefix := strings.TrimRight(strings.TrimSpace(assembly.PathPrefix), "/")
	input := SubsystemProvisioningInput{
		TenantID:                     strings.TrimSpace(assembly.TenantID),
		ApplicationID:                strings.TrimSpace(assembly.ApplicationID),
		EnvironmentID:                strings.TrimSpace(assembly.EnvironmentID),
		ApplicationCode:              strings.TrimSpace(assembly.ApplicationCode),
		Environment:                  strings.TrimSpace(assembly.Environment),
		ManifestChecksum:             strings.TrimSpace(assembly.ManifestChecksum),
		Issuer:                       strings.TrimSpace(assembly.Issuer),
		AuthenticationProvider:       strings.TrimSpace(assembly.AuthenticationProvider),
		AuthenticationRuntimeUpdate:  assembly.AuthenticationRuntimeUpdate,
		ClientID:                     assembly.ClientID,
		ClientSecret:                 assembly.ClientSecret,
		CatalogPublisherClientID:     strings.TrimSpace(assembly.CatalogPublisherClientID),
		CatalogPublisherClientSecret: assembly.CatalogPublisherClientSecret,
		ServiceCredentials:           assembly.ServiceCredentials,
		RelatedServiceClients:        assembly.RelatedServiceClients,
		InitialRuntimeProvisioning:   assembly.InitialRuntimeProvisioning,
	}
	if publicBaseURL == "" && upstreamURL == "" && pathPrefix == "" {
		// 仅重建：保留既有运行时配置，不下发浏览器 URL。
		return input, nil
	}
	if publicBaseURL == "" || upstreamURL == "" || pathPrefix == "" {
		return SubsystemProvisioningInput{}, fmt.Errorf("gateway fields must be provided together: %w", ErrValidation)
	}
	input.PublicURL = publicBaseURL + pathPrefix + "/"
	input.RedirectURI = publicBaseURL + pathPrefix + "/auth/callback"
	input.PathPrefix = pathPrefix
	input.UpstreamURL = upstreamURL
	return input, nil
}
